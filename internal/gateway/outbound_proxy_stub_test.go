package gateway

// End-to-end integration tests for the GLOBAL outbound proxy over a real
// dual-protocol forwarding stub: one helper process offers both an HTTP
// forward proxy (absolute-URI carrying plus CONNECT tunneling) and a SOCKS5
// CONNECT proxy, each really forwarding to the dialed target and counting
// what it carried. The relay, the settings service (the same 30s-TTL cache
// the deployment reads), and the resolver join run exactly as wired in
// production — only the proxy itself and the upstream are loopback stubs.
//
// The stub is plain Go on purpose: the suite must stay hermetic for CI
// (bare `go test ./...` runners have no external proxy binaries), and the
// assertion surface — per-target carry counts on a real forwarding proxy —
// is what the acceptance cases ask of the stub.

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	ycrypto "github.com/yolorouter/yolorouter/pkg/crypto"

	"github.com/yolorouter/yolorouter/internal/outboundproxy"
	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// dualProtocolProxyStub is a pair of real forwarding proxies sharing one
// helper: an HTTP proxy listener and a SOCKS5 listener. Both forward to the
// actual target (nothing is canned), and both record every target
// host:port they are asked to carry — the count is the "traffic exited
// through this proxy" assertion surface.
type dualProtocolProxyStub struct {
	httpURL      string // http://127.0.0.1:port of the HTTP proxy listener
	socks5URL    string // socks5://127.0.0.1:port of the SOCKS5 listener
	socks5hURL   string // same listener addressed as socks5h (alias scheme)
	httpCarries  func() []string
	socksCarries func() []string
}

// newDualProtocolProxyStub starts both listeners on fresh loopback ports.
func newDualProtocolProxyStub(t *testing.T) *dualProtocolProxyStub {
	t.Helper()
	s := &dualProtocolProxyStub{}

	// HTTP forward proxy: absolute-URI requests are forwarded (the form
	// net/http uses for an http target through a proxy), CONNECT requests
	// are tunneled. Both record the target before forwarding.
	var mu sync.Mutex
	var httpTargets []string
	forward := &http.Client{Timeout: 10 * time.Second}
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		var target string
		if r.Method == http.MethodConnect {
			target = r.Host
		} else {
			target = r.URL.Host
		}
		httpTargets = append(httpTargets, target)
		mu.Unlock()

		if r.Method == http.MethodConnect {
			s.tunnel(w, r.Host)
			return
		}
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
	t.Cleanup(proxySrv.Close)
	s.httpURL = proxySrv.URL
	s.httpCarries = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), httpTargets...)
	}

	// SOCKS5 CONNECT proxy (RFC 1928, no auth): enough server-side protocol
	// for the standard library's client, forwarding the tunnel to the real
	// target and recording each CONNECT's target address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5 stub listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var socksMu sync.Mutex
	var socksTargets []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			go func(c net.Conn) {
				target, tunnel, ok := s.socksServe(c)
				if !ok {
					return
				}
				socksMu.Lock()
				socksTargets = append(socksTargets, target)
				socksMu.Unlock()
				pipeBoth(c, tunnel)
			}(conn)
		}
	}()
	s.socksCarries = func() []string {
		socksMu.Lock()
		defer socksMu.Unlock()
		return append([]string(nil), socksTargets...)
	}

	addr := ln.Addr().String()
	s.socks5URL = "socks5://" + addr
	s.socks5hURL = "socks5h://" + addr
	return s
}

// tunnel hijacks the client connection for a CONNECT request and pipes it
// to the dialed target.
func (s *dualProtocolProxyStub) tunnel(w http.ResponseWriter, target string) {
	out, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = out.Close()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		_ = out.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = out.Close()
		_ = client.Close()
		return
	}
	pipeBoth(client, out)
}

// socksServe performs one SOCKS5 handshake on conn, returning the requested
// target, the dialed tunnel, and whether the session reached the piping
// phase.
func (s *dualProtocolProxyStub) socksServe(conn net.Conn) (string, net.Conn, bool) {
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()
	// Client greeting: VER NMETHODS METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return "", nil, false
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", nil, false
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil { // NO AUTHENTICATION REQUIRED
		return "", nil, false
	}
	// Request: VER CMD RSV ATYP ADDR PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return "", nil, false
	}
	if req[1] != 0x01 { // only CONNECT is served
		return "", nil, false
	}
	var host string
	switch req[3] {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", nil, false
		}
		host = net.IP(b).String()
	case 0x03: // domain name
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return "", nil, false
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", nil, false
		}
		host = string(b)
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", nil, false
		}
		host = net.IP(b).String()
	default:
		return "", nil, false
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return "", nil, false
	}
	port := int(portBytes[0])<<8 | int(portBytes[1])
	target := net.JoinHostPort(host, strconv.Itoa(port))

	tunnel, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		// Reply: general SOCKS server failure.
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return "", nil, false
	}
	// Reply: succeeded, BND.ADDR 0.0.0.0:0.
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		_ = tunnel.Close()
		return "", nil, false
	}
	conn = nil // ownership moves to the piper; the deferred close must not fire
	return target, tunnel, true
}

