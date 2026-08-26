// Concise-output projection: the priced output-volume roll-up and
// projected period savings behind the cost-optimization page's
// concise-output card. Companion to the compress stats in
// analytics_service.go — same filter shape, same resolveEffectiveRange
// windowing; the arithmetic that turns the volume into the window's
// projected savings lives here.
package analytics

import (
	"context"
	"math"
	"time"

	"github.com/yolorouter/yolorouter/internal/repository"
)

// ConciseOutputCoefficient is the single global factory coefficient behind
// the projected-savings figure: the median output-token reduction of the
// concise-output switch. One number for every model and instance — per-model
// tables age quickly and invite "why is my model in that band" questions an
// estimate cannot settle. The projection assumes the custom system prompt
// actually asks for concise output; an unrelated prompt text voids the figure
// (surfaced as help text in the UI).
//
// Measured 2026-08-24: 10 fixed Chinese questions x 3 rounds, paired on/off
// through a live gateway, with the switch installing exactly the prompt the
// console writes (both sentences, in Chinese — the English wording an
// English console writes is not in these measurements). Across 5 models
// (claude-opus-4-7, deepseek-v4-flash, deepseek-v4-pro, glm-5.1,
// qwen3.5-flash; default sampling parameters). Median of the 150 pairs =
// 12.6%. Every model's median is positive (+3.6% to +27.4%), but the spread
// is wide and real: quartiles [-4.0%, +27.2%], 44 of the 150 pairs negative,
// and one model/question cell swung from -193% to +46% between rounds. The
// median is the honest single-number summary of that distribution — a
// per-request promise it is not. The full methodology and the complete
// 150-pair raw data are published at
// https://github.com/yolorouter/yolorouter/blob/main/docs/concise-output-benchmark.md
const ConciseOutputCoefficient = 0.126

// ConciseOutputWindow echoes the resolved [start, end) window the projection
// aggregated — the range the price weighting came from. The console does not
// render it today; it is here so the response says which window produced the
// figure instead of leaving a caller to re-derive it from its own filter.
type ConciseOutputWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Days  int       `json:"days"`
}

// ConciseOutputProjection is the GET /analytics/concise-output-projection
// body. OutputSpendMicros / OutputRows / PricedRows / PricedOutputTokens
// come straight from repository.AggregatePricedOutputVolume (current-price
// recomputation, see that function). The card's figures are the two
// projected period totals: the window's priced spend and priced output
// tokens scaled by the coefficient. The window's traffic can mix requests
// with the switch on and off (per-key overrides beat the global switch), so
// the figures make no on/off claim and the UI labels them neutrally as
// projections. PricedOutputTokens < OutputTokens means some output traffic
// was unpriced and is not in the figures; their ratio is the coverage the
// UI reports.
type ConciseOutputProjection struct {
	Window             ConciseOutputWindow `json:"window"`
	OutputSpendMicros  int64               `json:"output_spend_micros"`
	OutputRows         int64               `json:"output_rows"`
	OutputTokens       int64               `json:"output_tokens"`
	PricedRows         int64               `json:"priced_rows"`
	PricedOutputTokens int64               `json:"priced_output_tokens"`
	// ProjectedSavedCostMicros = the window's priced output spend x
	// coefficient, in micros. Scaled from the UNROUNDED spend: rounding to
	// whole micros first would bake the discarded fraction into the figure.
	// A window whose scaled spend rounds to zero genuinely saved nothing
	// worth a cent — the UI's empty/unpriced explanatory states cover the
	// no-data cases before this figure renders.
	ProjectedSavedCostMicros int64 `json:"projected_saved_cost_micros"`
	// ProjectedSavedOutputTokens = the window's priced output tokens x
	// coefficient — the volume counterpart of the cost figure, on the same
	// priced-only basis.
	ProjectedSavedOutputTokens int64 `json:"projected_saved_output_tokens"`
	// Deprecated: the per-million unit rate the card rendered before the
	// period totals above. Console tabs loaded before a server upgrade still
	// read it — without it their null check passes undefined through and the
	// card prints NaN — so it ships alongside the period totals in the first
	// release after 0.1.8, slated for deletion in the release after that. Old
	// contract unchanged: spend x coefficient scaled to 1M output tokens,
	// nil when there is no priced traffic or the scaled figure leaves int64.
	// New consumers must read the period totals instead.
	ProjectedSavingsPerMillionTokensMicros *int64 `json:"projected_savings_per_million_tokens_micros"`
	// Coefficient echoes the factory coefficient behind the figures so the
	// UI renders their basis from the single backend source of truth
	// instead of hard-coding a copy that could drift.
	Coefficient float64 `json:"coefficient"`
}

