package gateway

import (
	"net/http"
	"testing"
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