// pipeBoth copies bytes in both directions until either side closes.
func pipeBoth(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		_ = a.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		_ = b.Close()
		done <- struct{}{}
	}()
	<-done
	<-done
}

// countFor reports how many carries the HTTP leg recorded for host.
func (s *dualProtocolProxyStub) httpCountFor(host string) int {
	n := 0
	for _, h := range s.httpCarries() {
		if h == host {
			n++
		}
	}
	return n
}

// socksCountFor reports how many CONNECTs the SOCKS5 leg recorded for host.
func (s *dualProtocolProxyStub) socksCountFor(host string) int {
	n := 0
	for _, h := range s.socksCarries() {
		if h == host {
			n++
		}
	}
	return n
}

// httpTotal / socksTotal report each leg's total carry count.
func (s *dualProtocolProxyStub) httpTotal() int  { return len(s.httpCarries()) }
func (s *dualProtocolProxyStub) socksTotal() int { return len(s.socksCarries()) }

// newCountingChatUpstream answers one valid OpenAI chat completion per hit,
// counting hits and recording the last bearer seen.
func newCountingChatUpstream(t *testing.T) (srv *httptest.Server, hits func() int, lastBearer func() string) {
	t.Helper()
	var mu sync.Mutex
	var n int
	var bearer string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		bearer = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"up","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
			mu.Lock()
			defer mu.Unlock()
			return n
		}, func() string {
			mu.Lock()
			defer mu.Unlock()
			return bearer
		}
}

