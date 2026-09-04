package protocols

import "encoding/json"

// ProtocolID identifies a wire protocol.
type ProtocolID string

const (
	ProtocolOpenAI    ProtocolID = "openai"
	ProtocolClaude    ProtocolID = "anthropic"
	ProtocolGemini    ProtocolID = "gemini"
	ProtocolResponses ProtocolID = "responses"
	// ProtocolImages is the OpenAI Images API family (POST
	// /v1/images/generations): JSON in, JSON out, no IR. Unlike the four
	// chat protocols it has no IR codecs — a request is routed on a subset
	// of its fields and forwarded with only the model field rewritten, so
	// its "codec" is the parse in internal/protocols/images and the
	// modality's own passthrough.
	ProtocolImages ProtocolID = "images"
	// ProtocolVideos is the OpenAI Videos job family (POST /v1/videos):
	// submit returns a job resource the caller polls, so unlike every
	// other ingress protocol the request that admits a payload and the
	// requests that observe its outcome are different HTTP calls. Its
	// shapes live in internal/protocols/videos.
	ProtocolVideos ProtocolID = "videos"
	// ProtocolAudio is the OpenAI speech family (POST /v1/audio/speech):
	// JSON in, binary audio out, billed by the character. Like images it
	// has no IR codecs — the modality parses the request itself and
	// encodes the upstream body in the serving dialect's shape.
	ProtocolAudio ProtocolID = "audio"
)

// RequestDecoder decodes a protocol-specific request JSON into IR.
type RequestDecoder interface {
	Protocol() ProtocolID
	DecodeRequest(body json.RawMessage, model string, isStream bool) (*IRRequest, error)
}

// ResponseDecoder decodes an upstream response JSON into IR.
type ResponseDecoder interface {
	DecodeResponse(body json.RawMessage) (*IRResponse, error)
}

// StreamDecoder decodes upstream SSE/JSON Lines into IR deltas.
type StreamDecoder interface {
	DecodeChunk(raw string) ([]IRStreamDelta, error)
	Finish() ([]IRStreamDelta, error)
}
