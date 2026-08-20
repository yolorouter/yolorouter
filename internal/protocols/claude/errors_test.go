package claude

import (
	"encoding/json"
	"testing"
)

// TestErrorBodyTypeMapping covers every OpenAI-flavored error-type string's
// mapping to a valid Anthropic error type, asserted through the envelope the
// builder actually produces.
func TestErrorBodyTypeMapping(t *testing.T) {
	tests := []struct {
		name    string
		errType string
		want    string
	}{
		{"authentication", "authentication_error", "authentication_error"},
		{"permission", "permission_error", "permission_error"},
		{"invalid_request", "invalid_request_error", "invalid_request_error"},
		{"not_found", "not_found_error", "not_found_error"},
		{"rate_limit", "rate_limit_error", "rate_limit_error"},
		{"insufficient_quota", "insufficient_quota", "invalid_request_error"},
		{"unavailable", "service_unavailable", "overloaded_error"},
		{"upstream", "upstream_error", "api_error"},
		{"server", "server_error", "api_error"},
		{"unknown_default", "some_unmapped_type", "api_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
				Error     struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(ErrorBody(0, tt.errType, "m", "rid"), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if body.Error.Type != tt.want {
				t.Errorf("error.type for %q = %q, want %q", tt.errType, body.Error.Type, tt.want)
			}
			if body.Type != "error" || body.RequestID != "rid" || body.Error.Message != "m" {
				t.Errorf("envelope = %+v, want type=error, request_id=rid, message unchanged", body)
			}
		})
	}
}
