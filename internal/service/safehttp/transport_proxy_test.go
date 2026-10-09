package safehttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubResolver is the test double for ProxyResolver: it returns a fixed
// URL (or error), counting consultations so tests can assert the resolver
// is actually in the request path.
type stubResolver struct {
	mu    sync.Mutex
	calls int
	proxy *url.URL
	err   error
}

func (s *stubResolver) resolve(*http.Request) (*url.URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.proxy, s.err
}

func (s *stubResolver) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// startRecordingHTTPProxy runs a loopback HTTP proxy stub that records each
// request's absolute-form URI (what an http-scheme proxy receives for an
// http target) and answers 200 "viaproxy".
func startRecordingHTTPProxy(t *testing.T) (baseURL string, uris func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.RequestURI)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("viaproxy"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// assertNotReservedRejection is the shared "address passed the policy gate"
// assertion for proxy dials: either the dial connected (definitely allowed),
// or it failed for a transport reason (refused/timeout/unreachable) whose
// message must NOT be the reserved-range policy rejection.
func assertNotReservedRejection(t *testing.T, conn net.Conn, err error) {
	t.Helper()
	if conn != nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Errorf("close conn: %v", closeErr)
		}
		return // the dial succeeded; the address was obviously allowed
	}
	if err == nil {
		t.Fatal("both conn and err are nil")
	}
	if strings.Contains(err.Error(), "reserved range") {
		t.Errorf("expected a transport failure, got the reserved-range policy rejection: %v", err)
	}
}

// TestProxyModeReversesTargetRejection pins the proxy-mode SSRF semantics:
// every target below is one the direct-mode policy rejects (loopback,
// private, link-local incl. cloud metadata, multicast, unspecified — the
// last two denied even with allowPrivate). With the resolver returning a
// proxy, none of them is resolved or validated locally: the request travels
// to the loopback proxy stub, which must receive the target in
// absolute-form. The direct-mode baseline is asserted in the same test so
// the reversal is self-contained rather than relying on the other tests
// staying green.
func TestProxyModeReversesTargetRejection(t *testing.T) {
	proxyBase, uris := startRecordingHTTPProxy(t)
	proxyURL, err := url.Parse(proxyBase)
	if err != nil {
		t.Fatalf("parse proxy stub URL: %v", err)
	}
	resolver := &stubResolver{proxy: proxyURL}
	proxied := &http.Client{
		Transport: NewProxyingTransport(false, 5*time.Second, 10*time.Second, resolver.resolve),
		Timeout:   5 * time.Second,
	}
	direct := &http.Client{Transport: NewTransport(false, 5*time.Second, 10*time.Second), Timeout: 5 * time.Second}

	targets := []string{
		"http://127.0.0.1:9/",        // loopback v4
		"http://10.255.255.1:80/",    // private v4
		"http://169.254.169.254:80/", // link-local v4 — cloud metadata
		"http://[::1]:9/",            // loopback v6
		"http://224.0.0.5:80/",       // multicast v4 (deniedCIDRs)
		"http://0.0.0.0:80/",         // unspecified v4 (deniedCIDRs)
	}
	for _, target := range targets {
		if _, err := direct.Get(target); err == nil {
			t.Fatalf("baseline broken: direct-mode request to %s unexpectedly succeeded; the reversal below would prove nothing", target)
		}

		resp, err := proxied.Get(target)
		if err != nil {
			t.Fatalf("proxied request to %s (rejected in direct mode) failed: %v", target, err)
		}
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Errorf("close body: %v", closeErr)
		}
		if err != nil {
			t.Fatalf("read body for %s: %v", target, err)
		}
		if resp.StatusCode != http.StatusOK || string(body) != "viaproxy" {
			t.Fatalf("proxied request to %s: status %d body %q", target, resp.StatusCode, body)
		}
	}

	got := uris()
	if len(got) != len(targets) {
		t.Fatalf("proxy stub saw %d requests, want %d (%v)", len(got), len(targets), got)
	}
	for i, target := range targets {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatalf("parse target: %v", err)
		}
		if !strings.Contains(got[i], u.Host) {
			t.Errorf("proxy stub request %d (%q) does not carry target host %q", i, got[i], u.Host)
		}
	}
	if resolver.count() != len(targets) {
		t.Fatalf("resolver consulted %d times, want %d", resolver.count(), len(targets))
	}
}

