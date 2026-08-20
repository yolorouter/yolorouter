package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// TestEveryProtocolRegistersItsErrorCodecs pins registry completeness two
// ways. The key set is exact: a new ProtocolID that never reaches the
// registry would silently fall back to the OpenAI error shape, so adding a
// protocol must come here and extend the list — a conscious decision, not a
// silent default. And within each entry the two error fields are non-nil: a
// nil one would not fail loudly, the dispatch would panic mid-request on
// the first local error that ingress produces.
func TestEveryProtocolRegistersItsErrorCodecs(t *testing.T) {
	want := map[protocols.ProtocolID]bool{
		protocols.ProtocolOpenAI:    true,
		protocols.ProtocolClaude:    true,
		protocols.ProtocolGemini:    true,
		protocols.ProtocolResponses: true,
	}
	for id := range want {
		if _, ok := codecRegistry[id]; !ok {
			t.Errorf("%s: protocol has no registry entry at all", id)
		}
	}
	for id, c := range codecRegistry {
		if !want[id] {
			t.Errorf("%s: registry entry not in this test's list — a new protocol must be added here so its error dialect is a decision, not a default", id)
		}
		if c.ErrorBody == nil {
			t.Errorf("%s: ErrorBody is nil — the protocol has no non-stream error envelope", id)
		}
		if c.StreamErrorFrames == nil {
			t.Errorf("%s: StreamErrorFrames is nil — the protocol has no mid-stream error shape", id)
		}
	}
}

// TestClaudeErrorTypeMappingSpeaksTheDecisionVocabulary drives every
// caller-facing error-type constant through the Claude envelope builder and
// pins the Anthropic type each one surfaces as. The mapping lives in the
// claude package as string literals; this test is the compile-side link
// back to the constants — renaming a decision.ErrType* value would silently
// degrade its Claude surface to api_error, and only this loop notices.
func TestClaudeErrorTypeMappingSpeaksTheDecisionVocabulary(t *testing.T) {
	tests := []struct {
		errType string
		want    string
	}{
		{errTypeAuthentication, "authentication_error"},
		{errTypePermission, "permission_error"},
		{errTypeInvalidRequest, "invalid_request_error"},
		{errTypeNotFound, "not_found_error"},
		{errTypeRateLimit, "rate_limit_error"},
		{errTypeInsufficientQuota, "invalid_request_error"},
		{errTypeUnavailable, "overloaded_error"},
		{errTypeUpstream, "api_error"},
		{errTypeServer, "api_error"},
	}
	for _, tt := range tests {
		var body struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		raw := LocalIngressErrorBody(protocols.ProtocolClaude, http.StatusBadRequest, tt.errType, "m", "rid")
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Error.Type != tt.want {
			t.Errorf("errType %q surfaces as %q on the Claude wire, want %q", tt.errType, body.Error.Type, tt.want)
		}
	}
}

// TestClassifyKeepsAuthAndRateLimitDistinct guards the derivation in
// classifyUpstreamStatus: 401 and 429 share the rotate-Key routing category,
// but their attempt outcomes and caller-facing error types must stay
// distinct — a derivation that folded them together would label every rate
// limit as an auth failure on the audit row.
func TestClassifyKeepsAuthAndRateLimitDistinct(t *testing.T) {
	auth := classifyUpstreamStatus(http.StatusUnauthorized)
	rate := classifyUpstreamStatus(http.StatusTooManyRequests)
	if auth.Category != statusRotateKey || rate.Category != statusRotateKey {
		t.Fatalf("categories = %v/%v, want both rotate-Key", auth.Category, rate.Category)
	}
	if auth.Outcome == rate.Outcome || auth.ErrorType == rate.ErrorType {
		t.Errorf("401 (%s/%s) and 429 (%s/%s) must not fold together",
			auth.Outcome, auth.ErrorType, rate.Outcome, rate.ErrorType)
	}
	if auth.Outcome != AttemptAuthFailed || rate.Outcome != AttemptRateLimited {
		t.Errorf("outcomes = %s/%s, want auth_failed/rate_limited", auth.Outcome, rate.Outcome)
	}
}
