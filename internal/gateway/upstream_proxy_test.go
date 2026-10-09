package gateway

// Outbound-proxy wiring tests for the kernel's three outbound surfaces:
// the relay's provider split (integration, through a real HTTP proxy stub),
// and the secondary-fetch client's global-only resolver seat (construction
// assertions). The per-request stance plumbing (WithUpstreamProxyStance /
// UpstreamProxyStanceFrom) is asserted directly here too — it is the road
// the provider identity travels from the dispatch site to the transport's
// Proxy callback.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	ycrypto "github.com/yolorouter/yolorouter/pkg/crypto"

	"github.com/yolorouter/yolorouter/internal/outboundproxy"
	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// countingProxy is a real HTTP forward proxy stub: it records the target
// host of every absolute-URI request it is asked to carry (the form Go's
// transport uses for an http target through a proxy) and forwards it. The
// per-host record is what the split assertions read — "provider A's traffic
// passed through here, provider B's never did".
type countingProxy struct {
	server *httptest.Server
	mu     sync.Mutex
	hosts  []string
}

func newCountingProxy(t *testing.T) *countingProxy {
	t.Helper()
	p := &countingProxy{}
	forward := &http.Client{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hosts = append(p.hosts, r.URL.Host)
		p.mu.Unlock()
		outReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		outReq.Header = r.Header.Clone()
		resp, err := forward.Do(outReq)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(p.server.Close)
	return p
}

// countFor reports how many requests this proxy carried for host.
func (p *countingProxy) countFor(host string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, h := range p.hosts {
		if h == host {
			n++
		}
	}
	return n
}

// total reports how many requests this proxy carried at all.
func (p *countingProxy) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hosts)
}

// hostOf extracts an httptest server's host:port.
func hostOf(srv *httptest.Server) string {
	u, err := url.Parse(srv.URL)
	if err != nil {
		return srv.URL
	}
	return u.Host
}

