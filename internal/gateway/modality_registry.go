package gateway

import (
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// modalities says which modality serves each ingress protocol.
//
// Registered per protocol rather than defaulted, because the two questions are
// different: what shape of payload a caller sent, and which protocol they sent
// it in. Text happens to serve all four today, and writing that out four times
// is the point — the entry is where a protocol that turns out to carry
// something else gets its own answer, and a default would silently hand that
// caller a text response instead.
var modalities = map[protocols.ProtocolID]Modality{
	protocols.ProtocolOpenAI:    NewTextModality(),
	protocols.ProtocolClaude:    NewTextModality(),
	protocols.ProtocolGemini:    NewTextModality(),
	protocols.ProtocolResponses: NewTextModality(),
	protocols.ProtocolImages:    NewImageModality(),
	protocols.ProtocolVideos:    NewVideoModality(),
	protocols.ProtocolAudio:     NewAudioModality(),
}

// modalityFor returns the modality registered against an ingress protocol.
//
// The miss is reported rather than absorbed. Nothing routes an unregistered
// protocol here today, and if that changes the request has no owner: serving it
// with whichever modality was nearest would answer a caller in a shape they
// cannot read, and answering nobody at all is the honest failure.
func modalityFor(ingress protocols.ProtocolID) (Modality, bool) {
	m, ok := modalities[ingress]
	return m, ok
}
