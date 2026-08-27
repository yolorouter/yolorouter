// Package compress shrinks a caller's request body before it is dispatched.
//
// It is an ingress rewriter: it runs once, on the body the caller actually
// sent, before any candidate has been chosen. That is the only placement where
// the saving is real — a body rewritten later would already have been counted,
// parsed, and estimated in its original size.
//
// It never refuses a request. A body it cannot parse is a body the upstream may
// well accept, so every failure path returns the caller's own bytes and reports
// why nothing was done. The only thing this capability decides is whether it
// could shrink the body; what a request costs, and whether it proceeds, stay
// with the kernel.
package compress

import (
	"context"

	"github.com/yolorouter/yolorouter/internal/compress"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// View is what this capability needs to know about the exchange. The switch is
// resolved per request by the kernel (key override over instance setting), and
// the protocol selects which live zone of the body may be rewritten at all.
type View interface {
	CompressEnabled() bool
	IsChatEndpoint() bool
	IngressProtocol() protocols.ProtocolID
}

// Compressor is the capability. It holds no per-request state: everything it
// needs arrives as the view and the body.
type Compressor struct {
	// opts is captured once at construction; the engine bounds its own work
	// with opts.Timeout, so no second deadline is layered on here.
	opts compress.CompressOptions
}

// New builds the capability with the engine's default budget.
func New() *Compressor { return &Compressor{opts: compress.DefaultOptions()} }

func (*Compressor) Name() string { return "compress" }

// Applies carries the whole gate, so the kernel never copies a body for a
// request this capability was not going to touch — the common case, since
// compression ships off and most deployments leave it that way.
//
// Declining here is also why nothing is reported for those requests: not being
// asked is not the same as trying and failing, and a skip reason on every row
// of a deployment with compression switched off would read as the latter.
func (*Compressor) Applies(view View) bool {
	return view.CompressEnabled() && view.IsChatEndpoint()
}

// RewriteIngress returns the shrunk body, or the caller's own body untouched.
//
// The engine bounds its own pass with opts.Timeout rather than running on the
// request's whole budget: a compressor that overran would otherwise spend the
// caller's deadline before the first byte went upstream, and the fallback —
// send what the caller sent — is always available and always correct.
func (c *Compressor) RewriteIngress(ctx context.Context, view View, body []byte, sink fact.Sink) ([]byte, error) {
	out, res := compress.ByProtocol(ctx, view.IngressProtocol(), body, c.opts)

	if res.Skipped {
		sink.Note(fact.CompressionSkipped{Reason: string(res.SkipReason)})
		return body, nil
	}
	sink.Note(fact.TokensSaved{
		Compressors:     res.CompressorsApplied,
		EstimatedTokens: res.EstimatedTokensSaved,
	})
	return out, nil
}
