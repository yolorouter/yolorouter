package gemini

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// errorBody is the Google API error envelope: a single nested "error" object
// carrying code/message/status. There is no request-id slot anywhere on the
// wire — the id only ever reaches the caller via a response header.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// errorStatus maps an HTTP status to Google's canonical UPPER_SNAKE status
// string. It maps directly from the HTTP status rather than from any error
// classification: Gemini's wire "status" field is defined in terms of the
// response's HTTP status, so deriving it from anything else would require a
// second, redundant mapping that could drift from the status code actually
// written.
func errorStatus(httpStatus int) string {
	switch httpStatus {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusInternalServerError:
		return "INTERNAL"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	default:
		switch {
		case httpStatus >= 400 && httpStatus < 500:
			return "INVALID_ARGUMENT"
		case httpStatus >= 500:
			return "INTERNAL"
		default:
			return "UNKNOWN"
		}
	}
}

// ErrorBody builds the Gemini-native error envelope. Both the "code" and
// "status" fields derive from the HTTP status; errType and requestID are
// unused — this dialect classifies by status alone and has no request-id
// slot — and are accepted so every protocol's builder shares one signature.
func ErrorBody(status int, _, message, _ string) []byte {
	b, _ := json.Marshal(errorBody{
		Error: errorDetail{Code: status, Message: message, Status: errorStatus(status)},
	})
	return b
}

// StreamErrorFrames builds the Gemini-shaped mid-stream error: a bare
// `data: {"error":{code,message,status}}` frame, with no [DONE] terminator —
// Gemini's streamGenerateContent SSE has no such convention at all (the
// stream encoder only ever emits plain data frames). The nested error uses
// 500/INTERNAL as its representative status: by the time a mid-stream error
// is written the response's real HTTP status is already committed as 200
// (headers went out with the first upstream chunk), so there is no
// meaningful per-request status left to report.
func StreamErrorFrames(message string) [][]byte {
	const midStreamStatus = http.StatusInternalServerError
	return [][]byte{fmt.Appendf(nil, `data: {"error":{"code":%d,"message":%q,"status":%q}}`+"\n\n",
		midStreamStatus, message, errorStatus(midStreamStatus))}
}