// TestHostnameProxyKeyMatchesTransportDialAddr is the make-or-break test for
// the registry mechanism: a hostname-keyed proxy address must match the
// opaque addr string net/http hands DialContext. It fails if
// canonicalProxyAddr drifts from the standard library's canonicalAddr,
// because a miss routes the proxy dial into safeDialContext, whose target
// policy rejects the loopback IP this stub resolves the proxy name to.
func TestHostnameProxyKeyMatchesTransportDialAddr(t *testing.T) {
	const proxyHost = "proxy.internal.test"
	const targetHost = "upstream-only-the-proxy-can-reach.invalid"

	proxyBase, uris := startRecordingHTTPProxy(t)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(proxyBase, "http://"))
	if err != nil {
		t.Fatalf("split proxy stub addr: %v", err)
	}

	var mu sync.Mutex
	var resolved []string
	original := resolveHost
	resolveHost = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		mu.Lock()
		resolved = append(resolved, host)
		mu.Unlock()
		if host == proxyHost {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return original(ctx, host)
	}
	t.Cleanup(func() { resolveHost = original })

	proxyURL, err := url.Parse("http://" + proxyHost + ":" + port)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	resolver := &stubResolver{proxy: proxyURL}
	client := &http.Client{
		Transport: NewProxyingTransport(false, 5*time.Second, 10*time.Second, resolver.resolve),
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get("http://" + targetHost + "/")
	if err != nil {
		t.Fatalf("request through hostname proxy failed (registry key likely drifted from net/http's dial addr): %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Errorf("close body: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "viaproxy" {
		t.Fatalf("unexpected body %q", body)
	}

	if got := uris(); len(got) != 1 || !strings.Contains(got[0], targetHost) {
		t.Fatalf("proxy stub requests = %v, want one carrying %q", got, targetHost)
	}

	mu.Lock()
	defer mu.Unlock()
	sawProxy := false
	for _, host := range resolved {
		if host == targetHost {
			t.Errorf("target host %q was resolved locally; a proxied request must never resolve its target", targetHost)
		}
		if host == proxyHost {
			sawProxy = true
		}
	}
	if !sawProxy {
		t.Errorf("proxy host %q was never resolved via resolveHost", proxyHost)
	}
}

// TestCanonicalProxyAddrFillsSchemeDefaultPorts pins the mirror of net/http's
// canonicalAddr: default ports per scheme, explicit ports kept, IPv6
// bracketed, and ASCII case NOT folded (the standard library's idnaASCII
// returns ASCII input unchanged).
func TestCanonicalProxyAddrFillsSchemeDefaultPorts(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"http://h", "h:80"},
		{"https://h", "h:443"},
		{"socks5://h", "h:1080"},
		{"socks5h://h", "h:1080"},
		{"socks5h://h:9", "h:9"},
		{"http://h:3128", "h:3128"},
		{"http://[::1]", "[::1]:80"},
		{"http://Proxy.LAN:3128", "Proxy.LAN:3128"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		if got := canonicalProxyAddr(u); got != c.want {
			t.Errorf("canonicalProxyAddr(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestProxyDialRejectsReservedRangeProxyAddresses is the transport-level
// backstop of the proxy-address policy: a proxy URL whose address sits in an
// always-denied reserved range is refused AT DIAL TIME, with the same
// wording the save-time validation uses — the transport never trusts that
// the stored URL was validated on the way in.
func TestProxyDialRejectsReservedRangeProxyAddresses(t *testing.T) {
	reserved := []string{
		"http://224.0.0.1:8080",    // multicast v4
		"http://240.0.0.1:1080",    // reserved v4
		"http://0.0.0.0:8080",      // unspecified v4
		"http://[ff00::1]:1080",    // multicast v6
		"socks5://198.18.0.1:1080", // benchmark (socks leg dials before handshake)
	}
	for _, raw := range reserved {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			resolver := &stubResolver{proxy: u}
			client := &http.Client{
				Transport: NewProxyingTransport(false, 5*time.Second, 10*time.Second, resolver.resolve),
				Timeout:   5 * time.Second,
			}
			_, err = client.Get("http://example.invalid/")
			if err == nil {
				t.Fatalf("request through reserved-range proxy %s unexpectedly succeeded", raw)
			}
			if !strings.Contains(err.Error(), "reserved range") {
				t.Fatalf("expected the reserved-range policy rejection for proxy %s, got: %v", raw, err)
			}
		})
	}
}

// TestProxyDialPolicyAllowsPrivateAndLoopbackProxyAddresses covers the
// allowed half of the transport-level proxy-address policy: loopback and
// private proxy addresses pass the gate (a LAN proxy or loopback test stub
// is the primary self-hosted topology). The end-to-end loopback leg is
// already proven by every proxy stub above; here the dial-level legs
// assert a private address and a closed loopback port fail for TRANSPORT
// reasons, not policy ones.
func TestProxyDialPolicyAllowsPrivateAndLoopbackProxyAddresses(t *testing.T) {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Private v4: whatever the local network does with 10.255.255.254:1
	// (refuse, black-hole until the 300ms budget, or connect), the outcome
	// must not be the reserved-range rejection.
	conn, err := dialProxyAddress(ctx, dialer, "tcp", "10.255.255.254:1", 300*time.Millisecond)
	assertNotReservedRejection(t, conn, err)

	// Loopback with a deterministically closed port: connection refused is
	// the expected transport error, proving the loopback address itself
	// passed the policy gate.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	conn, err = dialProxyAddress(ctx, dialer, "tcp", "127.0.0.1:"+port, 2*time.Second)
	assertNotReservedRejection(t, conn, err)
}

// startMiniSocks5Proxy runs a loopback SOCKS5 stub speaking just enough of
// RFC 1928 for the standard library's client: no-auth greeting, one CONNECT
// request whose target hostname is recorded (that record is the assertion
// surface for proxy-side DNS), success reply, then a canned HTTP response
// for whatever the client tunnels through the connection.
func startMiniSocks5Proxy(t *testing.T) (addr string, targetHost func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var got string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			go serveSocksConn(conn, &mu, &got)
		}
	}()
	return ln.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func serveSocksConn(conn net.Conn, mu *sync.Mutex, got *string) {
	defer func() { _ = conn.Close() }()
	// Client greeting: VER NMETHODS METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil { // NO AUTHENTICATION REQUIRED
		return
	}
	// CONNECT request: VER CMD RSV ATYP ADDR PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x03: // domain name
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = string(b)
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	mu.Lock()
	*got = host
	mu.Unlock()
	// Reply: succeeded, BND.ADDR 0.0.0.0:0.
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	// The tunneled request never needs a real upstream: answer immediately.
	_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 7\r\nConnection: close\r\n\r\nviasock"))
}

// TestSocksProxyHandsTargetHostnameToProxySide pins the socks5/socks5h
// resolution semantics (they are the same scheme to net/http): the target
// hostname is handed to the proxy unresolved. The target here is a .invalid
// name — guaranteed unresolvable anywhere (RFC 6761) — so the request
// succeeding at all proves no local resolution happened; the stub's record
// proves the hostname arrived at the proxy side, and the resolveHost hook
// proves the target never touched the local resolver.
func TestSocksProxyHandsTargetHostnameToProxySide(t *testing.T) {
	const targetHost = "socks-target.invalid"
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			addr, recordedHost := startMiniSocks5Proxy(t)
			proxyURL, err := url.Parse(scheme + "://" + addr)
			if err != nil {
				t.Fatalf("parse proxy URL: %v", err)
			}
			resolver := &stubResolver{proxy: proxyURL}

			var mu sync.Mutex
			var resolved []string
			original := resolveHost
			resolveHost = func(ctx context.Context, host string) ([]net.IPAddr, error) {
				mu.Lock()
				resolved = append(resolved, host)
				mu.Unlock()
				return original(ctx, host)
			}
			t.Cleanup(func() { resolveHost = original })

			client := &http.Client{
				Transport: NewProxyingTransport(false, 5*time.Second, 10*time.Second, resolver.resolve),
				Timeout:   10 * time.Second,
			}
			resp, err := client.Get("http://" + targetHost + "/")
			if err != nil {
				t.Fatalf("request through %s proxy failed: %v", scheme, err)
			}
			body, err := io.ReadAll(resp.Body)
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Errorf("close body: %v", closeErr)
			}
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(body) != "viasock" {
				t.Fatalf("unexpected body %q", body)
			}

			if got := recordedHost(); got != targetHost {
				t.Fatalf("proxy received target %q, want the unresolved hostname %q", got, targetHost)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, host := range resolved {
				if host == targetHost {
					t.Fatalf("target %q was resolved locally; a socks5/socks5h request must resolve proxy-side", targetHost)
				}
			}
		})
	}
}

