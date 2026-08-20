package claude

import (
	"encoding/json"
	"fmt"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// errorBody is the Anthropic-native error envelope: unlike the OpenAI shape
// (nested-only), the request id sits at the top level alongside a top-level
// "type":"error" discriminator, and the nested error.type uses Anthropic's
// own vocabulary (see errorType) rather than the OpenAI-flavored strings.
type errorBody struct {
	Type      string      `json:"type"`
	RequestID string      `json:"request_id"`
	Error     errorDetail `json:"error"`
}

type errorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// errorType maps the OpenAI-flavored error-type strings — the vocabulary
// every caller of ErrorBody speaks — to the closest valid Anthropic error
// type, so one classification can drive either dialect's wire envelope
// without every call site knowing both vocabularies.
func errorType(errType string) string {
	switch errType {
	case "authentication_error":
		return "authentication_error"
	case "permission_error":
		return "permission_error"
	case "invalid_request_error":
		return "invalid_request_error"
	case "not_found_error":
		return "not_found_error"
	case "rate_limit_error":
		return "rate_limit_error"
	case "insufficient_quota":
		// Anthropic has no quota-specific error type; invalid_request_error
		// is the closest fit.
		return "invalid_request_error"
	case "service_unavailable":
		return "overloaded_error"
	case "upstream_error", "server_error":
		return "api_error"
	default:
		return "api_error"
	}
}

// ErrorBody builds the Anthropic-native error envelope. status is unused —
// the envelope derives nothing from the HTTP status — and is accepted so
// every protocol's builder shares one signature. The message reaches the
// caller unmodified: this dialect carries the request id as a top-level
// request_id field, never inside the message.
func ErrorBody(_ int, errType, message, requestID string) []byte {
	b, _ := json.Marshal(errorBody{
		Type:      "error",
		RequestID: requestID,
		Error:     errorDetail{Type: errorType(errType), Message: message},
	})
	return b
}

// StreamErrorFrames builds the Anthropic-shaped mid-stream error: a single
// `event: error` SSE event carrying the Messages API error envelope. This
// dialect has no [DONE] terminator convention — the Anthropic SDK treats
// the connection close right after this event as the end of the stream, so
// nothing further is emitted.
func StreamErrorFrames(message string) [][]byte {
	evt := protocols.SSEEvent{
		Event: "error",
		Data:  fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":%q}}`, message),
	}
	return [][]byte{[]byte(evt.String())}
}
