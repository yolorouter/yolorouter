package gateway

// Per-character settlement: the candidate's one per-million-characters
// price, multiplied by the count of characters the usage report carried.
// There is no tier table and no input/output split — speech has no such
// axes — so the mode prices off a single column, and the count it prices
// is the settling candidate's own billing meter (the vendor's counting
// rule), reported by the modality either from the upstream's own usage
// field or recomputed exactly from the request text.

import (
	"encoding/json"
	"math"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
)

// audioPricingSnapshot is what a per-character settlement records alongside
// the cost, so a billed row can always explain itself: what mode priced it,
// how many characters in whose meter, and the unit price applied.
type audioPricingSnapshot struct {
	BillingMode string  `json:"billing_mode"`
	Characters  int     `json:"characters"`
	UnitPrice   float64 `json:"unit_price"`
	Unit        string  `json:"unit"`
	Source      string  `json:"source"`
}

// computeAudioCost prices one audio-mode candidate's settled delivery.
//
// Unknown, not zero, in every case where no price could be resolved
// honestly: no report, no count, a report that is not counting characters
// (a defensive guard — the modality owns the unit, and a mismatched one is
// a wiring bug whose bill would be fiction), or no configured price.
// Unpriced and free are different facts, and a dashboard that adds up the
// second must not be fed the first.
func computeAudioCost(cand *model.ModelCandidate, report *fact.UsageReported) costBreakdown {
	if cand == nil || report == nil || report.Unit != fact.UnitCharacter || report.Count <= 0 {
		return costBreakdown{}
	}
	if cand.AudioUnitPrice == nil {
		return costBreakdown{}
	}
	price := *cand.AudioUnitPrice
	if price < 0 {
		return costBreakdown{}
	}
	// The price is per million characters and money stores micros, so the
	// /1e6 of the former and the ×1e6 of the latter cancel — the product
	// already is micros. Written as the product with this comment rather
	// than the divided-then-multiplied form, which rounds twice for no
	// reason (the compress-savings line in computeCost uses the same
	// cancellation).
	micros := price*float64(report.Count) + 0.5
	if micros < 0 || micros > math.MaxInt64 {
		return costBreakdown{}
	}
	snap := audioPricingSnapshot{
		BillingMode: model.BillingModeAudio,
		Characters:  report.Count,
		UnitPrice:   price,
		Unit:        report.Unit.String(),
		Source:      report.Source.String(),
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
		AudioSnapshot: string(encoded),
	}
}
