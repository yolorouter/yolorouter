package osswire

// NewProxyResolvers' unit tests over a real system-settings service and a
// real sqlite database: the join itself is the subject — request context
// (stance or none) + settings rows → outboundproxy's verdict. The pure
// resolution arms are exhaustively tested in internal/outboundproxy; what
// these tests pin is that the GLUE feeds them the right inputs and never
// quietly reinterprets a verdict. Each case builds a fresh fixture: the
// settings cache serves a 30s TTL snapshot, so a case that wants a new
// global value pairs it with a new cache rather than fighting the TTL.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/outboundproxy"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// newProxyFixture builds a resolver pair over one fresh settings cache and
// database, with the global proxy rows already written (through the
// repository's own CAS update on the seeded version 1 — the same shape an
// admin's save produces).
func newProxyFixture(t *testing.T, globalMode, globalURL string) gateway.ProxyResolvers {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	if _, _, err := repository.UpdateOutboundProxy(db, 1, globalMode, globalURL); err != nil {
		t.Fatalf("seed global proxy rows: %v", err)
	}
	return NewProxyResolvers(systemsettings.NewSystemSettingsService(db))
}

// rawFixture returns the database too, for the case that tampers with the
// stored rows directly.
func rawFixture(t *testing.T) (gateway.ProxyResolvers, *gorm.DB) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	return NewProxyResolvers(systemsettings.NewSystemSettingsService(db)), db
}

// probeRequest builds the request shape the transport's Proxy callback
// receives, optionally stamped with a provider stance the way the kernel's
// dispatch sites stamp it.
func probeRequest(t *testing.T, targetURL string, stance *gateway.UpstreamProxyStance) *http.Request {
	t.Helper()
	ctx := context.Background()
	if stance != nil {
		ctx = gateway.WithUpstreamProxyStance(ctx, *stance)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return req
}

// upstreamVerdict resolves through the upstream seat and renders the
// verdict: "" for direct, the proxy URL when proxied, a test failure on
// resolution error.
func upstreamVerdict(t *testing.T, r gateway.ProxyResolvers, req *http.Request) string {
	t.Helper()
	u, err := r.Upstream(req)
	return renderVerdict(t, u, err)
}

// fetchVerdict resolves through the fetch seat and renders the verdict the
// same way.
func fetchVerdict(t *testing.T, r gateway.ProxyResolvers, req *http.Request) string {
	t.Helper()
	u, err := r.Fetch(req)
	return renderVerdict(t, u, err)
}

// renderVerdict renders one resolution result: "" for direct, the proxy
// URL when proxied, and a test failure on error.
func renderVerdict(t *testing.T, u *url.URL, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("resolution errored: %v", err)
	}
	if u == nil {
		return ""
	}
	return u.String()
}

