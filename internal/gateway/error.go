package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// openaiErrorBody is the OpenAI-compatible error envelope. Gateway traffic
// uses upstream's native wire format, NOT pkg/response (design doc §3) — so
// these responses intentionally do not carry the admin API's Code/Message
// envelope.
type openaiErrorBody struct {
	Error openaiError `json:"error"`
}

type openaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// OpenAI error "type" values (PRD §6.5.9 maps each failure class to one of
// these). Kept as untyped string constants — they only appear at the
// WriteOpenAIError call sites and in tests.
const (
	errTypeAuthentication    = "authentication_error"
	errTypePermission        = "permission_error"
	errTypeRateLimit         = "rate_limit_error"
	errTypeInvalidRequest    = "invalid_request_error"
	errTypeNotFound          = "not_found_error"
	errTypeUpstream          = "upstream_error"
	errTypeServer            = "server_error"
	errTypeUnavailable       = "service_unavailable"
	errTypeInsufficientQuota = "insufficient_quota" // OpenAI's type for budget/quota exhaustion (distinct from rate_limit_error)
)

// WriteOpenAIError writes one OpenAI-compatible error response and aborts
// the chain. status is the HTTP status; errType is the error.type string;
// message is shown verbatim to the caller.
func WriteOpenAIError(c *gin.Context, status int, errType, message string) {
	c.AbortWithStatusJSON(status, openaiErrorBody{
		Error: openaiError{Message: message, Type: errType},
	})
}

// WriteOpenAIErrorWithRequestID is WriteOpenAIError with the request id
// appended to the message, so a caller reporting an error can quote the id
// and the admin can find the row (GATE-08).
func WriteOpenAIErrorWithRequestID(c *gin.Context, status int, errType, message, requestID string) {
	if requestID != "" {
		message = message + " (request: " + requestID + ")"
	}
	WriteOpenAIError(c, status, errType, message)
}

// statusCategory classifies a non-2xx upstream HTTP status into the relay
// loop's three branches (PRD §6.5.7): rotate to another Key on the same
// provider, failover to the next candidate, or surface as terminal (no
// switch).
type statusCategory int

const (
	statusRotateKey      statusCategory = iota // 401/429: Key-scoped, try next key (GATE-09)
	statusFailover                             // 5xx: provider-scoped, try next candidate (GATE-10)
	statusTerminalClient                       // other 4xx: caller's problem, no switch (GATE-11)
)

// upstreamStatusClass is the full classification attemptOne needs from one
// upstream HTTP status: which branch to take, what outcome label to log
// (GATE-13), and (for terminal 4xx) which OpenAI error type to surface.
type upstreamStatusClass struct {
	Category  statusCategory
	Outcome   string
	ErrorType string
}

// classifyUpstreamStatus maps a non-2xx upstream status to its relay
// classification. One call site (attemptOne), one source of truth —
// replaces the former statusIsKeyRotation / statusIsCandidateFailover /
// clientErrorTypeFor / keyOutcome quartet that was spread across two files
// and had already drifted (the candidate/client branches hardcoded outcome
// labels while the rotate branch used a separate keyOutcome helper).
//
// 403 is intentionally NOT a rotate-Key status: a 403 from an
// OpenAI-compatible provider is usually account/permission scoped (the whole
// provider is forbidden), so rotating Keys within it is futile and we fall
// through to terminal.
func classifyUpstreamStatus(status int) upstreamStatusClass {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusTooManyRequests:
		outcome := AttemptAuthFailed
		if status == http.StatusTooManyRequests {
			outcome = AttemptRateLimited
		}
		return upstreamStatusClass{Category: statusRotateKey, Outcome: outcome}
	case status >= 500:
		return upstreamStatusClass{Category: statusFailover, Outcome: AttemptServerError}
	default:
		errType := errTypeInvalidRequest
		switch status {
		case http.StatusNotFound:
			errType = errTypeNotFound
		case http.StatusForbidden:
			errType = errTypePermission
		}
		return upstreamStatusClass{Category: statusTerminalClient, Outcome: AttemptClientError, ErrorType: errType}
	}
}
