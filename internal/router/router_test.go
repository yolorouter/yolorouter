package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/config"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	r, err := New(testDeps(t, db))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	return r
}

// testDeps is the one Deps every router test builds the engine from.
func testDeps(t *testing.T, db *gorm.DB) Deps {
	t.Helper()
	return Deps{
		DB:                db,
		ProviderMasterKey: testutil.ProviderMasterKey(),
		BodiesDir:         t.TempDir(),
		Update:            testUpdateConfig(),
		Gateway:           testGatewayConfig(),
	}
}

// testUpdateConfig enables the update feature with no explicit repo, so the
// /system/version route wires up against the compiled-in DefaultGitHubRepo
// (empty in tests — VersionService.Check short-circuits to check_failed, no
// network).
func testUpdateConfig() config.UpdateConfig {
	return config.UpdateConfig{Enabled: true}
}

// testGatewayConfig returns the production gateway defaults via the exported
// constructor. Router-level tests don't drive a real upstream relay, but the
// value is threaded through so the wiring stays identical to production
// instead of substituting a zero struct.
func testGatewayConfig() config.GatewayConfig {
	return config.DefaultGatewayConfig()
}

func TestProviderRoutesAreRegisteredUnderProtectedGroup(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// No session cookie: RequireSession must reject this before it
	// ever reaches the provider handler — proves providers routes are on
	// the `protected` subgroup, not directly on `admin`.
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unauthenticated request to a provider route, got %d, body: %s", w.Code, w.Body.String())
	}
}

