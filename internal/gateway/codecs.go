package gateway

import (
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/chat"
	"github.com/yolorouter/yolorouter/internal/protocols/claude"
	"github.com/yolorouter/yolorouter/internal/protocols/gemini"
	"github.com/yolorouter/yolorouter/internal/protocols/responses"
)

// protocolCodecs groups the six codec constructors for one wire protocol.
// This registry covers the relay's encode/decode dispatch; a new protocol
// additionally registers its modality (modality_registry.go), its credential
// probe behaviours (service's probeSpecs), and its compress live-zone
// locator (compress.ByProtocol) — each a table entry, never a new switch.
// Request/Response codecs are stateless empty structs and are stored by
// value; the stream codecs are stateful per-request state machines and must
// be constructed fresh for every call, hence the New... factory function
// fields.
type protocolCodecs struct {
	RequestDecoder   protocols.RequestDecoder
	RequestEncoder   protocols.RequestEncoder
	ResponseDecoder  protocols.ResponseDecoder
	ResponseEncoder  protocols.ResponseEncoder
	NewStreamDecoder func() protocols.StreamDecoder
	NewStreamEncoder func() protocols.StreamEncoder
	// ErrorBody builds the protocol's non-stream error envelope — the bytes
	// the caller is sent AND the bytes the audit row stores, from one
	// builder so the two cannot differ. Each dialect handles the request id
	// its own way (inside the message, a structural field, or header-only);
	// the builder owns that rule, so no call site re-implements it.
	ErrorBody func(status int, errType, message, requestID string) []byte
	// StreamErrorFrames builds the mid-stream error frames, terminator
	// convention included: a dialect that ends its stream with a sentinel
	// emits it here, and one that has no such convention emits nothing —
	// sending another dialect's terminator would be a protocol violation
	// its SDKs don't expect.
	StreamErrorFrames func(message string) [][]byte
}

var codecRegistry = map[protocols.ProtocolID]protocolCodecs{
	protocols.ProtocolOpenAI: {
		RequestDecoder:    chat.RequestDecoder{},
		RequestEncoder:    chat.RequestEncoder{},
		ResponseDecoder:   chat.ResponseDecoder{},
		ResponseEncoder:   chat.ResponseEncoder{},
		NewStreamDecoder:  func() protocols.StreamDecoder { return chat.NewStreamDecoder() },
		NewStreamEncoder:  func() protocols.StreamEncoder { return chat.NewStreamEncoder() },
		ErrorBody:         chat.ErrorBody,
		StreamErrorFrames: chat.StreamErrorFrames,
	},
	protocols.ProtocolClaude: {
		RequestDecoder:    claude.RequestDecoder{},
		RequestEncoder:    claude.RequestEncoder{},
		ResponseDecoder:   claude.ResponseDecoder{},
		ResponseEncoder:   claude.ResponseEncoder{},
		NewStreamDecoder:  func() protocols.StreamDecoder { return claude.NewStreamDecoder() },
		NewStreamEncoder:  func() protocols.StreamEncoder { return claude.NewStreamEncoder() },
		ErrorBody:         claude.ErrorBody,
		StreamErrorFrames: claude.StreamErrorFrames,
	},
	protocols.ProtocolGemini: {
		RequestDecoder:    gemini.RequestDecoder{},
		RequestEncoder:    gemini.RequestEncoder{},
		ResponseDecoder:   gemini.ResponseDecoder{},
		ResponseEncoder:   gemini.ResponseEncoder{},
		NewStreamDecoder:  func() protocols.StreamDecoder { return gemini.NewStreamDecoder() },
		NewStreamEncoder:  func() protocols.StreamEncoder { return gemini.NewStreamEncoder() },
		ErrorBody:         gemini.ErrorBody,
		StreamErrorFrames: gemini.StreamErrorFrames,
	},
	protocols.ProtocolResponses: {
		RequestDecoder:   responses.RequestDecoder{},
		RequestEncoder:   responses.RequestEncoder{},
		ResponseDecoder:  responses.ResponseDecoder{},
		ResponseEncoder:  responses.ResponseEncoder{},
		NewStreamDecoder: func() protocols.StreamDecoder { return responses.NewStreamDecoder() },
		NewStreamEncoder: func() protocols.StreamEncoder { return responses.NewStreamEncoder() },
		// The Responses dialect reuses the OpenAI error envelope verbatim
		// for non-stream errors; only its mid-stream frame shape is its own.
		ErrorBody:         chat.ErrorBody,
		StreamErrorFrames: responses.StreamErrorFrames,
	},
}

// codecsFor returns the codec group for a protocol; an unregistered protocol
// falls back to OpenAI Chat, mirroring the ingress/egress default elsewhere
// in the gateway.
func codecsFor(p protocols.ProtocolID) protocolCodecs {
	if c, ok := codecRegistry[p]; ok {
		return c
	}
	return codecRegistry[protocols.ProtocolOpenAI]
}
