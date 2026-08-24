// Concise-output projection: the priced output-volume roll-up and
// per-million-token projection behind the cost-optimization page's
// concise-output card. Companion to the compress stats in
// analytics_service.go — same filter shape, same resolveEffectiveRange
// windowing; the arithmetic that turns the volume into a projected
// per-million-token figure lives here.
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
// recomputation, see that function); ProjectedSavingsPerMillionTokensMicros
// is the card's figure: spend x coefficient normalized to one million
// output tokens. The per-million basis is traffic-independent — extrapolating
// a monthly total reads as cents on a lightly-used instance, while a unit
// rate stays meaningful at any volume. PricedOutputTokens < OutputTokens
// means some output traffic was unpriced and is not in the figure; their
// ratio is the coverage the UI reports.
type ConciseOutputProjection struct {
	Window             ConciseOutputWindow `json:"window"`
	OutputSpendMicros  int64               `json:"output_spend_micros"`
	OutputRows         int64               `json:"output_rows"`
	OutputTokens       int64               `json:"output_tokens"`
	PricedRows         int64               `json:"priced_rows"`
	PricedOutputTokens int64               `json:"priced_output_tokens"`
	// ProjectedSavingsPerMillionTokensMicros = spend x coefficient over the
	// priced token total, scaled to 1M tokens, in micros. NULL when the rate
	// cannot be computed — no priced traffic, or a unit price so large the
	// scaled figure leaves the int64 range. Deliberately nullable rather than
	// 0: with priced rows present, a zero would render as a legitimate
	// "¥0.00 saved" next to full pricing coverage, which is a different (and
	// false) claim from "this could not be computed".
	ProjectedSavingsPerMillionTokensMicros *int64 `json:"projected_savings_per_million_tokens_micros"`
	// Coefficient echoes the factory coefficient behind the figure so the
	// UI renders the rate's basis from the single backend source of truth
	// instead of hard-coding a copy that could drift.
	Coefficient float64 `json:"coefficient"`
}

// GetConciseOutputProjection aggregates the priced output volume for the
// filter and projects it to a per-million-token saving. The window resolves
// on the day-bucket cap like the compress stats, so this card and the
// compression card beside it aggregate the same range for a given filter (the
// window's traffic sets the price weighting). The same figure serves both
// switch states — enabled reads as "with the switch on", disabled as "if
// enabled" — the UI picks the wording.
func (s *AnalyticsService) GetConciseOutputProjection(ctx context.Context, filter *repository.RequestLogFilter, opts AnalyticsOptions, now time.Time) (*ConciseOutputProjection, error) {
	resolveEffectiveRange(filter, opts, BucketDay, now)
	volume, err := repository.AggregatePricedOutputVolume(ctx, s.db, filter)
	if err != nil {
		return nil, err
	}
	// Divides the UNROUNDED spend: rounding to whole micros first and then
	// scaling back up to a million tokens re-amplifies the discarded
	// fraction, which on a window of a few sub-micro tokens turns a real rate
	// into ¥0.00. The rate must not depend on how much traffic the window
	// happened to hold.
	//
	// The range check mirrors the repository's. An absurd unit price can push
	// the scaled figure out of int64 even when the spend itself fits, and an
	// out-of-range float64 conversion is undefined in Go. Rather than convert
	// it, the rate is left absent — the caller renders that as "no figure",
	// never as a saving of zero.
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
		ProjectedSavingsPerMillionTokensMicros: perMillion,
		Coefficient:                            ConciseOutputCoefficient,
	}, nil
}

// windowDays renders the span of [start, end) in whole 24h days, floored at
// 1 so a sub-day window still reads as one day. It is caption metadata only —
// the per-million rate is a unit figure and never divides by it. Rounded to
// the NEAREST day rather than up: ResolveTimeRange builds calendar windows in
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
