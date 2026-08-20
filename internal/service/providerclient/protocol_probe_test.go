package providerclient

import (
	"slices"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/chat"
	"github.com/yolorouter/yolorouter/internal/providerproto"
)

// TestUnknownProtocolFallsBackToOpenAI pins the fallback the table's own
// comment promises: a protocol nobody registered probes exactly like OpenAI —
// same encoder, same body shape — mirroring providerproto.TypeOf's
// normalization of unknown provider_type values. Without this, an
// unrecognized ProtocolID would silently probe with whatever spec the
// fallback happens to hand out, and a credential test could pass against an
// endpoint production never dispatches to that way.
func TestUnknownProtocolFallsBackToOpenAI(t *testing.T) {
	const bogus = protocols.ProtocolID("bogus-protocol")

	if _, ok := requestEncoderFor(bogus).(chat.RequestEncoder); !ok {
		t.Errorf("requestEncoderFor(unknown) = %T, want the OpenAI chat encoder", requestEncoderFor(bogus))
	}

	payload := chatCompletionPayload(bogus, "m1")
	if _, ok := payload["messages"]; !ok {
		t.Errorf("chatCompletionPayload(unknown) = %v, want the OpenAI messages shape", payload)
	}
	if payload["model"] != "m1" {
		t.Errorf("chatCompletionPayload(unknown) model = %v, want the tested model", payload["model"])
	}

}

// TestEveryProbeSpecIsFullyPopulated makes a half-filled entry a red test
// instead of a nil-dereference at probe time: every registered spec must
// carry every function field and a catalogue path — and the set of entries
// must be exactly the protocol vocabulary providerproto declares, so adding
// a protocol there is a red test here until its probe entry exists.
// successCertifiable is deliberately absent — false is a meaningful value
// (the entry defers its success validation), not a hole.
func TestEveryProbeSpecIsFullyPopulated(t *testing.T) {
	for _, want := range providerproto.All() {
		if _, ok := probeSpecs[want]; !ok {
			t.Errorf("providerproto declares %s but probeSpecs has no entry for it", want)
		}
	}
	for proto := range probeSpecs {
		if !slices.Contains(providerproto.All(), proto) {
			t.Errorf("probeSpecs has an entry %s that providerproto does not declare", proto)
		}
	}
	for proto, spec := range probeSpecs {
		if spec.encoder == nil {
			t.Errorf("%s: encoder is nil", proto)
		}
		if spec.basicPayload == nil || spec.streamingPayload == nil || spec.functionCallingPayload == nil {
			t.Errorf("%s: a payload builder is nil", proto)
		}
		if spec.parseModelPage == nil {
			t.Errorf("%s: parseModelPage is nil", proto)
		}
		if spec.modelsPath == "" {
			t.Errorf("%s: modelsPath is empty", proto)
		}
		if spec.validStreamBody == nil {
			t.Errorf("%s: validStreamBody is nil", proto)
		}
		if spec.validToolCallBody == nil {
			t.Errorf("%s: validToolCallBody is nil", proto)
		}
		if spec.validSuccessBody == nil || spec.modelScopedError == nil || spec.quotaError == nil ||
			spec.modelNotFoundError == nil || spec.extractMessage == nil {
			t.Errorf("%s: a body predicate is nil", proto)
		}
	}
}