// TestOffModeProxyingTransportMatchesPlainTransportBehavior is the mode=off
// zero-behavior-change regression: a resolver that always answers "no
// proxy" must leave the transport byte-for-byte equivalent to the original
// NewTransport — same failures, same error strings — on both a live
// loopback target and denied-range targets, with proxy environment
// variables set and unset. The env legs matter because a non-nil Proxy
// callback is a different transport configuration than Proxy == nil; the
// assertion that both produce IDENTICAL errors rules out the callback
// accidentally re-enabling environment proxying.
func TestOffModeProxyingTransportMatchesPlainTransportBehavior(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	off := &stubResolver{} // returns nil, nil — "mode=off"
	offClient := &http.Client{
		Transport: NewProxyingTransport(false, 5*time.Second, 10*time.Second, off.resolve),
		Timeout:   5 * time.Second,
	}
	plainClient := &http.Client{Transport: NewTransport(false, 5*time.Second, 10*time.Second), Timeout: 5 * time.Second}

	targets := []string{
		srv.URL,                // live loopback server: must stay SSRF-blocked
		"http://224.0.0.5:80/", // multicast target
		"http://0.0.0.0:80/",   // unspecified target
	}
	doPair := func(t *testing.T) {
		t.Helper()
		for _, target := range targets {
			_, plainErr := plainClient.Get(target)
			_, offErr := offClient.Get(target)
			if plainErr == nil || offErr == nil {
				t.Fatalf("target %s: plainErr=%v offErr=%v — mode=off must reject exactly like the plain transport", target, plainErr, offErr)
			}
			if plainErr.Error() != offErr.Error() {
				t.Fatalf("target %s: mode=off diverged from plain transport\nplain: %v\noff:   %v", target, plainErr, offErr)
			}
		}
	}

	doPair(t) // without env
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	doPair(t) // with env: still never honored

	if got := off.count(); got != 2*len(targets) {
		t.Fatalf("resolver consulted %d times, want %d (once per request)", got, 2*len(targets))
	}
}

// TestProxyResolverErrorAbortsRequest pins the error contract: a resolver
// failure (corrupt stored configuration) aborts the request instead of
// falling back to a direct connection. The target is a live loopback server
// so the two outcomes are distinguishable — a silent direct fallback would
// surface the SSRF "disallowed range" rejection, not the resolver's error.
func TestProxyResolverErrorAbortsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sentinel := errors.New("corrupt stored proxy URL")
	resolver := &stubResolver{err: sentinel}
	client := &http.Client{
		Transport: NewProxyingTransport(false, 5*time.Second, 10*time.Second, resolver.resolve),
		Timeout:   5 * time.Second,
	}
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("expected the request to fail, got success")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the resolver's error to surface, got: %v", err)
	}
	if strings.Contains(err.Error(), "disallowed range") {
		t.Fatalf("error mentions the direct-dial SSRF rejection; the request must not fall back to direct: %v", err)
	}
}
