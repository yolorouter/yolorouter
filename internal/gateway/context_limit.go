package gateway

// The model-capability context-limit pre-gate. When the site switches it
// on, a request whose estimated input size breaks the addressed model's
// declared context window is refused at the gateway before anything is
// dialled: the upstream would refuse it anyway (burning a round trip and,
// on metered retries, quota), and the refusal the gateway writes names the
// model and the window so the caller can shrink and resend.
//
// The estimate is deliberately a seam, not a tokenizer: chars/4 is the same
// heuristic the input-compression savings reporting uses, and the tolerance
// band (margin percent) exists because a heuristic at the boundary is more
// likely to over-count than under-count. Requests inside
// (window, window*(1+margin)] pass — refuse late, never early.

import (
	"fmt"
	"net/http"

	"github.com/yolorouter/yolorouter/internal/fact"
)

// errTypeContextLengthExceeded is the caller-facing error type of the
// context-limit refusal. Unlike the errType constants aliased in error.go,
// it is not a decision-table value — no capability reports it mid-flight —
// so it lives with its only producer instead of the decision vocabulary.
const errTypeContextLengthExceeded = "context_length_exceeded"

// estimateInputTokens estimates the input-token footprint of a request
// body. It is a package-level seam so tests can pin the boundary behaviour
// with exact numbers instead of bodies engineered to land on them; the
// production reading is chars/4, the same heuristic ratio the compression
// savings estimate uses (no local tokenizer is loaded).
var estimateInputTokens = defaultEstimateInputTokens

// defaultEstimateInputTokens reads the body's byte length as a rough token
// count. Escaped JSON punctuation inflates the byte count, which the
// margin band absorbs; precision is explicitly out of scope.
func defaultEstimateInputTokens(body []byte) int {
	return len(body) / 4
}

// contextLimitExceeded is the refusal's judgement, isolated so the boundary
// arithmetic can be tested without a body at all. The block threshold is
// window*(1+margin/100); the estimate is held against it with integer math
// (estimate*100 > window*(100+margin)) so no float rounding can move the
// boundary. Reaching the threshold exactly passes — the band
// (window, threshold] is tolerance, not a refusal zone.
func contextLimitExceeded(estimated, window, marginPercent int) bool {
	return int64(estimated)*100 > int64(window)*(100+int64(marginPercent))
}

// precheckContextLimit holds the final body against the addressed model's
// declared window. Nil return means the request proceeds. The gate is
// silent unless every input is present: the site switch off, an undeclared
// window (nil), and a non-positive window are all "nothing to compare
// against" — a declaration the admin never made must not become a refusal
// the caller has to interpret. The caller-side fault is fixed: the bytes
// are the caller's, and shrinking them is the caller's remedy.
func (s *Service) precheckContextLimit(rc *Exchange, m *Model, body []byte) *Rejection {
	if !rc.settings.Capability.ContextLimitBlock {
		return nil
	}
	if m == nil || m.ContextLength == nil || *m.ContextLength <= 0 {
		return nil
	}
	estimated := estimateInputTokens(body)
	window := *m.ContextLength
	if !contextLimitExceeded(estimated, window, rc.settings.Capability.ContextMarginPercent) {
		return nil
	}
	return &Rejection{
		Status:    http.StatusRequestEntityTooLarge,
		ErrorType: errTypeContextLengthExceeded,
		Message: fmt.Sprintf("model %q has a context window of %d tokens; the estimated request size of %d tokens exceeds it",
			m.Name, window, estimated),
		FailReason: "context_length_exceeded",
		Fault:      fact.FaultClient,
	}
}
