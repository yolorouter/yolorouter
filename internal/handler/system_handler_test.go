package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	versionsvc "github.com/yolorouter/yolorouter/internal/service/version"
)

// fakeVersionChecker is a test double for the handler's VersionChecker
// dependency: returns a fixed status and records which entry point ran.
type fakeVersionChecker struct {
	status      versionsvc.VersionStatus
	called      bool
	freshCalls  int
	cachedCalls int
}

func (f *fakeVersionChecker) Check(_ context.Context) versionsvc.VersionStatus {
	f.called = true
	f.cachedCalls++
	return f.status
}

func (f *fakeVersionChecker) CheckFresh(_ context.Context) versionsvc.VersionStatus {
	f.called = true
	f.freshCalls++
	return f.status
}

func newSystemTestRouter(info SystemInfo, svc VersionChecker) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/admin/system/version", GetSystemVersion(info, svc))
	return r
}

func TestGetSystemVersionReportsBuildInfo(t *testing.T) {
	info := SystemInfo{
		Version: "v0.1.0", Commit: "abc1234", BuildTime: "2026-07-20T00:00:00Z",
		GoVersion: "go1.26.2", GOOS: "linux", GOARCH: "amd64", DBDriver: "sqlite",
		UpdateMode: "in_place",
	}
	fake := &fakeVersionChecker{}

	r := newSystemTestRouter(info, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/system/version", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body %s", w.Code, w.Body.String())
	}
	if !fake.called {
		t.Fatalf("update check was not invoked")
	}

	data := decodeEnvelopeData(t, w.Body.Bytes())
	assertField(t, data, "version", "v0.1.0")
	assertField(t, data, "commit", "abc1234")
	assertField(t, data, "build_time", "2026-07-20T00:00:00Z")
	assertField(t, data, "go_version", "go1.26.2")
	assertField(t, data, "goos", "linux")
	assertField(t, data, "goarch", "amd64")
	assertField(t, data, "db_driver", "sqlite")
	// update_mode drives whether the console renders the one-click update
	// button or a per-runtime hint; dropping it would silently hide the
	// button everywhere.
	assertField(t, data, "update_mode", "in_place")
	// The gateway address is not part of this payload — it has its own
	// endpoint, readable without the admin role.
	if _, ok := data["endpoint"]; ok {
		t.Fatalf("endpoint must not ride along on the version payload, got %v", data)
	}
}

func newEndpointTestRouter(externalURL string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/admin/system/endpoint", GetSystemEndpoint(externalURL))
	return r
}

func endpointFor(t *testing.T, externalURL string, prepare func(*http.Request)) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/system/endpoint", nil)
	if prepare != nil {
		prepare(req)
	}
	w := httptest.NewRecorder()
	newEndpointTestRouter(externalURL).ServeHTTP(w, req)
	return decodeEnvelopeData(t, w.Body.Bytes())
}

func TestGetSystemEndpointConfiguredWins(t *testing.T) {
	// The configured origin is pinned regardless of client-controlled
	// Host / X-Forwarded-Proto headers.
	data := endpointFor(t, "https://gateway.example.com", func(req *http.Request) {
		req.Header.Set("X-Forwarded-Proto", "http")
		req.Host = "attacker.example"
	})
	assertField(t, data, "endpoint", "https://gateway.example.com")
}

func TestGetSystemEndpointDerivesFromForwarding(t *testing.T) {
	data := endpointFor(t, "", func(req *http.Request) {
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Host = "router.lan:8080"
	})
	assertField(t, data, "endpoint", "https://router.lan:8080")
}

func TestGetSystemEndpointFallsBackToPlainHost(t *testing.T) {
	// No configured value and no forwarding header: derive from the Host
	// alone (httptest defaults to example.com).
	assertField(t, endpointFor(t, "", nil), "endpoint", "http://example.com")
}

func TestGetSystemVersionMergesUpdateStatus(t *testing.T) {
	info := SystemInfo{Version: "v0.1.0"}
	fake := &fakeVersionChecker{status: versionsvc.VersionStatus{
		Current: "v0.1.0", Latest: "v0.2.0", HasUpdate: true, ReleaseURL: "https://example/release",
	}}

	r := newSystemTestRouter(info, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/system/version", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	data := decodeEnvelopeData(t, w.Body.Bytes())
	assertField(t, data, "latest", "v0.2.0")
	assertField(t, data, "has_update", true)
	assertField(t, data, "release_url", "https://example/release")
	assertField(t, data, "check_failed", false)
}

func TestGetSystemVersionSurfacesCheckFailed(t *testing.T) {
	info := SystemInfo{Version: "v0.1.0"}
	// A failed check (disabled, GitHub down, rate limit) must still return
	// 200 with check_failed=true rather than a 500 — the admin UI degrades
	// to "check failed", not an error toast.
	fake := &fakeVersionChecker{status: versionsvc.VersionStatus{Current: "v0.1.0", CheckFailed: true}}

	r := newSystemTestRouter(info, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/system/version", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("a failed check must still be 200, got %d", w.Code)
	}
	data := decodeEnvelopeData(t, w.Body.Bytes())
	assertField(t, data, "check_failed", true)
	assertField(t, data, "has_update", false)
}

// TestGetSystemVersionForceBypassesCache pins the ?force=1 contract: an
// operator-initiated check must hit the cache-bypassing entry point, and a
// plain load must not.
func TestGetSystemVersionForceBypassesCache(t *testing.T) {
	fake := &fakeVersionChecker{}
	r := newSystemTestRouter(SystemInfo{Version: "v0.1.0"}, fake)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/system/version?force=1", nil))
	if fake.freshCalls != 1 || fake.cachedCalls != 0 {
		t.Fatalf("force=1 must call CheckFresh exactly once (fresh=%d cached=%d)", fake.freshCalls, fake.cachedCalls)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/system/version", nil))
	if fake.freshCalls != 1 || fake.cachedCalls != 1 {
		t.Fatalf("a plain load must use the cached Check (fresh=%d cached=%d)", fake.freshCalls, fake.cachedCalls)
	}
}

func TestGetSystemVersionReportsNonNegativeUptime(t *testing.T) {
	info := SystemInfo{Version: "v0.1.0"}
	fake := &fakeVersionChecker{}
	r := newSystemTestRouter(info, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/system/version", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	data := decodeEnvelopeData(t, w.Body.Bytes())
	up, ok := data["uptime_seconds"].(float64)
	if !ok || up < 0 {
		t.Fatalf("uptime_seconds should be a non-negative number, got %v", data["uptime_seconds"])
	}
}

func decodeEnvelopeData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v, body %s", err, body)
	}
	if env.Code != 0 {
		t.Fatalf("expected envelope code 0, got %d, body %s", env.Code, body)
	}
	if env.Data == nil {
		t.Fatalf("envelope data is nil, body %s", body)
	}
	return env.Data
}

func assertField(t *testing.T, data map[string]any, key string, want any) {
	t.Helper()
	got, ok := data[key]
	if !ok {
		t.Fatalf("expected field %q in data, got %v", key, data)
	}
	if got != want {
		t.Fatalf("field %q = %v, want %v", key, got, want)
	}
}