// GetConciseOutputProjection aggregates the priced output volume for the
// filter and projects the window's saved cost and saved output tokens. The
// window resolves on the day-bucket cap like the compress stats, so this
// card and the compression card beside it aggregate the same range for a
// given filter. The same figures serve every switch state — per-key
// overrides can mix enabled and disabled traffic in one window, so the
// projection stays state-agnostic.
func (s *AnalyticsService) GetConciseOutputProjection(ctx context.Context, filter *repository.RequestLogFilter, opts AnalyticsOptions, now time.Time) (*ConciseOutputProjection, error) {
	resolveEffectiveRange(filter, opts, BucketDay, now)
	volume, err := repository.AggregatePricedOutputVolume(ctx, s.db, filter)
	if err != nil {
		return nil, err
	}
	// Scales the UNROUNDED spend so the discarded sub-micro fraction never
	// bakes into the figure. No range guard: the repository keeps the spend
	// inside int64, and the coefficient only shrinks it.
	savedCost := int64(math.Round(volume.OutputSpendMicrosExact * ConciseOutputCoefficient))
	savedTokens := int64(math.Round(float64(volume.PricedOutputTokens) * ConciseOutputCoefficient))
	// The deprecated per-million rate keeps its original guards, which the
	// totals no longer need: the 1e6 scale-up can leave int64 on an absurd
	// unit price, and an out-of-range float64 conversion is undefined in Go.
	// nil (never 0) marks "could not compute".
	var perMillion *int64
	if volume.PricedOutputTokens > 0 {
		scaled := math.Round(
			volume.OutputSpendMicrosExact * ConciseOutputCoefficient * 1e6 / float64(volume.PricedOutputTokens))
		if !math.IsNaN(scaled) && scaled < math.MaxInt64 {
			rate := int64(scaled)
			perMillion = &rate
		}
	}
	return &ConciseOutputProjection{
		Window: ConciseOutputWindow{
			Start: *filter.StartTime,
			End:   *filter.EndTime,
			Days:  windowDays(*filter.StartTime, *filter.EndTime),
		},
		OutputSpendMicros:                      volume.OutputSpendMicros,
		OutputRows:                             volume.OutputRows,
		OutputTokens:                           volume.OutputTokens,
		PricedRows:                             volume.PricedRows,
		PricedOutputTokens:                     volume.PricedOutputTokens,
		ProjectedSavedCostMicros:               savedCost,
		ProjectedSavedOutputTokens:             savedTokens,
		ProjectedSavingsPerMillionTokensMicros: perMillion,
		Coefficient:                            ConciseOutputCoefficient,
	}, nil
}

// windowDays renders the span of [start, end) in whole 24h days, floored at
// 1 so a sub-day window still reads as one day. It is caption metadata only —
// the projected totals never divide by it. Rounded to the NEAREST day
// rather than up: ResolveTimeRange builds calendar windows in
// the client's timezone, so a 7-day window crossing a DST transition spans
// 167 or 169 actual hours, and a caption reading "8 days" for the week the
// user picked would just look wrong.
func windowDays(start, end time.Time) int {
	days := int(math.Round(end.Sub(start).Hours() / 24))
	if days < 1 {
		return 1
	}
	return days
}
