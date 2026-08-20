package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// newIngressTestContext builds a gin test context wired to a real
// httptest.ResponseRecorder, mirroring the pattern used by
// TestWriteIngressError_OpenAIStashesResponseBody in error_test.go.
func newIngressTestContext(t *testing.T, path string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return c, w
}

// TestWriteIngressError_ClaudeEnvelopeDetails confirms the Claude ingress
// gets the Anthropic-native shape: top-level type + request_id, and a nested
// error.type/error.message — not the OpenAI-shaped nested-only envelope.
func TestWriteIngressError_ClaudeEnvelopeDetails(t *testing.T) {
	c, w := newIngressTestContext(t, "/v1/messages")

	WriteIngressError(c, protocols.ProtocolClaude, http.StatusTooManyRequests, errTypeRateLimit, "too many requests", "req_abc")

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body did not unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if body["type"] != "error" {
		t.Errorf(`top-level "type" = %v, want "error"`, body["type"])
	}
	if body["request_id"] != "req_abc" {
		t.Errorf(`top-level "request_id" = %v, want "req_abc"`, body["request_id"])
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf(`"error" field is not an object: %v`, body["error"])
	}
	if errObj["type"] != "rate_limit_error" {
		t.Errorf(`error.type = %v, want "rate_limit_error"`, errObj["type"])
	}
	if errObj["message"] != "too many requests" {
		t.Errorf(`error.message = %v, want "too many requests"`, errObj["message"])
	}
	// The message must NOT have the request id appended (unlike the OpenAI
	// path) — the id travels only in the top-level request_id field.
	if strings.Contains(errObj["message"].(string), "req_abc") {
		t.Errorf("error.message = %v, must not contain the request id", errObj["message"])
	}
}

// TestWriteIngressError_StashesResponseBody: when an Exchange is on the gin
// context, the error JSON actually sent to the caller must also be stashed
// into rc.ResponseBody() so the audit trail matches.
func TestWriteIngressError_StashesResponseBody(t *testing.T) {
	c, w := newIngressTestContext(t, "/v1/messages")
	rc := &Exchange{requestID: "req_x"}
	c.Set(relayContextKey, rc)

	WriteIngressError(c, protocols.ProtocolClaude, http.StatusNotFound, errTypeNotFound, "model does not exist", "req_x")

	if rc.ResponseBody() == nil {
		t.Fatal("rc.ResponseBody() was not stashed")
	}
	if string(rc.ResponseBody()) != w.Body.String() {
		t.Errorf("ResponseBody() = %s, want it to equal the sent body %s", rc.ResponseBody(), w.Body.String())
	}
}

// TestWriteIngressError_NoExchangeIsNoop confirms the stash is a true
// no-op (no panic) when no Exchange is on the context.
func TestWriteIngressError_NoExchangeIsNoop(t *testing.T) {
	c, w := newIngressTestContext(t, "/v1/messages")

	WriteIngressError(c, protocols.ProtocolClaude, http.StatusUnauthorized, errTypeAuthentication, "missing API key", "req_y")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestWriteIngressError_Claude confirms WriteIngressError routes
// /v1/messages traffic to the Anthropic envelope, with errType mapped to
// Anthropic's vocabulary and the message left untouched (id not appended).
func TestWriteIngressError_Claude(t *testing.T) {
	c, w := newIngressTestContext(t, "/v1/messages")

	WriteIngressError(c, protocols.ProtocolClaude, http.StatusForbidden, errTypePermission, "not allowed", "req_claude")

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body did not unmarshal: %v", err)
	}
	if body["type"] != "error" || body["request_id"] != "req_claude" {
		t.Errorf("body = %v, want top-level type=error, request_id=req_claude", body)
	}
	errObj := body["error"].(map[string]any)
	if errObj["type"] != "permission_error" {
		t.Errorf("error.type = %v, want permission_error", errObj["type"])
	}
	if errObj["message"] != "not allowed" {
		t.Errorf("error.message = %v, want unchanged message %q", errObj["message"], "not allowed")
	}
}

// TestWriteIngressError_OpenAI confirms WriteIngressError keeps the existing
// OpenAI-compatible behavior for any non-Claude ingress: nested-only
// envelope, id appended into message.
func TestWriteIngressError_OpenAI(t *testing.T) {
	c, w := newIngressTestContext(t, "/v1/chat/completions")

	WriteIngressError(c, protocols.ProtocolOpenAI, http.StatusNotFound, errTypeNotFound, "model does not exist", "req_openai")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body did not unmarshal: %v", err)
	}
	if _, hasTopLevelType := body["type"]; hasTopLevelType {
		t.Errorf("body has an OpenAI-illegal top-level %q field: %v", "type", body)
	}
	if _, hasTopLevelRequestID := body["request_id"]; hasTopLevelRequestID {
		t.Errorf("body has an OpenAI-illegal top-level %q field: %v", "request_id", body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf(`"error" field is not an object: %v`, body["error"])
	}
	if errObj["type"] != errTypeNotFound {
		t.Errorf("error.type = %v, want %q", errObj["type"], errTypeNotFound)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "model does not exist") || !strings.Contains(msg, "req_openai") {
		t.Errorf("error.message = %q, want it to contain both the message and the request id", msg)
	}
}

