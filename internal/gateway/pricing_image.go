package gateway

// Per-image settlement: the price a tier table resolves for one request's
// quality and size, multiplied by the number of images actually delivered.
// The table lives on the candidate; the axes and the delivered count arrive
// on the usage report; the snapshot that explains the bill is built from the
// same numbers the cost was computed from.

import (
	"encoding/json"
	"math"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// imagePricingSnapshot is what a per-image settlement records alongside the
// cost, so a billed row can always explain itself: what mode priced it, the
// request's axes, what was asked versus what arrived, and the unit price the
// table resolved to.
type imagePricingSnapshot struct {
	BillingMode    string  `json:"billing_mode"`
	RequestQuality string  `json:"request_quality"`
	RequestSize    string  `json:"request_size"`
	RequestN       int     `json:"request_n"`
	ActualN        int     `json:"actual_n"`
	UnitPrice      float64 `json:"unit_price"`
	PriceSource    string  `json:"price_source"`
	Unit           string  `json:"unit"`
	Source         string  `json:"source"`
	PromptTokens   int     `json:"prompt_tokens,omitempty"`
	OutputTokens   int     `json:"output_tokens,omitempty"`
}

// computeSettlementCost prices one settled exchange, dispatching on the
// settling candidate's billing mode.
//
// The dispatch is on the CANDIDATE's declaration, not on the modality: the
// payload reports facts — a delivered count, token sub-counts — and which of
// them prices the request is configuration. A token-mode image model and an
// image-mode one can deliver the same report and must settle differently.
func computeSettlementCost(cand *model.ModelCandidate, report *fact.UsageReported, usage *protocols.IRUsage, compressSaved int) costBreakdown {
	if cand != nil && cand.BillingMode == model.BillingModeImage {
		return computeImageCost(cand, report)
	}
	return computeCost(cand, usage, compressSaved)
}

// computeImageCost prices one image-mode candidate's settled delivery.
//
// Unknown, not zero, in every case where no price could be resolved honestly:
// no report (nothing delivered, or nothing the payload could count), no
// count, no table, or a table with no match and no default. Unpriced and
// free are different facts, and a dashboard that adds up the second must not
// be fed the first.
func computeImageCost(cand *model.ModelCandidate, report *fact.UsageReported) costBreakdown {
	if cand == nil || report == nil || report.Count <= 0 {
		return costBreakdown{}
	}
	tiers := model.ParseImagePricingTiers(cand.ImagePricingTiers)
	if tiers == nil {
		return costBreakdown{}
	}
	price, source, ok := tiers.ResolvePrice(report.Quality, report.Size)
	if !ok {
		return costBreakdown{}
	}
	micros := price*float64(report.Count)*microsPerUnit + 0.5
	if micros < 0 || micros > math.MaxInt64 {
		return costBreakdown{}
	}
	snap := imagePricingSnapshot{
		BillingMode:    model.BillingModeImage,
		RequestQuality: report.Quality,
		RequestSize:    report.Size,
		RequestN:       report.Requested,
		ActualN:        report.Count,
		UnitPrice:      price,
		PriceSource:    source,
		Unit:           report.Unit.String(),
		Source:         report.Source.String(),
		PromptTokens:   report.Prompt,
		OutputTokens:   report.Completion,
	}
	encoded, err := json.Marshal(snap)
	if err != nil {
		// The snapshot is diagnostics for the bill, not the bill; a
		// snapshot that cannot serialize must not unprice a cost that
		// computed cleanly.
		encoded = nil
	}
	return costBreakdown{
		CostMicros:    int64(micros),
		Known:         true,
		ImageSnapshot: string(encoded),
	}
}