func newStubProxy(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpstreamResolverProviderCustomWinsOverGlobalCustom(t *testing.T) {
	proxyA, proxyB := newStubProxy(t), newStubProxy(t)
	resolvers := newProxyFixture(t, outboundproxy.ModeCustom, proxyA.URL)

	req := probeRequest(t, "https://up.example/v1/chat", &gateway.UpstreamProxyStance{Mode: outboundproxy.ModeCustom, URL: proxyB.URL})
	if got := upstreamVerdict(t, resolvers, req); got != proxyB.URL {
		t.Fatalf("provider explicit override must win: got %q, want %q", got, proxyB.URL)
	}
}

func TestUpstreamResolverInheritUsesGlobalCustom(t *testing.T) {
	proxyA := newStubProxy(t)
	resolvers := newProxyFixture(t, outboundproxy.ModeCustom, proxyA.URL)

	req := probeRequest(t, "https://up.example/v1/chat", &gateway.UpstreamProxyStance{Mode: outboundproxy.ProviderModeInherit})
	if got := upstreamVerdict(t, resolvers, req); got != proxyA.URL {
		t.Fatalf("inherit stance must take the global proxy: got %q, want %q", got, proxyA.URL)
	}
}

func TestUpstreamResolverUnstampedRequestTakesGlobal(t *testing.T) {
	// The probe surface for a provider whose row is not in hand (an unsaved
	// provider preview) dispatches unstamped: global-only, the same
	// resolution a stamped inherit stance produces.
	proxyA := newStubProxy(t)
	resolvers := newProxyFixture(t, outboundproxy.ModeCustom, proxyA.URL)

	if got := upstreamVerdict(t, resolvers, probeRequest(t, "https://up.example/v1/chat", nil)); got != proxyA.URL {
		t.Fatalf("unstamped request must resolve global-only: got %q, want %q", got, proxyA.URL)
	}
}

func TestUpstreamResolverProviderOffOverridesGlobalCustom(t *testing.T) {
	proxyA := newStubProxy(t)
	resolvers := newProxyFixture(t, outboundproxy.ModeCustom, proxyA.URL)

	req := probeRequest(t, "https://up.example/v1/chat", &gateway.UpstreamProxyStance{Mode: outboundproxy.ModeOff})
	if got := upstreamVerdict(t, resolvers, req); got != "" {
		t.Fatalf("provider off must force direct even under a global proxy: got %q", got)
	}
}

func TestResolversGlobalOffIsDirect(t *testing.T) {
	resolvers := newProxyFixture(t, outboundproxy.ModeOff, "") // the seeded default

	stamped := probeRequest(t, "https://up.example/v1/chat", &gateway.UpstreamProxyStance{Mode: outboundproxy.ProviderModeInherit})
	if got := upstreamVerdict(t, resolvers, stamped); got != "" {
		t.Fatalf("inherit under global off must be direct: got %q", got)
	}
	if got := fetchVerdict(t, resolvers, probeRequest(t, "https://cdn.example/img.png", nil)); got != "" {
		t.Fatalf("fetch under global off must be direct: got %q", got)
	}
}

func TestFetchResolverIgnoresProviderStance(t *testing.T) {
	// The fetch seat is global-only by construction: even a request whose
	// context carries a provider stance (a fetch that runs on a request the
	// relay dispatched) resolves the global setting, never the provider's
	// override — the destination was third-party-chosen.
	req := probeRequest(t, "https://cdn.example/img.png", &gateway.UpstreamProxyStance{Mode: outboundproxy.ModeCustom, URL: "http://provider-proxy.example:3128"})

	off := newProxyFixture(t, outboundproxy.ModeOff, "")
	if got := fetchVerdict(t, off, req); got != "" {
		t.Fatalf("fetch seat consulted a provider stance: got %q, want direct", got)
	}

	proxyGlobal := newStubProxy(t)
	custom := newProxyFixture(t, outboundproxy.ModeCustom, proxyGlobal.URL)
	if got := fetchVerdict(t, custom, req); got != proxyGlobal.URL {
		t.Fatalf("fetch seat must take the global proxy: got %q, want %q", got, proxyGlobal.URL)
	}
}

func TestUpstreamResolverCorruptStoredURLAbortsInsteadOfDirect(t *testing.T) {
	// A tampered proxy_url row (a value that could never pass save-time
	// validation) must fail the request, never fall back to direct — the
	// resolver hands outboundproxy's error through untouched.
	resolvers, db := rawFixture(t)
	if err := db.Exec("UPDATE system_settings SET value = 'custom' WHERE key = 'proxy_mode'").Error; err != nil {
		t.Fatalf("tamper proxy_mode row: %v", err)
	}
	if err := db.Exec("UPDATE system_settings SET value = 'ftp://not-a-supported-scheme' WHERE key = 'proxy_url'").Error; err != nil {
		t.Fatalf("tamper proxy_url row: %v", err)
	}

	_, err := resolvers.Upstream(probeRequest(t, "https://up.example/v1/chat", nil))
	if err == nil {
		t.Fatal("a corrupt stored proxy URL must abort the request, not resolve")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("error should carry the parse verdict: %v", err)
	}
}

func TestUpstreamResolverSystemModeReadsEnvironment(t *testing.T) {
	resolvers := newProxyFixture(t, outboundproxy.ModeSystem, "")

	t.Setenv("HTTPS_PROXY", "http://env-proxy.example:3128")
	t.Setenv("NO_PROXY", "")

	if got := upstreamVerdict(t, resolvers, probeRequest(t, "https://up.example/v1/chat", nil)); got != "http://env-proxy.example:3128" {
		t.Fatalf("system mode must resolve the process environment: got %q", got)
	}
}
