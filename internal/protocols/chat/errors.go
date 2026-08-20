package chat

import (
	"encoding/json"
	"fmt"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// errorBody is the OpenAI-compatible error envelope: a single nested
// "error" object carrying message and type. The envelope has no field for a
// request id — the OpenAI dialect carries it inside the message instead
// (see ErrorBody).
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// ErrorBody builds the OpenAI-compatible error envelope. status is unused —
// the envelope derives nothing from the HTTP status — and is accepted so
// every protocol's builder shares one signature. The request id is appended
// into the message (" (request: <id>)"): this dialect has no structural slot
// for it, and the message is the one place a caller quoting the error to
// support will copy from.
func ErrorBody(_ int, errType, message, requestID string) []byte {
	b, _ := json.Marshal(errorBody{Error: errorDetail{
		Message: protocols.AppendRequestID(message, requestID),
		Type:    errType,
	}})
	return b
}

// StreamErrorFrames builds the OpenAI-shaped mid-stream error: an inline
// `data: {"error":...}` frame followed by `data: [DONE]`. The terminator is
// part of the dialect — OpenAI SDK clients block on [DONE] to finalize
// their completion, and omitting it leaves them hanging until their own
// read timeout.
func StreamErrorFrames(message string) [][]byte {
	return [][]byte{
		fmt.Appendf(nil, `data: {"error":{"message":%q,"type":"upstream_error"}}`+"\n\n", message),
		[]byte("data: [DONE]\n\n"),
	}
}
