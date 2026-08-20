package gemini

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestErrorBodyStatusMapping covers every HTTP status with an explicit
// mapping to Google's canonical status string, plus the generic 4xx/5xx
// fallbacks, asserted through the envelope the builder actually produces.
func TestErrorBodyStatusMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   string
	}{
		{"bad_request", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"unauthorized", http.StatusUnauthorized, "UNAUTHENTICATED"},
		{"forbidden", http.StatusForbidden, "PERMISSION_DENIED"},
		{"not_found", http.StatusNotFound, "NOT_FOUND"},
		{"too_many_requests", http.StatusTooManyRequests, "RESOURCE_EXHAUSTED"},
		{"internal_server_error", http.StatusInternalServerError, "INTERNAL"},
		{"service_unavailable", http.StatusServiceUnavailable, "UNAVAILABLE"},
		{"gateway_timeout", http.StatusGatewayTimeout, "DEADLINE_EXCEEDED"},
		{"other_4xx", http.StatusConflict, "INVALID_ARGUMENT"},
		{"other_5xx", http.StatusBadGateway, "INTERNAL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
					Status  string `json:"status"`
				} `json:"error"`
			}
			if err := json.Unmarshal(ErrorBody(tt.status, "ignored", "m", "rid"), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if body.Error.Status != tt.want {
				t.Errorf("error.status for %d = %q, want %q", tt.status, body.Error.Status, tt.want)
			}
			if body.Error.Code != tt.status || body.Error.Message != "m" {
				t.Errorf("envelope = %+v, want code mirroring the HTTP status and message unchanged", body.Error)
			}
		})
	}
}
