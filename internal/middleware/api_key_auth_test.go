package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
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

// TestAuthRejectionWritesBodyRow (Task 7, Codex #2): a request rejected at
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