// newChatUpstream is an httptest upstream answering one valid OpenAI chat
// completion, recording that it was reached and with which bearer.
func newChatUpstream(t *testing.T, reached *bool, sawBearer *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		*sawBearer = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"up","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// proxyResolversOverDB mirrors osswire.NewProxyResolvers: the same join of
// the settings cache and outboundproxy's pure resolution, rebuilt here
// because an in-package gateway test cannot import the deployment glue
// (osswire imports gateway). The glue itself is unit-tested in its own
// package; what these tests add is the kernel's half of the contract —
// stance stamping, seat separation, and the transport the kernel builds.
func proxyResolversOverDB(t *testing.T, db *gorm.DB) ProxyResolvers {
	t.Helper()
	settingsSvc := systemsettings.NewSystemSettingsService(db)
	resolveGlobal := func(req *http.Request) outboundproxy.Global {
		snap, _, err := settingsSvc.GetOutboundProxy(req.Context())
		if err != nil {
			t.Logf("outbound proxy settings read failed (continuing on snapshot): %v", err)
		}
		return outboundproxy.Global{Mode: snap.Mode, URL: snap.URL}
	}
	return ProxyResolvers{
		Upstream: func(req *http.Request) (*url.URL, error) {
			override := outboundproxy.Override{}
			if stance, ok := UpstreamProxyStanceFrom(req); ok {
				override = outboundproxy.Override{Mode: stance.Mode, URL: stance.URL}
			}
			return outboundproxy.Resolve(req.URL, override, resolveGlobal(req), nil)
		},
		Fetch: func(req *http.Request) (*url.URL, error) {
			return outboundproxy.ResolveGlobal(req.URL, resolveGlobal(req), nil)
		},
	}
}

// setProviderStance writes one provider's proxy stance columns directly —
// the row state the admin UI persists.
func setProviderStance(t *testing.T, db *gorm.DB, providerID uint, mode, rawURL string) {
	t.Helper()
	if err := db.Exec("UPDATE providers SET proxy_mode = ?, proxy_url = ? WHERE id = ?", mode, rawURL, providerID).Error; err != nil {
		t.Fatalf("seed provider stance: %v", err)
	}
}

// TestUpstreamProxyStanceContextRoundTrip pins the context plumbing itself:
// a stance stamped on the context comes back off a request built from it,
// and an unstamped request reports absent (the resolver's inherit path).
func TestUpstreamProxyStanceContextRoundTrip(t *testing.T) {
	ctx := WithUpstreamProxyStance(context.Background(), UpstreamProxyStance{Mode: "custom", URL: "http://p.example:3128"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://up.example/v1/chat", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	got, ok := UpstreamProxyStanceFrom(req)
	if !ok {
		t.Fatal("stamped stance not visible on the request built from the context")
	}
	if got.Mode != "custom" || got.URL != "http://p.example:3128" {
		t.Fatalf("stance round-tripped as %+v", got)
	}

	bare, err := http.NewRequest(http.MethodPost, "https://up.example/v1/chat", nil)
	if err != nil {
		t.Fatalf("build bare request: %v", err)
	}
	if _, ok := UpstreamProxyStanceFrom(bare); ok {
		t.Fatal("unstamped request reported a stance; the resolver's inherit arm depends on this being absent")
	}
}

// TestRelayProviderProxySplit is the integration leg of TC-10: provider A
// carries an explicit custom proxy, provider B inherits the global off —
// A's relay must exit through the proxy stub, B's must go direct, in the
// same process, through the same injected resolver pair. The global
// setting stays off (the seeded default), so B's directness is the inherit
// arm, not a second global configuration.
func TestRelayProviderProxySplit(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var aReached, bReached bool
	var aBearer, bBearer string
	upstreamA := newChatUpstream(t, &aReached, &aBearer)
	upstreamB := newChatUpstream(t, &bReached, &bBearer)
	proxy := newCountingProxy(t)

	pA := createProvider(t, db, "proxied", upstreamA.URL)
	setProviderStance(t, db, pA.ID, "custom", proxy.server.URL)
	pB := createProvider(t, db, "direct", upstreamB.URL) // inherit: proxy_mode stays ''

	masterKey := bytes.Repeat([]byte{0x42}, 32)
	master := ycrypto.NewSecretBox(masterKey)
	// allowPrivate=true: both upstreams are loopback httptest servers, and
	// B's direct leg must actually dial its target (the production-supported
	// way to permit loopback upstreams). A's proxied leg never consults the
	// target policy, so the flag costs that leg nothing.
	svc := NewService(testStoreFrom(db), nil, master, true, stubSettingsProvider{}, testGatewayConfig(), proxyResolversOverDB(t, db))
	svc.videoTasks = newTestVideoTasks(t, db, master, testStoreFrom(db), svc.client)
	videoTasks = svc.videoTasks

	createProviderKey(t, db, master, pA.ID, "sk-upstream-a", "ka", 1, true)
	createProviderKey(t, db, master, pB.ID, "sk-upstream-b", "kb", 1, true)
	mA := createModelAndCandidate(t, db, pA, "model-a", "up-a", true, true, 1)
	mB := createModelAndCandidate(t, db, pB, "model-b", "up-b", true, true, 1)
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{mA.ID, mB.ID})

	relay := func(model string) {
		t.Helper()
		c, w := newCtx([]byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("relay of %s: status = %d, want 200; body = %s", model, w.Code, w.Body.String())
		}
	}

	relay("model-a")
	if !aReached {
		t.Fatal("provider A's upstream was never reached")
	}
	if aBearer != "Bearer sk-upstream-a" {
		t.Fatalf("provider A's upstream saw authorization %q", aBearer)
	}
	if got := proxy.countFor(hostOf(upstreamA)); got < 1 {
		t.Fatalf("provider A (custom proxy) did not exit through the proxy stub: %d carries for its host", got)
	}

	before := proxy.total()
	relay("model-b")
	if !bReached {
		t.Fatal("provider B's upstream was never reached")
	}
	if bBearer != "Bearer sk-upstream-b" {
		t.Fatalf("provider B's upstream saw authorization %q", bBearer)
	}
	if got := proxy.total(); got != before {
		t.Fatalf("provider B (inherit, global off) leaked through the proxy stub: %d new carries", got-before)
	}
}

// TestSecondaryFetchClientResolvesGlobalProxy is TC-13's construction
// assertion: the shared secondary-fetch client is built on the FETCH seat
// of the injected pair — the resolver it consults is the global-only one,
// never the provider-bound upstream seat, and a request through that client
// physically travels the proxy the fetch resolver returned.
func TestSecondaryFetchClientResolvesGlobalProxy(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	proxy := newCountingProxy(t)

	var (
		mu        sync.Mutex
		fetchSaw  []string
		upstreamN int
	)
	fetchSeat := func(req *http.Request) (*url.URL, error) {
		mu.Lock()
		fetchSaw = append(fetchSaw, req.URL.String())
		mu.Unlock()
		return url.Parse(proxy.server.URL)
	}
	upstreamSeat := func(*http.Request) (*url.URL, error) {
		mu.Lock()
		upstreamN++
		mu.Unlock()
		return nil, nil
	}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	svc := NewService(testStoreFrom(db), nil, ycrypto.NewSecretBox(bytes.Repeat([]byte{0x42}, 32)), true, stubSettingsProvider{}, testGatewayConfig(), ProxyResolvers{
		Upstream: upstreamSeat,
		Fetch:    fetchSeat,
	})
	client := svc.secondaryFetchClient()

	resp, err := client.Get(target.URL + "/media")
	if err != nil {
		t.Fatalf("secondary fetch through the fetch resolver's proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("secondary fetch status = %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(fetchSaw) == 0 {
		t.Fatal("the secondary-fetch client never consulted the injected Fetch resolver")
	}
	if !strings.Contains(fetchSaw[0], "/media") {
		t.Fatalf("fetch resolver consulted about %q, want the fetch target", fetchSaw[0])
	}
	if upstreamN != 0 {
		t.Fatalf("the secondary-fetch client consulted the provider-bound upstream seat %d times; the fetch seat must be global-only", upstreamN)
	}
	if got := proxy.countFor(hostOf(target)); got < 1 {
		t.Fatal("the fetch never physically traveled the proxy the global resolver returned")
	}
}

// TestSecondaryFetchClientOffStaysDirect is TC-19: with the fetch resolver
// deciding "no proxy" (the global off decision), the secondary-fetch client
// dials DIRECT under the target policy — the shipped behavior, pinned here
// by the loopback refusal the fetch transport's own SSRF dialer (built with
// allowPrivate=false) has always produced.
func TestSecondaryFetchClientOffStaysDirect(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	proxy := newCountingProxy(t)

	var (
		mu        sync.Mutex
		consulted int
	)
	offFetch := func(*http.Request) (*url.URL, error) {
		mu.Lock()
		consulted++
		mu.Unlock()
		return nil, nil
	}

	svc := NewService(testStoreFrom(db), nil, ycrypto.NewSecretBox(bytes.Repeat([]byte{0x42}, 32)), true, stubSettingsProvider{}, testGatewayConfig(), ProxyResolvers{
		Upstream: func(*http.Request) (*url.URL, error) { return url.Parse(proxy.server.URL) },
		Fetch:    offFetch,
	})
	client := svc.secondaryFetchClient()

	// A loopback target the direct dial must refuse under the target policy
	// (the fetch transport is deliberately built with allowPrivate=false —
	// secondary URLs are third-party-chosen). If the client instead dialed
	// through a proxy, this request would leave for the proxy and come back
	// as a connect failure naming the proxy, not the target's disallowed
	// range.
	_, err := client.Get("http://127.0.0.9:1/media")
	if err == nil {
		t.Fatal("a direct dial to a disallowed range must fail")
	}
	if !strings.Contains(err.Error(), "disallowed range") {
		t.Fatalf("error does not name the target-policy refusal (dial must be direct, not proxied): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if consulted != 1 {
		t.Fatalf("the off decision must flow through the fetch resolver (consulted %d times, want 1)", consulted)
	}
	if got := proxy.total(); got != 0 {
		t.Fatalf("off must mean no proxy carries: %d requests reached the stub", got)
	}
}
