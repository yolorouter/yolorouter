package gateway

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// relayContextKey is the gin.Context key Handle stores the in-flight
// RelayContext under (relay.go: c.Set(relayContextKey, rc)), so
// WriteOpenAIError* can stash the local error JSON it is about to return
// into rc.ResponseBody without threading an
// *RelayContext parameter through every call site. Absent on paths that
// never call Handle (e.g. unit tests, or middleware.APIKeyAuth's own 401s
// before Handle ever runs) — stashLocalErrorBody is then a no-op.
const relayContextKey = "relay_context"

// openaiErrorBody is the OpenAI-compatible error envelope. Gateway traffic
// uses upstream's native wire format, NOT pkg/response — so
// these responses intentionally do not carry the admin API's Code/Message
// envelope.
type openaiErrorBody struct {
	Error openaiError `json:"error"`
}

type openaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// LocalErrorBody serializes the OpenAI-compatible error envelope used by
// WriteOpenAIError/WriteOpenAIErrorWithRequestID. Exported so
// middleware.logAuthRejection (a different package, rejecting requests
// before Handle — and any RelayContext — ever exists) can build the exact
// same response_body JSON for its own request_log_bodies row instead of
// duplicating the envelope shape (single source of truth for
// shared logic).
func LocalErrorBody(errType, message string) []byte {
	b, _ := json.Marshal(openaiErrorBody{Error: openaiError{Message: message, Type: errType}})
	return b
}

// stashLocalErrorBody records the local error JSON WriteOpenAIError is about
// to return, as response_body for this request's request_log_bodies row
// No-op when no RelayContext is on the context. The
// body is a gateway-generated error envelope (no caller/upstream content), so
// it is stored verbatim — v0.1 does not scrub body content.
func stashLocalErrorBody(c *gin.Context, errType, message string) {
	v, ok := c.Get(relayContextKey)
	if !ok {
		return
	}
	rc, ok := v.(*RelayContext)
	if !ok || rc == nil {
		return
	}
	rc.ResponseBody = LocalErrorBody(errType, message)
}

// OpenAI error "type" values (each failure class maps to one of
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
	stashLocalErrorBody(c, errType, message)
	c.AbortWithStatusJSON(status, openaiErrorBody{
		Error: openaiError{Message: message, Type: errType},
	})
}

// WriteOpenAIErrorWithRequestID is WriteOpenAIError with the request id
// appended to the message, so a caller reporting an error can quote the id
// and the admin can find the row.
func WriteOpenAIErrorWithRequestID(c *gin.Context, status int, errType, message, requestID string) {
	if requestID != "" {
		message = message + " (request: " + requestID + ")"
	}
	WriteOpenAIError(c, status, errType, message)
}

// statusCategory classifies a non-2xx upstream HTTP status into the relay
// loop's three branches: rotate to another Key on the same
// provider, failover to the next candidate, or surface as terminal (no
// switch).
type statusCategory int

const (
	statusRotateKey      statusCategory = iota // 401/429: Key-scoped, try next key
	statusFailover                             // 5xx: provider-scoped, try next candidate
	statusTerminalClient                       // other 4xx: caller's problem, no switch
)

// upstreamStatusClass is the full classification attemptOne needs from one
// upstream HTTP status: which branch to take, what outcome label to log,
// and (for terminal 4xx) which OpenAI error type to surface.
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
