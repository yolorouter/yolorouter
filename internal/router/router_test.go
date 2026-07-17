package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
	"github.com/yolorouter/yolorouter-ce/pkg/response"
)

func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	return r
}

// TestNewFailsFastWhenEmbeddedFrontendIsBroken is the integration test the
// -tags embed adversarial-review round asked for: a distFS with real files
// but no index.html (a broken frontend build — e.g. a Vite output-path
// misconfiguration — that still exited 0) must make New() itself fail,
// not just degrade a specific route at request time. This is the whole
// point of validateEmbeddedFrontend running at construction time: /healthz
// is a separate, unconditionally-registered route that bypasses NoRoute
// entirely, so if New() didn't fail here, a broken embed would still
// report healthy while every real page request 500s — invisible to any
// health/readiness check. Uses newWithDistFS directly (an injected
// fstest.MapFS) since a real embed.FS can only ever come from an actual
// compile-time embed directive — there's no way to construct a
// "populated but missing index.html" embed.FS to drive this through the
// public New().
func TestNewFailsFastWhenEmbeddedFrontendIsBroken(t *testing.T) {
	broken := fstest.MapFS{
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
		// deliberately no index.html
	}
	_, err := newWithDistFS(broken)
	if err == nil {
		t.Fatalf("expected New() to fail when distFS has files but no index.html")
	}
}

// TestNewSucceedsWithEmptyDistFS guards the other half of the same
// invariant: an empty distFS (no frontend embedded at all — the state
// every plain, non -tags embed build always has, see web/embed_stub.go)
// must NOT be treated as broken; New() should succeed and fall back to the
// placeholder at request time (see TestUnknownFrontendPathFallsBackToIndexHTML).
func TestNewSucceedsWithEmptyDistFS(t *testing.T) {
	if _, err := newWithDistFS(fstest.MapFS{}); err != nil {
		t.Fatalf("expected New() to succeed with an empty distFS, got: %v", err)
	}
}

// TestNewSucceedsWithCompleteFrontend guards the third case: a distFS with
// both real files and an index.html referencing them by real Vite-shaped
// root-relative paths must be accepted, and an external URL reference
// (nothing local to check it against) must not be treated as broken.
func TestNewSucceedsWithCompleteFrontend(t *testing.T) {
	complete := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(
			`<html><head><script src="/assets/app.js"></script>` +
				`<link rel="preconnect" href="https://fonts.googleapis.com"></head></html>`,
		)},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	if _, err := newWithDistFS(complete); err != nil {
		t.Fatalf("expected New() to succeed with a complete frontend, got: %v", err)
	}
}

// TestNewFailsForEmptyIndexHTML guards against a truncated/zero-byte
// index.html (e.g. an interrupted copy) passing validation just because a
// file with that name exists.
func TestNewFailsForEmptyIndexHTML(t *testing.T) {
	empty := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	if _, err := newWithDistFS(empty); err == nil {
		t.Fatalf("expected New() to fail for an empty index.html")
	}
}

// TestNewFailsWhenIndexHTMLReferencesMissingAsset guards against a partial
// copy: index.html landed, but the hashed asset it references didn't — a
// blank page or 404s in the browser despite router.New() and /healthz both
// reporting success, unless caught here.
func TestNewFailsWhenIndexHTMLReferencesMissingAsset(t *testing.T) {
	partial := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(
			`<html><head><script src="/assets/missing-CNWoupNg.js"></script></head></html>`,
		)},
	}
	if _, err := newWithDistFS(partial); err == nil {
		t.Fatalf("expected New() to fail when index.html references a missing local asset")
	}
}

func TestHealthzReturnsOK(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("unexpected content-type: %s", w.Header().Get("Content-Type"))
	}
}

func TestHealthzAcceptsHead(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodHead, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHealthzRejectsPost(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	// NoMethod must route through the same unified envelope as NoRoute,
	// not Gin's default plain-text 405 (design doc §8).
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("expected JSON envelope content-type, got %q, body: %s", ct, w.Body.String())
	}
	assertAdminEnvelope(t, w.Body.Bytes(), errcode.MethodNotAllowed)
}

func TestUnknownAPIPathReturns404Envelope(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	assertAdminEnvelope(t, w.Body.Bytes(), errcode.RouteNotFound)
}

func TestUnknownFrontendPathFallsBackToIndexHTML(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/some/spa/route", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (index.html fallback), got %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Fatalf("expected non-empty body")
	}
}

func TestMissingStaticAssetReturnsRealNotFound(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/assets/does-not-exist.js", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing static asset, got %d", w.Code)
	}
}