// proxyResolversOverService is the deployment glue join (the same shape
// osswire.NewProxyResolvers assembles) over a settings service the test
// holds on to, so the save path (UpdateOutboundProxy) and the transport's
// read path (GetOutboundProxy's cached snapshot) share one cache — exactly
// the production wiring, where one service instance feeds both.
func proxyResolversOverService(settingsSvc *systemsettings.SystemSettingsService) ProxyResolvers {
	resolveGlobal := func(req *http.Request) outboundproxy.Global {
		// On a read failure the snapshot is still the best-known value —
		// the fail-open contract the production glue keeps.
		snap, _, _ := settingsSvc.GetOutboundProxy(req.Context())
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

// setGlobalCustomProxy saves mode=custom + rawURL through the service's own
// update path (validation, CAS, cache publish) — the admin UI's save, minus
// the HTTP hop.
func setGlobalCustomProxy(t *testing.T, settingsSvc *systemsettings.SystemSettingsService, rawURL string) {
	t.Helper()
	ctx := context.Background()
	_, version, err := settingsSvc.GetOutboundProxyForHandler(ctx)
	if err != nil {
		t.Fatalf("read outbound proxy version: %v", err)
	}
	if _, _, err := settingsSvc.UpdateOutboundProxy(ctx, version, "custom", rawURL); err != nil {
		t.Fatalf("update outbound proxy to %s: %v", rawURL, err)
	}
}

// newGlobalProxyRelayWorld builds the full relay world — provider with NO
// per-provider stance (inherit the global), key, model+candidate, API key —
// and returns a relay func that must always complete with 200. The provider
// intentionally starts with the seeded empty stance: the global setting is the
// only proxy input under test.
func newGlobalProxyRelayWorld(t *testing.T, db *gorm.DB, upstream *httptest.Server, resolvers ProxyResolvers) func() {
	t.Helper()
	p := createProvider(t, db, "global-proxied", upstream.URL) // no stance: inherit

	masterKey := bytes.Repeat([]byte{0x42}, 32)
	master := ycrypto.NewSecretBox(masterKey)
	// allowPrivate=true: the upstream is a loopback httptest server, the
	// production-supported way to permit loopback upstreams; the proxied
	// legs never consult the target policy, so the flag costs them nothing.
	svc := NewService(testStoreFrom(db), nil, master, true, stubSettingsProvider{}, testGatewayConfig(), resolvers)
	svc.videoTasks = newTestVideoTasks(t, db, master, testStoreFrom(db), svc.client)
	videoTasks = svc.videoTasks

	createProviderKey(t, db, master, p.ID, "sk-upstream-g", "kg", 1, true)
	m := createModelAndCandidate(t, db, p, "model-g", "up-g", true, true, 1)
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	return func() {
		t.Helper()
		c, w := newCtx([]byte(`{"model":"model-g","messages":[{"role":"user","content":"hello"}]}`))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("relay: status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	}
}

// TestRelayGlobalCustomProxyEndToEnd drives the GLOBAL custom proxy through
// both stub protocols: before any save (the seeded off default) the relay
// goes direct and the stubs carry nothing; after the admin-style save of
// mode=custom pointing at the stub, the very same relay's upstream request
// arrives THROUGH the stub (socks5 leg and http leg each), with the upstream
// still served a correct request — the forwarding is real, nothing canned.
// The socks5h spelling rides the same listener to pin the alias handling on
// the full path.
func TestRelayGlobalCustomProxyEndToEnd(t *testing.T) {
	cases := []struct {
		name     string
		proxyURL func(s *dualProtocolProxyStub) string
		httpLeg  int // minimum HTTP-leg carries expected for the upstream host
		socksLeg int // minimum SOCKS5-leg connects expected for the upstream host
	}{
		{"socks5", func(s *dualProtocolProxyStub) string { return s.socks5URL }, 0, 1},
		{"socks5h", func(s *dualProtocolProxyStub) string { return s.socks5hURL }, 0, 1},
		{"http", func(s *dualProtocolProxyStub) string { return s.httpURL }, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			stub := newDualProtocolProxyStub(t)
			settingsSvc := systemsettings.NewSystemSettingsService(db)

			upstream, hits, lastBearer := newCountingChatUpstream(t)
			upstreamHost := hostOf(upstream)

			relay := newGlobalProxyRelayWorld(t, db, upstream, proxyResolversOverService(settingsSvc))

			// Baseline: the seeded global off — direct dial, stubs idle.
			relay()
			if hits() != 1 {
				t.Fatalf("baseline relay did not reach the upstream (hits=%d)", hits())
			}
			if stub.httpTotal()+stub.socksTotal() != 0 {
				t.Fatalf("off baseline leaked through a stub: http=%d socks=%d", stub.httpTotal(), stub.socksTotal())
			}

			// Save the global custom proxy (the admin path) and relay again.
			setGlobalCustomProxy(t, settingsSvc, tc.proxyURL(stub))
			relay()

			if hits() != 2 {
				t.Fatalf("proxied relay did not reach the upstream (hits=%d, want 2)", hits())
			}
			if got := lastBearer(); got != "Bearer sk-upstream-g" {
				t.Fatalf("upstream saw authorization %q", got)
			}
			if got := stub.httpCountFor(upstreamHost); got < tc.httpLeg {
				t.Fatalf("http leg carried %d requests for the upstream, want >= %d", got, tc.httpLeg)
			}
			if got := stub.socksCountFor(upstreamHost); got < tc.socksLeg {
				t.Fatalf("socks5 leg carried %d connects for the upstream, want >= %d", got, tc.socksLeg)
			}
		})
	}
}

// TestRelayGlobalProxySwitchWithoutRestart pins the no-restart switch: with
// the relay world left running (same service instances, same transport),
// saving the global proxy from stub A to stub B must move the exit from A
// to B within one settings-cache TTL window (30s), and once B is observed,
// A must receive nothing further. The switch is also cross-protocol
// (A = http leg, B = socks5 leg), so it cannot pass by pool-key luck on a
// single listener.
func TestRelayGlobalProxySwitchWithoutRestart(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	stubA := newDualProtocolProxyStub(t)
	stubB := newDualProtocolProxyStub(t)
	settingsSvc := systemsettings.NewSystemSettingsService(db)

	upstream, hits, _ := newCountingChatUpstream(t)
	upstreamHost := hostOf(upstream)

	relay := newGlobalProxyRelayWorld(t, db, upstream, proxyResolversOverService(settingsSvc))

	// Exit via A first.
	setGlobalCustomProxy(t, settingsSvc, stubA.httpURL)
	relay()
	if got := stubA.httpCountFor(upstreamHost); got < 1 {
		t.Fatalf("relay did not exit through stub A before the switch (carries=%d)", got)
	}
	if stubB.httpTotal()+stubB.socksTotal() != 0 {
		t.Fatal("stub B carried traffic before being configured")
	}

	// Switch A -> B with everything still running, then poll relays until
	// the exit is observed at B. The deadline is the cache TTL bound: the
	// acceptance contract is "visible within 30s of the save, no restart".
	switchedAt := time.Now()
	setGlobalCustomProxy(t, settingsSvc, stubB.socks5URL)
	const ttlBound = 30 * time.Second
	deadline := switchedAt.Add(ttlBound)
	observed := false
	for !observed && time.Now().Before(deadline) {
		relay()
		observed = stubB.socksCountFor(upstreamHost) >= 1
		if !observed {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !observed {
		t.Fatalf("exit never switched to stub B within %s (B connects=%d)", ttlBound, stubB.socksCountFor(upstreamHost))
	}
	elapsed := time.Since(switchedAt)
	if elapsed > ttlBound {
		t.Fatalf("switch observed at %s, beyond the %s bound", elapsed, ttlBound)
	}
	t.Logf("exit switched from stub A to stub B %s after the save (no restart)", elapsed)

	// Post-switch: A receives nothing further (every new dial is B's), and
	// the relay keeps completing — a fresh tunnel through B or a reused
	// keep-alive connection through B are both "exit via B"; neither can
	// touch A.
	frozenA := stubA.httpTotal()
	relay()
	relay()
	if got := stubA.httpTotal(); got != frozenA {
		t.Fatalf("stub A carried %d additional requests after the switch to B", got-frozenA)
	}
	if hits() < 3 {
		t.Fatalf("upstream reached %d times, want >= 3 (pre-switch legs plus the poll)", hits())
	}
}
