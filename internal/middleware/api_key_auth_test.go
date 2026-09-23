package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/loopback"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

func newAuthContext(t *testing.T, authHeader string) *gin.Context {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if authHeader != "" {
		c.Request.Header.Set("Authorization", authHeader)
	}
	return c
}

// TestExtractBearerKey locks in the RFC 7235 scheme handling: case-
// insensitive scheme, whitespace/tab separator tolerated, and the
// "bearerXYZ" typo rejected as malformed (not parsed as token "XYZ").
func TestExtractBearerKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"standard", "Bearer sk-yr-abc", "sk-yr-abc"},
		{"lowercase scheme", "bearer sk-yr-abc", "sk-yr-abc"},
		{"uppercase scheme", "BEARER sk-yr-abc", "sk-yr-abc"},
		{"mixed case", "BeArEr sk-yr-abc", "sk-yr-abc"},
		{"tab separator", "Bearer\tsk-yr-abc", "sk-yr-abc"},
		{"extra spaces collapsed", "Bearer   sk-yr-abc", "sk-yr-abc"},
		{"missing header", "", ""},
		{"wrong scheme", "Basic sk-yr-abc", ""},
		{"bearerXYZ typo rejected", "bearerXYZ", ""},
		{"bearerX-with-token rejected", "bearerXYZ sk-yr-abc", ""},
		{"just bearer", "Bearer", ""},
		{"bearer trailing space", "Bearer ", ""},
		{"token with internal spaces preserved", "Bearer sk yr abc", "sk yr abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newAuthContext(t, tt.input)
			if got := extractBearerKey(c); got != tt.want {
				t.Errorf("extractBearerKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestAuthRejectionWritesBodyRow: a request rejected at
// the auth gate (missing / unknown key) never reaches gateway.Handle, so its
// finalize() never runs — without logAuthRejection also writing the
// request_log_bodies row, both bodies would be permanently unrecorded for
// this whole failure class. Covers both the audit row (request_logs, already
// tested indirectly elsewhere) and the new body row.
func TestAuthRejectionWritesBodyRow(t *testing.T) {
	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	cases := []struct {
		name       string
		authHeader string
		wantReason string
	}{
		{"missing key", "", "missing API key"},
		{"unknown key", "Bearer sk-yr-does-not-exist", "invalid API key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db := testutil.NewSQLiteDB(t)
			r := gin.New()
			r.Use(RequestID())
			r.POST("/v1/chat/completions", APIKeyAuth(db), func(c *gin.Context) {
				c.Status(http.StatusOK) // never reached — auth always rejects here
			})

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			requestID := w.Header().Get("X-Request-Id")
			if requestID == "" {
				t.Fatal("X-Request-Id header not set by RequestID middleware")
			}

			var auditRow model.RequestLog
			if err := db.Where("request_id = ?", requestID).First(&auditRow).Error; err != nil {
				t.Fatalf("expected a request_logs audit row: %v", err)
			}
			if auditRow.APIKeyID != nil {
				t.Errorf("audit row api_key_id = %v, want nil for an auth rejection", *auditRow.APIKeyID)
			}
			if auditRow.StatusCode != http.StatusUnauthorized {
				t.Errorf("audit row status_code = %d, want 401", auditRow.StatusCode)
			}

			bodyRow, err := repository.GetRequestLogBodyByRequestID(db, requestID)
			if err != nil {
				t.Fatalf("GetRequestLogBodyByRequestID: %v", err)
			}
			if bodyRow == nil {
				t.Fatal("expected a request_log_bodies row for the auth rejection")
			}
			if !bytes.Contains([]byte(bodyRow.RequestBody), []byte(`"model":"gpt-4o"`)) {
				t.Errorf("body.request_body = %q, want the caller's request body", bodyRow.RequestBody)
			}
			if !bytes.Contains([]byte(bodyRow.ResponseBody), []byte(tc.wantReason)) {
				t.Errorf("body.response_body = %q, want it to contain %q", bodyRow.ResponseBody, tc.wantReason)
			}
			if !bytes.Contains([]byte(bodyRow.ResponseBody), []byte("authentication_error")) {
				t.Errorf("body.response_body = %q, want the authentication_error type", bodyRow.ResponseBody)
			}
		})
	}
}

// seedAPIKey inserts one active APIKey row keyed to rawKey and returns it,
// mirroring the pattern gateway/relay_test.go uses for its own fixtures.
func seedAPIKey(t *testing.T, db *gorm.DB, rawKey string) *model.APIKey {
	t.Helper()
	now := time.Now().UTC()
	prefixLen := len(rawKey)
	if prefixLen > 12 {
		prefixLen = 12
	}
	// Every key needs an enabled owning account: APIKeyAuth checks the
	// owner's status before admitting the credential, so an ownerless key
	// would 500 on the status lookup rather than authenticate.
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

// newAuthRouter builds a gin engine with RequestID + APIKeyAuth mounted on
// path, its next handler responding 200 so a passing request is
// distinguishable from a rejected one.
// newAuthRouterFor builds a gin engine with RequestID + APIKeyAuth mounted on
// method+path, its next handler responding 200 so a passing request is
// distinguishable from a rejected one. Shared by the POST (newAuthRouter)
// and GET (newAuthRouterGET) variants.
func newAuthRouterFor(db *gorm.DB, method, path string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Handle(method, path, APIKeyAuth(db), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return r
}

func newAuthRouter(db *gorm.DB, path string) *gin.Engine {
	return newAuthRouterFor(db, http.MethodPost, path)
}

// TestAPIKeyAuth_XAPIKeyHeader confirms the Anthropic SDK's X-Api-Key header
// authenticates a caller on /v1/messages, without any Authorization header —
// this is the whole point of the task: Claude Code / the Anthropic SDK sends
// exactly this header and nothing else.
func TestAPIKeyAuth_XAPIKeyHeader(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-anthropic-sdk")
	r := newAuthRouter(db, "/v1/messages")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		bytes.NewReader([]byte(`{"model":"claude-3-opus","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("X-Api-Key", "sk-yr-anthropic-sdk")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (authenticated); body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_BearerStillWorks locks in that adding X-Api-Key support did
// not regress the original Authorization: Bearer path.
func TestAPIKeyAuth_BearerStillWorks(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-bearer-ok")
	r := newAuthRouter(db, "/v1/messages")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer sk-yr-bearer-ok")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (authenticated); body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_ConflictingHeaders confirms that presenting both headers
// with different values is rejected outright rather than silently preferring
// one — a mismatch almost certainly means the caller sent a stale credential
// in one of the two headers, and picking a winner would hide that.
func TestAPIKeyAuth_ConflictingHeaders(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-bearer-value")
	seedAPIKey(t, db, "sk-yr-header-value")
	r := newAuthRouter(db, "/v1/messages")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer sk-yr-bearer-value")
	req.Header.Set("X-Api-Key", "sk-yr-header-value")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for conflicting headers; body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_ConflictingHeaders_SameValue confirms both headers present
// with the SAME value is accepted (not treated as a conflict).
func TestAPIKeyAuth_ConflictingHeaders_SameValue(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-same-value")
	r := newAuthRouter(db, "/v1/messages")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer sk-yr-same-value")
	req.Header.Set("X-Api-Key", "sk-yr-same-value")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for identical headers; body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_MissingKey_IngressAwareEnvelope confirms the auth-rejection
// error envelope matches the ingress: /v1/messages gets the Anthropic
// envelope (top-level "type":"error" + "request_id"), /v1/chat/completions
// keeps the existing OpenAI nested-only envelope.
func TestAPIKeyAuth_MissingKey_IngressAwareEnvelope(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		claude bool
	}{
		{"claude ingress", "/v1/messages", true},
		{"openai ingress", "/v1/chat/completions", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			r := newAuthRouter(db, tc.path)

			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}

			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body did not unmarshal: %v (body=%s)", err, w.Body.String())
			}
			_, hasTopType := body["type"]
			_, hasTopRequestID := body["request_id"]
			if tc.claude {
				if !hasTopType || !hasTopRequestID {
					t.Errorf("claude ingress: body = %v, want top-level type + request_id", body)
				}
			} else {
				if hasTopType || hasTopRequestID {
					t.Errorf("openai ingress: body = %v, want no top-level type/request_id", body)
				}
				errObj, ok := body["error"].(map[string]any)
				if !ok {
					t.Fatalf("openai ingress: body missing nested error object: %v", body)
				}
				if errObj["type"] != "authentication_error" {
					t.Errorf("openai ingress: error.type = %v, want authentication_error", errObj["type"])
				}
			}
		})
	}
}

// newAuthRouterGET is newAuthRouter for GET routes — the /v1/models discovery
// endpoints, which the POST-only newAuthRouter cannot mount.
func newAuthRouterGET(db *gorm.DB, path string) *gin.Engine {
	return newAuthRouterFor(db, http.MethodGet, path)
}

// TestAPIKeyAuth_ModelsDiscovery_HeaderAwareEnvelope confirms the auth-reject
// envelope on /v1/models follows the caller's wire protocol, detected via the
// anthropic-version header. IngressProtocol is path-based and maps every
// /v1/models request to OpenAI, so without header awareness an Anthropic SDK
// caller (which sends anthropic-version) would get an OpenAI-shaped 401
// instead of the Claude envelope it expects. The OpenAI caller keeps the
// OpenAI envelope.
func TestAPIKeyAuth_ModelsDiscovery_HeaderAwareEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		anthropic  bool
		wantClaude bool
	}{
		{"anthropic SDK sends anthropic-version", true, true},
		{"openai client omits it", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			r := newAuthRouterGET(db, "/v1/models")

			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tc.anthropic {
				req.Header.Set("anthropic-version", "2023-06-01")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body did not unmarshal: %v (body=%s)", err, w.Body.String())
			}
			_, hasTopType := body["type"]
			_, hasTopRequestID := body["request_id"]
			if tc.wantClaude {
				if !hasTopType || !hasTopRequestID {
					t.Errorf("anthropic caller: body=%v, want top-level type + request_id", body)
				}
			} else {
				if hasTopType || hasTopRequestID {
					t.Errorf("openai caller: body=%v, want no top-level type/request_id", body)
				}
			}
		})
	}
}

// geminiIngressPath is a native Gemini generateContent path, matching
// gateway.IngressProtocol's prefix+suffix rule so these tests exercise the
// Gemini ingress branch of resolveAPIKey.
const geminiIngressPath = "/v1beta/models/gemini-1.5-pro:generateContent"

// geminiUnknownActionPath is the same surface with an action name neither
// IngressProtocol nor parseGeminiPath recognizes: protocol classification
// falls back to OpenAI, but the path is still on the Gemini surface, so the
// Google GenAI SDK's credential sources must still be honored — otherwise a
// typo'd method name answers a misleading 401 "missing API key" instead of
// the handler's unknown-route 404.
const geminiUnknownActionPath = "/v1beta/models/gemini-1.5-pro:streamGenerateReply"

// TestAPIKeyAuth_GeminiGoogHeader confirms the Google GenAI SDK's
// x-goog-api-key header authenticates a caller on the Gemini ingress.
func TestAPIKeyAuth_GeminiGoogHeader(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-goog-header")
	r := newAuthRouter(db, geminiIngressPath)

	req := httptest.NewRequest(http.MethodPost, geminiIngressPath, nil)
	req.Header.Set("x-goog-api-key", "sk-yr-goog-header")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (authenticated); body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_GeminiQueryKey confirms the ?key= query parameter
// authenticates a caller on the Gemini ingress.
func TestAPIKeyAuth_GeminiQueryKey(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-goog-query")
	r := newAuthRouter(db, geminiIngressPath)

	req := httptest.NewRequest(http.MethodPost, geminiIngressPath+"?key=sk-yr-goog-query", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (authenticated); body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_GeminiGoogHeaderAndQuery_SameValue confirms both Gemini
// sources present with the SAME value is accepted, not treated as a
// conflict.
func TestAPIKeyAuth_GeminiGoogHeaderAndQuery_SameValue(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-goog-both-same")
	r := newAuthRouter(db, geminiIngressPath)

	req := httptest.NewRequest(http.MethodPost, geminiIngressPath+"?key=sk-yr-goog-both-same", nil)
	req.Header.Set("x-goog-api-key", "sk-yr-goog-both-same")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for identical sources; body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_GeminiGoogHeaderAndQuery_Conflict confirms x-goog-api-key
// and ?key= carrying DIFFERENT values is rejected as a conflict.
func TestAPIKeyAuth_GeminiGoogHeaderAndQuery_Conflict(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-goog-a")
	seedAPIKey(t, db, "sk-yr-goog-b")
	r := newAuthRouter(db, geminiIngressPath)

	req := httptest.NewRequest(http.MethodPost, geminiIngressPath+"?key=sk-yr-goog-b", nil)
	req.Header.Set("x-goog-api-key", "sk-yr-goog-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for conflicting Gemini sources; body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_GeminiGoogHeaderConflictsWithBearer confirms x-goog-api-key
// disagreeing with Authorization: Bearer is rejected as a conflict too —
// the conflict rule applies across all applicable sources, not just the
// two Gemini-specific ones.
func TestAPIKeyAuth_GeminiGoogHeaderConflictsWithBearer(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-bearer-side")
	seedAPIKey(t, db, "sk-yr-goog-side")
	r := newAuthRouter(db, geminiIngressPath)

	req := httptest.NewRequest(http.MethodPost, geminiIngressPath, nil)
	req.Header.Set("Authorization", "Bearer sk-yr-bearer-side")
	req.Header.Set("x-goog-api-key", "sk-yr-goog-side")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for x-goog-api-key vs Bearer conflict; body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_GeminiMissingAllSources confirms the Gemini ingress still
// rejects a request that carries none of the four applicable sources.
func TestAPIKeyAuth_GeminiMissingAllSources(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthRouter(db, geminiIngressPath)

	req := httptest.NewRequest(http.MethodPost, geminiIngressPath, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a keyless Gemini request; body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_QueryKeyRejectedOffGeminiIngress confirms the ?key= query
// parameter is NOT accepted as a credential source on a non-Gemini
// ingress — an OpenAI caller must not be able to authenticate via a query
// parameter just because the Gemini ingress happens to allow one.
func TestAPIKeyAuth_QueryKeyRejectedOffGeminiIngress(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-openai-query-attempt")
	r := newAuthRouter(db, "/v1/chat/completions")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key=sk-yr-openai-query-attempt", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: ?key= must not authenticate the OpenAI ingress; body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_GeminiGoogSources_UnknownAction confirms the two
// Google-GenAI-SDK credential sources still authenticate when the action
// name is unrecognized: the request stays on the Gemini surface, and what
// happens next (the gateway handler's unknown-route 404) is a routing
// answer — auth must not preempt it with a misleading "missing API key".
// Both subtests fail against the old protocol-classification gate, which
// sent these callers to 401 before routing could answer 404.
func TestAPIKeyAuth_GeminiGoogSources_UnknownAction(t *testing.T) {
	cases := []struct {
		name  string
		query bool
	}{
		{"x-goog-api-key header", false},
		{"?key= query parameter", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			seedAPIKey(t, db, "sk-yr-goog-unknown-action")
			r := newAuthRouter(db, geminiUnknownActionPath)

			target := geminiUnknownActionPath
			if tc.query {
				target += "?key=sk-yr-goog-unknown-action"
			}
			req := httptest.NewRequest(http.MethodPost, target, nil)
			if !tc.query {
				req.Header.Set("x-goog-api-key", "sk-yr-goog-unknown-action")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (authenticated on the Gemini surface despite unknown action); body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestAPIKeyAuth_BearerOnGeminiUnknownAction is the Bearer regression
// assertion for the surface-based gate: Authorization is accepted on every
// path, so an unknown-action Gemini request authenticated with Bearer keeps
// passing auth exactly as before (the handler then 404s the action).
func TestAPIKeyAuth_BearerOnGeminiUnknownAction(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAPIKey(t, db, "sk-yr-bearer-unknown-action")
	r := newAuthRouter(db, geminiUnknownActionPath)

	req := httptest.NewRequest(http.MethodPost, geminiUnknownActionPath, nil)
	req.Header.Set("Authorization", "Bearer sk-yr-bearer-unknown-action")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Bearer authenticates on any path); body=%s", w.Code, w.Body.String())
	}
}

// TestAPIKeyAuth_GeminiAuthRejection_QueryKeyNeverPersisted is the
// audit-leak guard: a Gemini request carrying ?key=<secret> must never have
// that secret land in the request_logs / request_log_bodies audit trail.
// logAuthRejection is the middleware's own persistence path (it runs
// whenever auth rejects, independent of gateway.Handle's finalize), and
// gateway.IngressProtocol plus every persistence site here key off
// c.Request.URL.Path only (never .RawQuery/.String()/.RequestURI) — so no
// query string, and therefore no key value, is ever stored. This test locks
// that in against regression.
func TestAPIKeyAuth_GeminiAuthRejection_QueryKeyNeverPersisted(t *testing.T) {
	const secret = "sk-yr-rejected-must-not-leak"
	db := testutil.NewSQLiteDB(t)
	r := newAuthRouter(db, geminiIngressPath)

	// No matching APIKey row seeded, so this is rejected as "invalid API key"
	// — the rejection path (logAuthRejection) still must not persist the
	// query-string secret anywhere.
	req := httptest.NewRequest(http.MethodPost, geminiIngressPath+"?key="+secret, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	requestID := w.Header().Get("X-Request-Id")

	var auditRow model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&auditRow).Error; err != nil {
		t.Fatalf("expected a request_logs audit row: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v", auditRow), secret) {
		t.Errorf("request_logs row contains the query-string secret: %+v", auditRow)
	}

	bodyRow, err := repository.GetRequestLogBodyByRequestID(db, requestID)
	if err != nil {
		t.Fatalf("GetRequestLogBodyByRequestID: %v", err)
	}
	if bodyRow == nil {
		t.Fatal("expected a request_log_bodies row for the auth rejection")
	}
	if strings.Contains(bodyRow.RequestHeaders, secret) ||
		strings.Contains(bodyRow.RequestBody, secret) ||
		strings.Contains(bodyRow.ResponseBody, secret) {
		t.Errorf("request_log_bodies row contains the query-string secret: %+v", bodyRow)
	}
}

// TestAPIKeyAuth_AuditBodyMatchesSentBody is the audit-parity requirement:
// the request_log_bodies.response_body row logAuthRejection writes for a
// rejected request must equal, byte for byte, what the caller actually
// received on the wire — for both ingresses.
func TestAPIKeyAuth_AuditBodyMatchesSentBody(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"claude ingress", "/v1/messages"},
		{"openai ingress", "/v1/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			r := newAuthRouter(db, tc.path)

			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			requestID := w.Header().Get("X-Request-Id")
			if requestID == "" {
				t.Fatal("X-Request-Id header not set")
			}

			bodyRow, err := repository.GetRequestLogBodyByRequestID(db, requestID)
			if err != nil {
				t.Fatalf("GetRequestLogBodyByRequestID: %v", err)
			}
			if bodyRow == nil {
				t.Fatal("expected a request_log_bodies row for the auth rejection")
			}
			if bodyRow.ResponseBody != w.Body.String() {
				t.Errorf("audit response_body = %q, want it to equal the sent body %q", bodyRow.ResponseBody, w.Body.String())
			}
		})
	}
}

// TestAuthRejectedSubCallCarriesSourceMarking: the auth gate is the second
// request_logs writer, and a rejected loopback sub-call must carry the same
// source/parent marking the gateway recorder would give it — otherwise it
// reads as a caller request in the log. A forged token gets nothing.
func TestAuthRejectedSubCallCarriesSourceMarking(t *testing.T) {
	cases := []struct {
		name       string
		token      string
		wantSource string
		wantParent string
	}{
		{"valid token", loopback.Token, model.RequestLogSourceVisionFallback, "parent-abc"},
		{"forged token", "forged-value", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db := testutil.NewSQLiteDB(t)
			r := gin.New()
			r.Use(RequestID())
			r.POST("/v1/chat/completions", APIKeyAuth(db), func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
			// No API key at all: rejected at the gate, exercising this writer.
			req.Header.Set(loopback.HeaderInternal, tc.token)
			req.Header.Set(loopback.HeaderParent, "parent-abc")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			var row model.RequestLog
			if err := db.Where("request_id = ?", w.Header().Get("X-Request-Id")).First(&row).Error; err != nil {
				t.Fatalf("audit row: %v", err)
			}
			if row.Source != tc.wantSource || row.ParentRequestID != tc.wantParent {
				t.Fatalf("source/parent = %q/%q, want %q/%q", row.Source, row.ParentRequestID, tc.wantSource, tc.wantParent)
			}
		})
	}
}
