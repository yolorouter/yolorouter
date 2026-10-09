package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/config"
)

// testGatewayConfig returns the production gateway defaults via the exported
// constructor, so a test-built Service behaves the same as a
// production-wired one; individual tests can override fields on the returned
// struct as needed.
func testGatewayConfig() config.GatewayConfig {
	return config.DefaultGatewayConfig()
}

// TestNewUpstreamClient_SetsResponseHeaderTimeout verifies the headerTimeout
// argument lands on the SSRF-safe transport's ResponseHeaderTimeout, bounding
// the wait for the first byte of an upstream response (the reasoning-model
// "thinking" phase) independently of the per-attempt context that later bounds
// the body.
func TestNewUpstreamClient_SetsResponseHeaderTimeout(t *testing.T) {
	c := NewUpstreamClient(false, 600*time.Second, 5*time.Second, 10*time.Second, nil)
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.httpClient.Transport)
	}
	if got := tr.ResponseHeaderTimeout; got != 600*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 600s", got)
	}
}

// TestNewUpstreamClient_ZeroHeaderTimeout keeps the zero-value path explicit:
// a caller passing 0 must produce a transport whose ResponseHeaderTimeout is
// also 0 (no header-phase bound), rather than silently substituting a default.
func TestNewUpstreamClient_ZeroHeaderTimeout(t *testing.T) {
	c := NewUpstreamClient(false, 0, 5*time.Second, 10*time.Second, nil)
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.httpClient.Transport)
	}
	if got := tr.ResponseHeaderTimeout; got != 0 {
		t.Errorf("ResponseHeaderTimeout = %v, want 0", got)
	}
}

// TestNewUpstreamClient_SetsTLSHandshakeTimeout verifies the tlsHandshake
// argument lands on the SSRF-safe transport's TLSHandshakeTimeout, bounding
// the TLS handshake independently of the TCP dial and per-attempt context.
func TestNewUpstreamClient_SetsTLSHandshakeTimeout(t *testing.T) {
	c := NewUpstreamClient(false, 0, 5*time.Second, 10*time.Second, nil)
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.httpClient.Transport)
	}
	if got := tr.TLSHandshakeTimeout; got != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 10s", got)
	}
}

// TestNewUpstreamClient_ForwardsConnectTimeout verifies the connectTimeout
// argument reaches the SSRF-safe transport's DialContext. The net.Dialer that
// owns the timeout is captured inside the DialContext closure and is not a
// field on *http.Transport, so reflect-based inspection (as used for
// ResponseHeaderTimeout above) is not possible; instead this is a behavioral
// check that mirrors safehttp.TestNewTransport_SetsDialTimeout. It dials an
// unroutable TEST-NET-1 address (RFC 5737) with a short connectTimeout and
// confirms the dial returns within a window far shorter than the OS default
// retransmit timeout. Environments that immediately reject the route are
// skipped rather than false-failing.
func TestNewUpstreamClient_ForwardsConnectTimeout(t *testing.T) {
	c := NewUpstreamClient(false, 0, 150*time.Millisecond, 10*time.Second, nil)
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.httpClient.Transport)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := tr.DialContext(ctx, "tcp", "192.0.2.1:80") // TEST-NET-1, unroutable
	elapsed := time.Since(start)
	if conn != nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Errorf("close unexpected conn: %v", closeErr)
		}
		t.Fatal("expected dial to unroutable TEST-NET-1 address to fail")
	}
	if err == nil {
		t.Fatal("expected error from dial to unroutable address")
	}
	if elapsed < 150*time.Millisecond {
		t.Skipf("route rejected in %v; cannot exercise dialer timeout in this environment", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("dial took %v with 150ms connectTimeout; connectTimeout may not be wired into the upstream client", elapsed)
	}
}
