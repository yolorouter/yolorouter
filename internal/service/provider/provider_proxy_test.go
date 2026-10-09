package provider

// The probe-rides-the-proxy integration tests (TC-09): a provider whose
// stored stance is a custom proxy must have its connection tests (the
// test-key / test-all surfaces) travel through that same proxy stub — the
// same resolution, the same channel, real routed traffic will use. These
// drive the REAL HTTPProviderClient over the REAL resolver glue
// (osswire.NewProxyResolvers over a real settings cache), so what is under
// test is the whole join: the stance the service stamps onto the call
// context, the probe transport's resolver seat, and the settings rows.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway/osswire"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/outboundproxy"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// countingForwardProxy mirrors the gateway test's proxy stub: it records the
// target host of every absolute-URI request it carries and forwards it.
type countingForwardProxy struct {
	server *httptest.Server
	mu     sync.Mutex
	hosts  []string
}

func newCountingForwardProxy(t *testing.T) *countingForwardProxy {
	t.Helper()
	p := &countingForwardProxy{}
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

func (p *countingForwardProxy) countFor(host string) int {
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

func (p *countingForwardProxy) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hosts)
}

// probeUpstream is an httptest upstream answering one valid OpenAI chat
// completion (the credential-test shape), recording what reached it.
func probeUpstream(t *testing.T, reached *bool, sawBearer *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		*sawBearer = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// proxiedProviderFixture assembles the production wiring shape: a provider
// service whose probe client is the real HTTPProviderClient with the
// provider-bound resolver over a real settings cache, plus one stored
// provider configured with a custom proxy stance and one key.
type proxiedProviderFixture struct {
	svc        *ProviderService
	upstream   *httptest.Server
	proxy      *countingForwardProxy
	provider   *ProviderView
	upstreamHt string
}

func newProxiedProviderFixture(t *testing.T) *proxiedProviderFixture {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	var reached bool
	var sawBearer string
	upstream := probeUpstream(t, &reached, &sawBearer)
	proxy := newCountingForwardProxy(t)

	proxies := osswire.NewProxyResolvers(systemsettings.NewSystemSettingsService(db))
	svc := NewProviderService(db, testutil.ProviderSecrets(),
		providerclient.NewHTTPProviderClient(false, proxies.Upstream))

	custom := outboundproxy.ModeCustom
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "proxied", BaseURL: upstream.URL, KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuv", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	// The create-time verification ran before any stance existed (global
	// off, unstamped inherit) and its direct loopback dial was refused by
	// the SSRF policy — the key starts failed, which is the state a
	// test-key button is pressed on.
	// Attach the provider's proxy stance now — the edit the admin UI makes.
	if _, err := svc.UpdateProvider(context.Background(), provider.ID, UpdateProviderInput{
		Name: provider.Name, BaseURL: provider.BaseURL,
		ProxyMode: &custom, ProxyURL: &proxy.server.URL,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("UpdateProvider(proxy stance) failed: %v", err)
	}
	f := &proxiedProviderFixture{svc: svc, upstream: upstream, proxy: proxy, provider: provider}
	f.upstreamHt = upstreamHost(t, upstream)
	return f
}

func upstreamHost(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	return u.Host
}

func TestTestProviderKeyRidesProviderProxy(t *testing.T) {
	f := newProxiedProviderFixture(t)

	before := f.proxy.total()
	view, err := f.svc.TestProviderKey(context.Background(), f.provider.ID, f.provider.Keys[0].ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("TestProviderKey failed: %v", err)
	}
	if view.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected the proxied probe to pass, got verification status %d (detail on view)", view.VerificationStatus)
	}
	if got := f.proxy.total() - before; got != 1 {
		t.Fatalf("the probe must have traveled the provider's proxy exactly once: %d carries", got)
	}
	if got := f.proxy.countFor(f.upstreamHt); got != 1 {
		t.Fatalf("the proxy carried %d requests for the upstream host, want 1", got)
	}
}

func TestTestAllProviderKeysRidesProviderProxy(t *testing.T) {
	f := newProxiedProviderFixture(t)

	before := f.proxy.total()
	results, err := f.svc.TestAllProviderKeys(context.Background(), f.provider.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 batch result, got %d", len(results))
	}
	if got := f.proxy.total() - before; got != 1 {
		t.Fatalf("the batch probe must have traveled the provider's proxy exactly once: %d carries", got)
	}
}
