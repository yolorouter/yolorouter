package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

func TestClassifyUpstreamStatus(t *testing.T) {
	tests := []struct {
		status   int
		category statusCategory
		outcome  string
		errType  string
	}{
		{http.StatusUnauthorized, statusRotateKey, AttemptAuthFailed, errTypeAuthentication},
		{http.StatusTooManyRequests, statusRotateKey, AttemptRateLimited, errTypeRateLimit},
		{http.StatusInternalServerError, statusFailover, AttemptServerError, errTypeUpstream},
		{http.StatusBadGateway, statusFailover, AttemptServerError, errTypeUpstream},
		{http.StatusServiceUnavailable, statusFailover, AttemptServerError, errTypeUpstream},
		{http.StatusBadRequest, statusTerminalClient, AttemptClientError, errTypeInvalidRequest},
		// 403 is terminal (provider-scoped), NOT a rotate-Key status.
		{http.StatusForbidden, statusTerminalClient, AttemptClientError, errTypePermission},
		{http.StatusNotFound, statusTerminalClient, AttemptClientError, errTypeNotFound},
		{http.StatusUnprocessableEntity, statusTerminalClient, AttemptClientError, errTypeInvalidRequest},
	}
	for _, tt := range tests {
		if tt.errType == "" {
			t.Fatalf("status %d expects an empty error type: every classification has to name "+
				"one, because a verdict carrying this status can reach the caller and an empty "+
				"type arrives as a field their client will branch on and find nothing",
				tt.status)
		}
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

// TestWriteIngressError_OpenAIStashesResponseBody: when an Exchange is on
// the gin context (as Handle installs it), the local error JSON returned to
// the caller is also stashed into rc.ResponseBody(), so
// request_log_bodies.response_body reflects what the caller actually
// received for a locally-rejected request.
func TestWriteIngressError_OpenAIStashesResponseBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	rc := &Exchange{requestID: "req_x"}
	c.Set(relayContextKey, rc)

	WriteIngressError(c, protocols.ProtocolOpenAI, http.StatusNotFound, errTypeNotFound, "model does not exist", "req_x")

	if rc.ResponseBody() == nil {
		t.Fatal("rc.ResponseBody() was not stashed")
	}
	got := string(rc.ResponseBody())
	for _, want := range []string{`"message"`, `"type"`, "model does not exist", errTypeNotFound, "req_x"} {
		if !strings.Contains(got, want) {
			t.Errorf("ResponseBody = %s, want it to contain %q", got, want)
		}
	}
}

// TestWriteIngressError_OpenAINoExchangeIsNoop confirms the stash is a
// true no-op (no panic, no side effect) when no Exchange is on the context
// — the path middleware.APIKeyAuth's own 401s take, since Handle never runs
// for those.
func TestWriteIngressError_OpenAINoExchangeIsNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	WriteIngressError(c, protocols.ProtocolOpenAI, http.StatusUnauthorized, errTypeAuthentication, "missing API key", "")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// quotaExhaustedBody separates "the account cannot pay" from "slow down".
// The former is key-scoped and permanent until topped up; the latter heals
// on its own — conflating them either strands healthy keys or burns
// attempts on dead ones.
func TestQuotaExhaustedBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"openai insufficient_quota code", `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`, true},
		{"insufficient_quota type without code, as a yolorouter upstream sends it", `{"error":{"type":"insufficient_quota","message":"budget limit exceeded"}}`, true},
		{"billing in message", `{"error":{"message":"billing hard limit reached"}}`, true},
		{"credit in message", `{"error":{"message":"Your credit balance is too low"}}`, true},
		{"plain rate limit", `{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached, retry after 2s"}}`, false},
		{"gemini per-minute throttle is not exhaustion", `{"error":{"code":429,"message":"Quota exceeded for quota metric 'GenerateContentRequestsPerMinute' and limit 'per minute' of service","status":"RESOURCE_EXHAUSTED"}}`, false},
		{"bare quota word is not a signal", `{"error":{"message":"Request quota reached, slow down"}}`, false},
		{"numeric code does not panic", `{"error":{"code":429,"message":"too many requests"}}`, false},
		{"empty body", ``, false},
		{"not json", `slow down`, false},
		{"no error object", `{"message":"quota"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaExhaustedBody([]byte(tc.body)); got != tc.want {
				t.Fatalf("quotaExhaustedBody(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
