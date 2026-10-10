// Package visionfallback turns images into text for models that cannot see.
//
// It is an ingress rewriter: when a caller sends images to a model whose
// admin declared it unable to read them, the request's images are described
// through the globally configured vision model (a loopback call through this
// gateway's own API, billed to the same caller) and replaced with the
// descriptions; with no vision model configured they are replaced with
// placeholders, which keeps an upstream that would reject image parts from
// failing the whole request.
//
// It never refuses a request and never invents certainty: a model whose
// declaration is absent is left entirely alone (missing metadata must not
// degrade a vision-capable model), a body that cannot be parsed is forwarded
// as the caller sent it, and when every describe attempt fails the original
// bytes go through untouched — the upstream's own error beats silently
// dropping content.
package visionfallback

import (
	"context"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/settings"
)

// View is what this capability needs to know about the exchange. The
// configuration (which model describes, with what prompt) is resolved per
// request by the kernel; the sub-call bit is the recursion guard — a
// describe call must never trigger another describe call.
type View interface {
	IngressProtocol() protocols.ProtocolID
	IngressPath() string
	IsChatEndpoint() bool
	RequestID() string
	VisionFallbackModel() string
	VisionFallbackPrompt() string
	AuthCredential() string
	IsVisionFallbackSubCall() bool
}

const (
	// maxImages bounds how many images one request may have described; the
	// rest keep placeholders. Caps the loopback fan-out (and its cost) a
	// single caller request can trigger.
	maxImages = 10
	// maxBase64Len bounds a single inline image payload (~10MB of pixels).
	maxBase64Len = 13_500_000
	// describeTimeout serves twice: as the per-call HTTP client timeout and
	// as the context budget over the whole describe pass — a loopback call is
	// a full gateway request of its own and must not eat the caller's budget.
	describeTimeout = 30 * time.Second
)

// Fallback is the capability. loopbackBase is this server's own address
// ("http://localhost:<port>"), supplied at assembly.
type Fallback struct {
	db           *gorm.DB
	loopbackBase string
	client       *http.Client
}

// New builds the capability against the shared DB handle and the server's
// own base URL.
func New(db *gorm.DB, loopbackBase string) *Fallback {
	return &Fallback{
		db:           db,
		loopbackBase: loopbackBase,
		client:       &http.Client{Timeout: describeTimeout},
	}
}

func (*Fallback) Name() string { return "visionfallback" }

// Applies gates on what the view alone can answer: only chat-shaped
// endpoints carry image content blocks, and a loopback self-call must never
// re-enter this capability. Whether the target model actually needs help is
// a DB question, answered (cheaply, behind a byte-scan) in RewriteIngress.
func (*Fallback) Applies(view View) bool {
	return view.IsChatEndpoint() && !view.IsVisionFallbackSubCall()
}

// RewriteIngress returns the body with images described or stripped, or the
// caller's own bytes untouched whenever it cannot (or must not) act.
func (f *Fallback) RewriteIngress(ctx context.Context, view View, body []byte, sink fact.Sink) ([]byte, error) {
	proto := view.IngressProtocol()
	if !hasImageMarker(proto, body) {
		return body, nil
	}
	modelName := extractModelName(proto, view.IngressPath(), body)
	if modelName == "" {
		return body, nil
	}
	m, err := repository.FindModelByName(f.db.WithContext(ctx), modelName)
	//nolint:staticcheck // SA1019: reading the deprecated column is the point —
	// this hot path keeps consuming supports_image_input until the switch to
	// input_modalities; every save-path write of the array derives the column
	// from the same direction, so this read observes exactly what the admin
	// declared.
	if err != nil || m.SupportsImageInput == nil || *m.SupportsImageInput {
		// Unknown model (routing will reject it authoritatively), undeclared,
		// or declared image-capable: not this capability's business.
		return body, nil
	}

	sites, err := collectImageSites(proto, body)
	if err != nil {
		sink.Note(fact.VisionFallbackSkipped{Reason: "parse_failed"})
		return body, nil
	}
	if len(sites) == 0 {
		return body, nil
	}

	texts := make([]string, len(sites))
	if view.VisionFallbackModel() == "" {
		// No describe model configured: strip. Placeholders instead of
		// silent removal, so the target model knows content was elided
		// rather than seeing a conversation with holes in it.
		for i := range texts {
			texts[i] = strippedPlaceholder
		}
		out, err := replaceImageTexts(proto, body, texts)
		if err != nil {
			sink.Note(fact.VisionFallbackSkipped{Reason: "patch_failed"})
			return body, nil
		}
		sink.Note(fact.VisionFallbackStripped{Images: len(sites)})
		return out, nil
	}

	// Only in-limit images are worth a describe call; the rest are settled
	// as over-limit placeholders up front — a hard limit is not a transient
	// failure, and forwarding the image anyway would hand it to a model
	// declared unable to read it.
	attempted := make([]bool, len(sites))
	attemptedCount := 0
	for i, site := range sites {
		if i < maxImages && site.payloadLen <= maxBase64Len {
			attempted[i] = true
			attemptedCount++
		}
	}

	if attemptedCount == 0 {
		// Every image fell outside the limits: nothing was (or will be)
		// described, so the describe model must not be named on the record —
		// this is a strip, just with the limit placeholder.
		for i := range texts {
			texts[i] = limitPlaceholder
		}
		out, err := replaceImageTexts(proto, body, texts)
		if err != nil {
			sink.Note(fact.VisionFallbackSkipped{Reason: "patch_failed"})
			return body, nil
		}
		sink.Note(fact.VisionFallbackStripped{Images: len(sites)})
		return out, nil
	}

	prompt := view.VisionFallbackPrompt()
	if prompt == "" {
		prompt = settings.VisionFallbackDefaultPrompt
	}
	dctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	descriptions := f.describeAll(dctx, sites, attempted, prompt, view)

	described := 0
	for i := range sites {
		switch {
		case descriptions[i] != "":
			described++
			texts[i] = "[Image description: " + descriptions[i] + "]"
		case attempted[i]:
			texts[i] = failedPlaceholder
		default:
			texts[i] = limitPlaceholder
		}
	}
	if attemptedCount > 0 && described == 0 {
		// Every describe ATTEMPT failed: forward the caller's own bytes and
		// let the upstream speak for itself — stripping here would silently
		// drop content on a transient outage of the describe model.
		sink.Note(fact.VisionFallbackSkipped{Reason: "describe_failed"})
		return body, nil
	}
	out, err := replaceImageTexts(proto, body, texts)
	if err != nil {
		sink.Note(fact.VisionFallbackSkipped{Reason: "patch_failed"})
		return body, nil
	}
	sink.Note(fact.VisionFallbackApplied{
		Model:     view.VisionFallbackModel(),
		Images:    len(sites),
		Described: described,
	})
	return out, nil
}

// The three placeholder texts name three different causes — the model (and
// anyone reading the conversation) should not be told "unsupported" when the
// real story was "too big" or "the describe call failed".
const (
	// strippedPlaceholder: no describe model is configured at all.
	strippedPlaceholder = "[Image omitted: this model does not accept image input]"
	// limitPlaceholder: the image fell outside the describe count/size limits.
	limitPlaceholder = "[Image omitted: over the description count or size limit]"
	// failedPlaceholder: the describe call for this image failed while others
	// succeeded.
	failedPlaceholder = "[Image description unavailable]"
)
