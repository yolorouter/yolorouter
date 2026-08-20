package responses

import "fmt"

// The Responses dialect reuses the OpenAI error envelope verbatim for
// non-stream errors, so it defines no envelope builder of its own — only
// the mid-stream frame shape is this dialect's own, and only that lives
// here.

// StreamErrorFrames builds the Responses-shaped mid-stream error: a bare
// `data: {"type":"error",...}` frame matching the top-level error event
// shape the stream decoder itself recognizes on the decode side, with no
// [DONE] terminator — the Responses SSE stream is framed as typed
// response.* events (response.created/.../response.failed), never the
// OpenAI Chat [DONE] sentinel (the decoder tolerates "[DONE]" only as a
// no-op, not as the completion signal).
func StreamErrorFrames(message string) [][]byte {
	return [][]byte{fmt.Appendf(nil, `data: {"type":"error","message":%q,"code":null,"param":null}`+"\n\n", message)}
}