// TestNewFailsFastWhenEmbeddedFrontendIsBroken is the integration test for
// -tags embed builds: a distFS with real files
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
	_, err := newWithDistFS(broken, testDeps(t, testutil.NewSQLiteDB(t)))
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
	if _, err := newWithDistFS(fstest.MapFS{}, testDeps(t, testutil.NewSQLiteDB(t))); err != nil {
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
	if _, err := newWithDistFS(complete, testDeps(t, testutil.NewSQLiteDB(t))); err != nil {
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
	if _, err := newWithDistFS(empty, testDeps(t, testutil.NewSQLiteDB(t))); err == nil {
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
	if _, err := newWithDistFS(partial, testDeps(t, testutil.NewSQLiteDB(t))); err == nil {
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
	// not Gin's default plain-text 405.
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
// {"error": {message, type, code}} shape instead.
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
// hitting it with POST — there are no real /v1 routes yet, but the dispatcher
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

// seedAPIKey inserts one active APIKey row keyed to rawKey and returns it,
// mirroring the pattern internal/middleware/api_key_auth_test.go and
// internal/gateway/relay_test.go both already use for their own fixtures.
func seedAPIKey(t *testing.T, db *gorm.DB, rawKey string) *model.APIKey {
	t.Helper()
	now := time.Now().UTC()
	prefixLen := len(rawKey)
	if prefixLen > 12 {
		prefixLen = 12
	}
	// The key needs an enabled owning account — APIKeyAuth verifies the
	// owner's status before admitting the credential.
	owner := &model.User{Username: "key-owner-" + rawKey[:prefixLen], Role: model.RoleMember,
		Status: model.UserStatusEnabled, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(owner).Error; err != nil {
		t.Fatalf("seed key owner: %v", err)
	}
	k := &model.APIKey{
		KeyHash: crypto.HashToken(rawKey), KeyPrefix: rawKey[:prefixLen], UserID: owner.ID, Status: model.APIKeyStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(k).Error; err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	return k
}

// TestMessagesRouteReachesGatewayWithValidKey proves POST /v1/messages is
// registered on the same protected /v1 group as /v1/chat/completions
// (inherits BodySizeLimit + APIKeyAuth + the bodies-dir stash) and actually
// dispatches into the gateway handler for a caller with a valid key — no
// provider/model is configured in this test, so the request won't succeed
// end to end (Service.Handle itself replies 404 "model does not exist",
// a legitimate gateway-generated response, not a routing failure). What this
// asserts is that the response is the Claude envelope Handle produces for
// that rejection — proving both that the route dispatched into the gateway
// handler (not 401 auth-rejected, not a route/method 404/405) and that it
// resolved /v1/messages to the Claude ingress while doing so.
func TestMessagesRouteReachesGatewayWithValidKey(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-messages-route")
	r, err := New(testDeps(t, db))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	body := []byte(`{"model":"claude-3-opus","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("X-Api-Key", "sk-yr-messages-route")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusMethodNotAllowed {
		t.Fatalf("expected the request to reach the gateway handler (not rejected at auth/routing), got %d, body: %s", w.Code, w.Body.String())
	}
	assertClaudeEnvelope(t, w.Body.Bytes())
}

// TestResponsesRouteReachesGatewayWithValidKey proves POST /v1/responses is
// registered on the same protected /v1 group as /v1/chat/completions and
// /v1/messages (inherits BodySizeLimit + APIKeyAuth + the bodies-dir
// stash) and actually dispatches into the gateway handler for a caller with
// a valid key. The Responses ingress body decode itself isn't wired up yet
// (that's a later task) -- peekIngress falls back to the OpenAI parse for
// any non-Claude ingress, so an empty JSON body fails its
// non-empty-messages check -- but that failure is itself proof the request
// reached Service.Handle rather than being rejected at auth or routing
// (401/404/405).
func TestResponsesRouteReachesGatewayWithValidKey(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-responses-route")
	r, err := New(testDeps(t, db))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Api-Key", "sk-yr-responses-route")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
		t.Fatalf("expected the request to reach the gateway handler (not rejected at auth/routing), got %d, body: %s", w.Code, w.Body.String())
	}
}

// TestGeminiGenerateContentRouteReachesGatewayWithValidKey is the Gemini
// counterpart of TestMessagesRouteReachesGatewayWithValidKey: it proves
// POST /v1beta/models/{model}:generateContent is registered on a sibling
// group carrying the identical middleware chain as /v1 (not mounted bare on
// the engine, which would skip auth/body-limit/bodies-dir), and that gin's
// ":modelaction" parameter correctly matches a path segment containing a
// colon. No provider/model is configured in this test, and the Gemini
// ingress now resolves its external model name from the URL path itself
// (not the body -- see parseGeminiPath/peekGeminiIngress), so the request
// reaches Service.Handle, resolves the model name from the path, and
// gets a legitimate gateway-generated 404 "model does not exist" for that
// unconfigured model -- not a routing failure (401/405), which is what this
// test asserts.
func TestGeminiGenerateContentRouteReachesGatewayWithValidKey(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-gemini-route")
	r, err := New(testDeps(t, db))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.0-flash:generateContent", bytes.NewReader([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)))
	req.Header.Set("X-Api-Key", "sk-yr-gemini-route")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusMethodNotAllowed {
		t.Fatalf("expected the request to reach the gateway handler (not rejected at auth/routing), got %d, body: %s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 model-not-found from the gateway (no model configured), got %d, body: %s", w.Code, w.Body.String())
	}
}

// TestGeminiRouteWithoutAPIKeyIsUnauthorized guards the security property
// the sibling /v1beta group exists for: /v1beta must carry the same
// APIKeyAuth chain as /v1, not be mounted bare on the engine (which would
// skip auth entirely).
func TestGeminiRouteWithoutAPIKeyIsUnauthorized(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.0-flash:generateContent", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unauthenticated gemini request, got %d, body: %s", w.Code, w.Body.String())
	}
}

// TestGeminiRouteWithSlashInModelSegmentDoesNotRoute documents (and locks
// in) an actual limitation of the /v1beta/models/:modelaction route: a model
// segment containing a percent-encoded slash, like a tuned model's
// "tunedModels/xyz" resource name, does NOT reach the gateway handler
// through real HTTP routing, unlike gateway.parseGeminiPath's own unit tests
// might suggest (parseGeminiPath happily URL-decodes a "%2F" it is handed
// directly). The reason is upstream of parseGeminiPath entirely: net/http
// decodes "%2F" into a literal "/" in req.URL.Path before gin ever sees the
// request (verified directly against httptest.NewRequest — RawPath keeps the
// original encoding, but gin's default router matches against the decoded
// Path, not RawPath, since UseRawPath is never enabled here), so the
// registered ":modelaction" param — which gin's tree matches against a
// single "/"-delimited path segment — never matches the resulting two-segment
// path at all. The request 404s at the routing layer, never reaching
// parseGeminiPath. See parseGeminiPath's doc comment for the same note from
// the decode side.
func TestGeminiRouteWithSlashInModelSegmentDoesNotRoute(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/tunedModels%2Fmy-model:generateContent", bytes.NewReader([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// No API key is set on purpose: if this ever DID route through to the
	// gateway handler, it would 401 (unauthenticated) rather than 404. What
	// this test pins is the routing layer itself never matching the path at
	// all -- both a plain SPA-fallback 404 and a 401 would be suspicious
	// here, but a 401 in particular would mean the route silently started
	// matching a slashed segment, which is the one thing this test exists to
	// catch.
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (route does not match a slash-containing model segment), got %d, body: %s", w.Code, w.Body.String())
	}
}

// TestMessagesWrongMethodReturnsClaudeEnvelope drives the NoMethod path for
// /v1/messages specifically: since /v1/messages is only registered for POST,
// a GET must 405 with the Anthropic-native envelope, not the OpenAI shape
// every other /v1/* path uses.
func TestMessagesWrongMethodReturnsClaudeEnvelope(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	assertClaudeEnvelope(t, w.Body.Bytes())
}

// TestGeminiWrongMethodReturnsGeminiEnvelope is
// TestMessagesWrongMethodReturnsClaudeEnvelope's Gemini counterpart: since
// /v1beta/models/:modelaction is only registered for POST, a GET must 405
// with the Gemini-native envelope (nested code/message/status, no top-level
// request_id) — proving IsGatewayNamespace's /v1beta admission plus
// WriteNamespacedError's Gemini branch are wired all the way through the
// real router, not just at the middleware-unit level.
func TestGeminiWrongMethodReturnsGeminiEnvelope(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1beta/models/gemini-2.0-flash:generateContent", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d, body: %s", w.Code, w.Body.String())
	}
	assertGeminiEnvelope(t, w.Body.Bytes())
}

// TestUnknownV1BetaPathReturns404 guards the NoRoute path specifically (not
// NoMethod): an unmatched /v1beta/* path must still be dispatched through
// the gateway namespace (not fall through to the SPA/admin 404), proving
// IsGatewayNamespace's /v1beta admission covers NoRoute too.
func TestUnknownV1BetaPathReturns404(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1beta/does-not-exist", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d, body: %s", w.Code, w.Body.String())
	}
}

// TestChatCompletionsWrongMethodReturnsOpenAIEnvelope is
// TestMessagesWrongMethodReturnsClaudeEnvelope's OpenAI-ingress counterpart,
// now driven against the real registered route (not the synthetic
// "/v1/test-only-route" TestV1WrongMethodReturnsOpenAICompatibleEnvelope
// uses) — locks in that adding the Claude branch did not flip
// /v1/chat/completions's own wrong-method shape.
func TestChatCompletionsWrongMethodReturnsOpenAIEnvelope(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	assertGatewayEnvelope(t, w.Body.Bytes(), "method_not_allowed")
}

// TestMessagesSubPathReturns404OpenAIEnvelope guards the exact-path-match
// semantics of gateway.IngressProtocol: only the literal "/v1/messages"
// path maps to the Claude ingress, so a 404 for anything under it (a
// messages-like /v1/... shape that never matched a route) must still fall
// back to the OpenAI envelope shape, not silently switch protocols.
func TestMessagesSubPathReturns404OpenAIEnvelope(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/messages/nonexistent", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	assertGatewayEnvelope(t, w.Body.Bytes(), "route_not_found")
}

// TestOversizedAdminRequestReturns413Envelope is the integration path
// TestBodySizeLimitReturns413Envelope (internal/middleware/middleware_test.go)
// can't cover: that test hand-writes the 413 response itself against a
// bare test route, so it never exercises whether a REAL /api/admin handler
// (bound to middleware.BodySizeLimit(1<<20) via this router, see
// newWithDistFS) actually maps the resulting *http.MaxBytesError to the
// same envelope instead of a generic 400 (see internal/handler's bindJSON).
func TestOversizedAdminRequestReturns413Envelope(t *testing.T) {
	r := newTestRouter(t)

	oversized := bytes.Repeat([]byte("a"), (1<<20)+1) // valid JSON string content, just too big
	body := append([]byte(`{"username":"admin","password":"`), oversized...)
	body = append(body, []byte(`"}`)...)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d, body: %s", w.Code, w.Body.String())
	}
	assertAdminEnvelope(t, w.Body.Bytes(), errcode.RequestEntityTooLarge)
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

// assertClaudeEnvelope checks the Anthropic-native error shape: a top-level
// "type":"error" discriminator plus "request_id", and a nested
// error.type/error.message — the shape /v1/messages callers expect,
// distinct from assertGatewayEnvelope's OpenAI shape above.
func assertClaudeEnvelope(t *testing.T, body []byte) {
	t.Helper()
	var env struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Error     struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("expected Claude-style error envelope JSON, got unparseable body %s: %v", body, err)
	}
	if env.Type != "error" {
		t.Fatalf(`expected top-level "type":"error", got %q (body: %s)`, env.Type, body)
	}
	if env.RequestID == "" {
		t.Fatalf("expected non-empty top-level request_id, body: %s", body)
	}
	if env.Error.Type == "" || env.Error.Message == "" {
		t.Fatalf("expected non-empty error.type and error.message, body: %s", body)
	}
}

// assertGeminiEnvelope checks the Gemini-native error shape: a single nested
// "error" object carrying code/message/status, with no top-level request_id
// field (unlike assertClaudeEnvelope) — the shape /v1beta callers expect.
func assertGeminiEnvelope(t *testing.T, body []byte) {
	t.Helper()
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("expected Gemini-style error envelope JSON, got unparseable body %s: %v", body, err)
	}
	if env.Error.Code == 0 {
		t.Fatalf("expected non-zero error.code, body: %s", body)
	}
	if env.Error.Message == "" || env.Error.Status == "" {
		t.Fatalf("expected non-empty error.message and error.status, body: %s", body)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(body, &raw)
	if _, ok := raw["request_id"]; ok {
		t.Fatalf("must not leak a top-level request_id field, got: %s", body)
	}
}
