package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestClassifyUpstreamStatus(t *testing.T) {
	tests := []struct {
		status   int
		category statusCategory
		outcome  string
		errType  string
	}{
		{http.StatusUnauthorized, statusRotateKey, AttemptAuthFailed, ""},
		{http.StatusTooManyRequests, statusRotateKey, AttemptRateLimited, ""},
		{http.StatusInternalServerError, statusFailover, AttemptServerError, ""},
		{http.StatusBadGateway, statusFailover, AttemptServerError, ""},
		{http.StatusServiceUnavailable, statusFailover, AttemptServerError, ""},
		{http.StatusBadRequest, statusTerminalClient, AttemptClientError, errTypeInvalidRequest},
		// 403 is terminal (provider-scoped), NOT a rotate-Key status.
		{http.StatusForbidden, statusTerminalClient, AttemptClientError, errTypePermission},
		{http.StatusNotFound, statusTerminalClient, AttemptClientError, errTypeNotFound},
		{http.StatusUnprocessableEntity, statusTerminalClient, AttemptClientError, errTypeInvalidRequest},
	}
	for _, tt := range tests {
		got := classifyUpstreamStatus(tt.status)
		if got.Category != tt.category {
			t.Errorf("status %d: category = %v, want %v", tt.status, got.Category, tt.category)
		}
		if got.Outcome != tt.outcome {
			t.Errorf("status %d: outcome = %q, want %q", tt.status, got.Outcome, tt.outcome)
		}
		if got.ErrorType != tt.errType {
			t.Errorf("status %d: errType = %q, want %q", tt.status, got.ErrorType, tt.errType)
		}
	}
}

// TestWriteOpenAIErrorStashesResponseBody (Task 7, Codex #1): when a
// RelayContext is on the gin context (as Handle installs it), the local
// error JSON WriteOpenAIErrorWithRequestID returns to the caller is also
// stashed into rc.ResponseBody, so request_log_bodies.response_body reflects
// what the caller actually received for a locally-rejected request.
func TestWriteOpenAIErrorStashesResponseBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	rc := &RelayContext{RequestID: "req_x"}
	c.Set(relayContextKey, rc)

	WriteOpenAIErrorWithRequestID(c, http.StatusNotFound, errTypeNotFound, "model does not exist", "req_x")

	if rc.ResponseBody == nil {
		t.Fatal("rc.ResponseBody was not stashed")
	}
	got := string(rc.ResponseBody)
	for _, want := range []string{`"message"`, `"type"`, "model does not exist", errTypeNotFound, "req_x"} {
		if !strings.Contains(got, want) {
			t.Errorf("ResponseBody = %s, want it to contain %q", got, want)
		}
	}
}

// TestWriteOpenAIErrorNoRelayContextIsNoop confirms the stash is a true
// no-op (no panic, no side effect) when no RelayContext is on the context —
// the path middleware.APIKeyAuth's own 401s take, since Handle never runs
// for those.
func TestWriteOpenAIErrorNoRelayContextIsNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	WriteOpenAIError(c, http.StatusUnauthorized, errTypeAuthentication, "missing API key")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
