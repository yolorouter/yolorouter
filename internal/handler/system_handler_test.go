package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/service"
)

// fakeVersionChecker is a test double for the handler's VersionChecker
// dependency: returns a fixed status and records that Check was called.
type fakeVersionChecker struct {
	status service.VersionStatus
	called bool
}

func (f *fakeVersionChecker) Check(_ context.Context) service.VersionStatus {
	f.called = true
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
}

func TestGetSystemVersionMergesUpdateStatus(t *testing.T) {
	info := SystemInfo{Version: "v0.1.0"}
	fake := &fakeVersionChecker{status: service.VersionStatus{
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
	fake := &fakeVersionChecker{status: service.VersionStatus{Current: "v0.1.0", CheckFailed: true}}

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