// TestWriteIngressError_SetsRequestIDHeader confirms both ingresses leave
// the caller-visible request id on the response header the RequestID
// middleware already establishes (X-Request-Id), so SDKs can surface it
// even from a locally-generated error.
func TestWriteIngressError_SetsRequestIDHeader(t *testing.T) {
	cases := []struct {
		name    string
		ingress protocols.ProtocolID
		path    string
	}{
		{"claude", protocols.ProtocolClaude, "/v1/messages"},
		{"openai", protocols.ProtocolOpenAI, "/v1/chat/completions"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newIngressTestContext(t, tt.path)
			WriteIngressError(c, tt.ingress, http.StatusInternalServerError, errTypeServer, "boom", "req_hdr_"+tt.name)

			got := w.Header().Get("X-Request-Id")
			if got != "req_hdr_"+tt.name {
				t.Errorf("X-Request-Id header = %q, want %q", got, "req_hdr_"+tt.name)
			}
		})
	}
}

// TestLocalIngressErrorBody_MatchesSentBody is the audit-parity
// requirement: LocalIngressErrorBody must byte-for-byte equal what
// WriteIngressError actually put on the wire, for both ingresses.
func TestLocalIngressErrorBody_MatchesSentBody(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		c, w := newIngressTestContext(t, "/v1/messages")
		WriteIngressError(c, protocols.ProtocolClaude, http.StatusBadRequest, errTypeInvalidRequest, "bad body", "req_1")

		got := LocalIngressErrorBody(protocols.ProtocolClaude, http.StatusBadRequest, errTypeInvalidRequest, "bad body", "req_1")
		if string(got) != w.Body.String() {
			t.Errorf("LocalIngressErrorBody = %s, want it to equal sent body %s", got, w.Body.String())
		}
	})

	t.Run("openai", func(t *testing.T) {
		c, w := newIngressTestContext(t, "/v1/chat/completions")
		WriteIngressError(c, protocols.ProtocolOpenAI, http.StatusBadRequest, errTypeInvalidRequest, "bad body", "req_2")

		// The builder is authoritative for the OpenAI dialect's id-in-message
		// rule: the raw message goes in on both sides and the bytes match.
		got := LocalIngressErrorBody(protocols.ProtocolOpenAI, http.StatusBadRequest, errTypeInvalidRequest, "bad body", "req_2")
		if string(got) != w.Body.String() {
			t.Errorf("LocalIngressErrorBody = %s, want it to equal sent body %s", got, w.Body.String())
		}
	})
}

// TestLocalIngressErrorBody_ClaudeOverloadedShape pins one more Claude
// mapping through the audit-parity path: service_unavailable surfaces as
// Anthropic's overloaded_error, and the audit bytes equal the sent bytes.
func TestLocalIngressErrorBody_ClaudeOverloadedShape(t *testing.T) {
	c, w := newIngressTestContext(t, "/v1/messages")
	WriteIngressError(c, protocols.ProtocolClaude, http.StatusServiceUnavailable, errTypeUnavailable, "overloaded", "req_3")

	got := LocalIngressErrorBody(protocols.ProtocolClaude, http.StatusServiceUnavailable, errTypeUnavailable, "overloaded", "req_3")
	if string(got) != w.Body.String() {
		t.Errorf("LocalIngressErrorBody = %s, want it to equal sent body %s", got, w.Body.String())
	}
}

