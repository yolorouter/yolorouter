// Package imagesize rewrites an image request's size separator for the
// upstream dialect that expects it.
//
// The OpenAI Images API spells a size "1024x1024"; some image models served
// behind OpenAI-compatible endpoints read "1024*1024" instead, and reject
// the other spelling outright. The model's own name says which family it is
// — the qwen-image and wan* lineages are the star-spelling ones — so the
// rewrite triggers on the model prefix rather than on provider identity: a
// prefix is a statement about the model, and it travels with the request
// through failover where a provider check would not.
//
// It is an egress rewriter because the spelling belongs to the upstream
// dialect, not to the exchange: the caller's body keeps the OpenAI spelling
// end to end, and only the bytes leaving for the provider change. The
// conversion is idempotent, so a candidate that converts again (the native
// DashScope dialect does its own) is untouched by the second pass.
package imagesize

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/images"
)

// View is what this capability needs of the exchange: which protocol the
// caller spoke, and the provider's own name for the model the current
// attempt addresses — the name whose family decides the spelling.
type View interface {
	IngressProtocol() protocols.ProtocolID
	CandidateProviderModelName() string
}

// starSpelledPrefixes are the model families whose upstreams read the size
// as "1024*1024". A prefix list, deliberately: new families join by name the
// way they arrive in the catalogue, without a provider-type registry entry.
var starSpelledPrefixes = []string{"qwen-image-", "wanx-", "wan2."}

// Separator is the capability. It holds no per-request state.
type Separator struct{}

// New builds the capability.
func New() *Separator { return &Separator{} }

func (*Separator) Name() string { return "image_size_separator" }

// RewriteEgress converts the size separator for the models that need it and
// leaves every other body alone — including image requests for other models
// and every non-image request, which is most of them.
func (*Separator) RewriteEgress(_ context.Context, view View, _ protocols.ProtocolID, body []byte, _ fact.Sink) ([]byte, error) {
	if view.IngressProtocol() != protocols.ProtocolImages {
		return body, nil
	}
	model := view.CandidateProviderModelName()
	starSpelled := false
	for _, prefix := range starSpelledPrefixes {
		if strings.HasPrefix(model, prefix) {
			starSpelled = true
			break
		}
	}
	if !starSpelled {
		return body, nil
	}
	// One-field patch rather than a decode-and-reencode: the request's other
	// fields — provider-private extensions among them — must reach the
	// upstream exactly as the caller wrote them.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		// Not this capability's business to judge; an unparseable images body
		// is refused where it was parsed, at admission.
		return body, nil
	}
	rawSize, ok := top["size"]
	if !ok {
		return body, nil
	}
	var size string
	if err := json.Unmarshal(rawSize, &size); err != nil || !strings.ContainsAny(size, "xX") {
		return body, nil
	}
	encoded, err := json.Marshal(images.ConvertSize(size))
	if err != nil {
		return body, nil
	}
	top["size"] = encoded
	out, err := json.Marshal(top)
	if err != nil {
		return body, nil
	}
	return out, nil
}