// TestIsRegularFileRejectsDirectories is a direct unit test of isRegularFile
// against a real directory entry via fstest.MapFS. The embedded frontend's
// dist FS (internal/web/dist, populated via a go:embed directive) only ever
// holds the placeholder index.html in this repo/test environment, with no
// real assets/ directory to exercise, so an httptest.NewRequest-based
// integration test against router.New() would pass even with the
// !info.IsDir() check removed entirely. This test targets the function
// itself so it actually fails if that check regresses.
func TestIsRegularFileRejectsDirectories(t *testing.T) {
	fsys := fstest.MapFS{
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	if isRegularFile(fsys, "assets") {
		t.Fatalf("expected isRegularFile to reject a directory")
	}
	if !isRegularFile(fsys, "assets/app.js") {
		t.Fatalf("expected isRegularFile to accept a real file")
	}
	if isRegularFile(fsys, "does-not-exist") {
		t.Fatalf("expected isRegularFile to reject a missing path")
	}
}

// TestHasAnyFileDistinguishesEmptyFromPopulated locks in the mechanism
// router.New() uses to tell "no frontend build embedded at all" (the
// state every plain, non -tags embed build always has — serve the
// friendly placeholder) apart from "a frontend build was embedded but
// it's missing index.html" (a broken build — e.g. a Vite output-path
// misconfiguration that still exits 0; must surface as a real error
// instead of silently serving the same 200 placeholder as the expected
// case, which would let a broken production deploy go live invisibly).
// Direct unit test via fstest.MapFS, the same reasoning
// TestIsRegularFileRejectsDirectories above uses.
func TestHasAnyFileDistinguishesEmptyFromPopulated(t *testing.T) {
	if hasAnyFile(fstest.MapFS{}) {
		t.Fatalf("expected hasAnyFile to report false for an empty FS")
	}
	populated := fstest.MapFS{"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")}}
	if !hasAnyFile(populated) {
		t.Fatalf("expected hasAnyFile to report true for a populated FS")
	}
}

// TestAssetsDirectoryReturns404 is the router-level companion to
// TestIsRegularFileRejectsDirectories above: with only a placeholder
// index.html embedded, /assets and /assets/ simply have nothing to serve —
// this guards the isStaticAssetNamespace real-404 fallback path stays a 404
// (not an accidental SPA-fallback 200), independent of directory-listing
// behavior specifically.
func TestAssetsDirectoryReturns404(t *testing.T) {
	r := newTestRouter(t)
	for _, path := range []string{"/assets", "/assets/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("path %q: expected 404, got %d, body: %s", path, w.Code, w.Body.String())
		}
	}
}

// TestUnknownV1PathReturnsOpenAICompatibleEnvelope guards the /api vs /v1
// namespace split: /v1/* must never leak the admin pkg/response envelope
// (code/message/data/timestamp) — gateway clients expect the OpenAI-style
// {"error": {message, type, code}} shape instead (design doc §7/§9).
func TestUnknownV1PathReturnsOpenAICompatibleEnvelope(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	assertGatewayEnvelope(t, w.Body.Bytes(), "route_not_found")
}

// TestV1WrongMethodReturnsOpenAICompatibleEnvelope drives the NoMethod path
// specifically (not NoRoute) by registering a real GET route under /v1 and
// hitting it with POST — M0 has no real /v1 routes yet, but the dispatcher
// itself (shared with NoRoute and Recovery via middleware.WriteNamespacedError)
// is already exercisable this way.
func TestV1WrongMethodReturnsOpenAICompatibleEnvelope(t *testing.T) {
	r := newTestRouter(t)
	r.GET("/v1/test-only-route", func(c *gin.Context) {})
	req := httptest.NewRequest(http.MethodPost, "/v1/test-only-route", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	assertGatewayEnvelope(t, w.Body.Bytes(), "method_not_allowed")
}

func assertAdminEnvelope(t *testing.T, body []byte, wantCode int) {
	t.Helper()
	var env response.Response
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("expected admin envelope JSON, got unparseable body %s: %v", body, err)
	}
	if env.Code != wantCode {
		t.Fatalf("expected code %d, got %d (body: %s)", wantCode, env.Code, body)
	}
	if env.Message == "" {
		t.Fatalf("expected non-empty message, body: %s", body)
	}
	if env.Timestamp == 0 {
		t.Fatalf("expected non-zero timestamp, body: %s", body)
	}
}

func assertGatewayEnvelope(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("expected OpenAI-style error envelope JSON, got unparseable body %s: %v", body, err)
	}
	if env.Error.Code != wantCode {
		t.Fatalf("expected error.code %q, got %q (body: %s)", wantCode, env.Error.Code, body)
	}
	if env.Error.Message == "" || env.Error.Type == "" {
		t.Fatalf("expected non-empty error.message and error.type, body: %s", body)
	}
	// The admin envelope's fields must not leak into this shape.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(body, &raw)
	if _, ok := raw["timestamp"]; ok {
		t.Fatalf("must not leak the admin envelope's timestamp field, got: %s", body)
	}
	if _, ok := raw["code"]; ok {
		t.Fatalf("must not leak the admin envelope's top-level code field, got: %s", body)
	}
}