// TestWriteIngressError_GeminiEnvelopeDetails confirms the Gemini ingress
// gets the Google API error shape: a single nested "error" object carrying
// code/message/status, with no top-level request_id field (unlike
// Anthropic's envelope) and the request id only on the X-Request-Id header.
func TestWriteIngressError_GeminiEnvelopeDetails(t *testing.T) {
	c, w := newIngressTestContext(t, "/v1beta/models/gemini-2.0-flash:generateContent")

	WriteIngressError(c, protocols.ProtocolGemini, http.StatusTooManyRequests, errTypeRateLimit, "too many requests", "req_gemini")

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
	if got := w.Header().Get("X-Request-Id"); got != "req_gemini" {
		t.Errorf("X-Request-Id header = %q, want %q", got, "req_gemini")
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body did not unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if _, hasTopLevelRequestID := body["request_id"]; hasTopLevelRequestID {
		t.Errorf("body has a top-level request_id field, want none: %v", body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf(`"error" field is not an object: %v`, body["error"])
	}
	if code, _ := errObj["code"].(float64); int(code) != http.StatusTooManyRequests {
		t.Errorf("error.code = %v, want %d", errObj["code"], http.StatusTooManyRequests)
	}
	if errObj["message"] != "too many requests" {
		t.Errorf("error.message = %v, want %q", errObj["message"], "too many requests")
	}
	if errObj["status"] != "RESOURCE_EXHAUSTED" {
		t.Errorf("error.status = %v, want RESOURCE_EXHAUSTED", errObj["status"])
	}
	// The request id must NOT leak into the message (it only ever travels on
	// the header for Gemini).
	if strings.Contains(errObj["message"].(string), "req_gemini") {
		t.Errorf("error.message = %v, must not contain the request id", errObj["message"])
	}
}

// TestWriteIngressError_Gemini is the Gemini counterpart of
// TestWriteIngressError_Claude/_OpenAI: confirms WriteIngressError routes a
// /v1beta native Gemini path to the Gemini envelope across the status codes
// the task calls out (401/400/404/429/502), with the right canonical status
// string and the code field mirroring the HTTP status.
func TestWriteIngressError_Gemini(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantStatus string
	}{
		{"unauthorized", http.StatusUnauthorized, "UNAUTHENTICATED"},
		{"bad_request", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"not_found", http.StatusNotFound, "NOT_FOUND"},
		{"too_many_requests", http.StatusTooManyRequests, "RESOURCE_EXHAUSTED"},
		{"bad_gateway", http.StatusBadGateway, "INTERNAL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := newIngressTestContext(t, "/v1beta/models/gemini-2.0-flash:generateContent")
			WriteIngressError(c, protocols.ProtocolGemini, tt.status, errTypeInvalidRequest, "upstream failed", "req_"+tt.name)

			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if got := w.Header().Get("X-Request-Id"); got != "req_"+tt.name {
				t.Errorf("X-Request-Id header = %q, want %q", got, "req_"+tt.name)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body did not unmarshal: %v (body=%s)", err, w.Body.String())
			}
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf(`"error" field is not an object: %v`, body["error"])
			}
			if code, _ := errObj["code"].(float64); int(code) != tt.status {
				t.Errorf("error.code = %v, want %d", errObj["code"], tt.status)
			}
			if errObj["status"] != tt.wantStatus {
				t.Errorf("error.status = %v, want %q", errObj["status"], tt.wantStatus)
			}
			if errObj["message"] != "upstream failed" {
				t.Errorf("error.message = %v, want unchanged message %q", errObj["message"], "upstream failed")
			}
		})
	}
}

// TestLocalIngressErrorBody_GeminiMatchesSentBody is the Gemini counterpart
// of TestLocalIngressErrorBody_MatchesSentBody: LocalIngressErrorBody must
// byte-for-byte equal what WriteIngressError actually put on the wire for the
// Gemini ingress.
func TestLocalIngressErrorBody_GeminiMatchesSentBody(t *testing.T) {
	c, w := newIngressTestContext(t, "/v1beta/models/gemini-2.0-flash:generateContent")
	WriteIngressError(c, protocols.ProtocolGemini, http.StatusNotFound, errTypeNotFound, "model does not exist", "req_gemini_4")

	got := LocalIngressErrorBody(protocols.ProtocolGemini, http.StatusNotFound, errTypeNotFound, "model does not exist", "req_gemini_4")
	if string(got) != w.Body.String() {
		t.Errorf("LocalIngressErrorBody = %s, want it to equal sent body %s", got, w.Body.String())
	}
}

// TestWriteIngressError_Responses confirms the Responses ingress reuses the
// OpenAI-compatible error envelope verbatim (no Responses-specific writer):
// the Responses API's non-stream error shape is the same
// {"error":{message,type}} nested object OpenAI Chat uses, so
// WriteIngressError's default branch already produces the correct wire body
// — this test pins that reuse as intentional, not an oversight.
func TestWriteIngressError_Responses(t *testing.T) {
	c, w := newIngressTestContext(t, "/v1/responses")

	WriteIngressError(c, protocols.ProtocolResponses, http.StatusBadRequest, errTypeInvalidRequest, "invalid input", "req_responses")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body did not unmarshal: %v", err)
	}
	if _, hasTopLevelType := body["type"]; hasTopLevelType {
		t.Errorf("body has an OpenAI-illegal top-level %q field: %v", "type", body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf(`"error" field is not an object: %v`, body["error"])
	}
	if errObj["type"] != errTypeInvalidRequest {
		t.Errorf("error.type = %v, want %q", errObj["type"], errTypeInvalidRequest)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "invalid input") || !strings.Contains(msg, "req_responses") {
		t.Errorf("error.message = %q, want it to contain both the message and the request id", msg)
	}
}
