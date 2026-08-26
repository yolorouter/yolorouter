package analytics

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedPricedOutputRow inserts a model with a priced candidate plus one
// request log carrying the given output volume, all inside any reasonable
// test window, and returns the service bound to the same db.
func seedPricedOutputRow(t *testing.T, requestID string, outputTokens int, at time.Time) *AnalyticsService {
	t.Helper()
	return seedPricedOutputRowAt(t, requestID, outputTokens, 1.0, at)
}

// seedPricedOutputRowAt is seedPricedOutputRow with the candidate's output
// price spelled out, for the cases that turn on the price itself.
func seedPricedOutputRowAt(t *testing.T, requestID string, outputTokens int, outputPrice float64, at time.Time) *AnalyticsService {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	m := model.Model{Name: "m-priced", ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover}
	if err := db.Create(&m).Error; err != nil {
		t.Fatalf("create model: %v", err)
	}
	p := model.Provider{ID: 1, Name: "provider-1", ProviderType: "openai"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	providerID := uint(1)
	c := model.ModelCandidate{ModelID: m.ID, ProviderID: providerID,
		ProviderModelName: "upstream/m-priced", OutputPrice: outputPrice}
	if err := db.Create(&c).Error; err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	log := model.RequestLog{RequestID: requestID, ModelName: "m-priced",
		ProviderID: &providerID, StatusCode: 200, OutputTokens: outputTokens, CreatedAt: at}
	if err := repository.CreateRequestLog(db, &log); err != nil {
		t.Fatalf("seed log %s: %v", requestID, err)
	}
	return NewAnalyticsService(db)
}

// TestConciseOutputCoefficientMatchesPublishedBenchmark is the one place the
// coefficient is checked against a literal rather than against itself. Every
// other assertion in this package derives its expected figure FROM the
// constant, so a typo in it would move implementation and tests together and
// stay green — while the console, the README and the published benchmark all
// went on quoting the old number. This test is what makes changing the
// constant a deliberate act: the literal here and the median in
// docs/concise-output-benchmark.md have to move together.
func TestConciseOutputCoefficientMatchesPublishedBenchmark(t *testing.T) {
	const published = 0.126 // median of the 150 measured pairs, 2026-08-24
	if ConciseOutputCoefficient != published {
		t.Fatalf("ConciseOutputCoefficient = %v, want %v — if the benchmark was re-run, "+
			"update this literal and the published benchmark documents in the same change",
			ConciseOutputCoefficient, published)
	}
}

// TestConciseOutputProjectionPeriodTotals pins the two period-total formulas:
// saved cost = window spend x coefficient, saved tokens = window priced
// output tokens x coefficient. A dropped coefficient factor or a swapped
// basis changes the card's figures silently, so the expected values are
// computed from the formulas themselves.
func TestConciseOutputProjectionPeriodTotals(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	svc := seedPricedOutputRow(t, "p1", 700_000, at)

	start := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	f := &repository.RequestLogFilter{StartTime: &start, EndTime: &end}

	p, err := svc.GetConciseOutputProjection(context.Background(), f, AnalyticsOptions{}, now)
	if err != nil {
		t.Fatalf("GetConciseOutputProjection: %v", err)
	}
	if p.Window.Start != start || p.Window.End != end {
		t.Errorf("window echo: got [%v, %v), want the resolved explicit range", p.Window.Start, p.Window.End)
	}
	if p.Window.Days != 7 {
		t.Errorf("Window.Days = %d, want 7", p.Window.Days)
	}
	if p.OutputSpendMicros != 700_000 || p.OutputRows != 1 || p.PricedRows != 1 || p.PricedOutputTokens != 700_000 {
		t.Fatalf("volume: got spend=%d rows=%d priced=%d tokens=%d, want 700000/1/1/700000",
			p.OutputSpendMicros, p.OutputRows, p.PricedRows, p.PricedOutputTokens)
	}
	// 700K tokens at 1 CNY/M = 700000 micros spend for the whole window.
	wantCost := int64(math.Round(700_000 * ConciseOutputCoefficient))
	if p.ProjectedSavedCostMicros != wantCost {
		t.Errorf("saved cost = %d, want %d (window spend x coefficient)",
			p.ProjectedSavedCostMicros, wantCost)
	}
	wantTokens := int64(math.Round(700_000 * ConciseOutputCoefficient))
	if p.ProjectedSavedOutputTokens != wantTokens {
		t.Errorf("saved tokens = %d, want %d (priced output tokens x coefficient)",
			p.ProjectedSavedOutputTokens, wantTokens)
	}
	if p.Coefficient != ConciseOutputCoefficient {
		t.Errorf("Coefficient echo = %v, want %v (the UI renders the figures' basis from it)",
			p.Coefficient, ConciseOutputCoefficient)
	}
	// Deprecated per-million rate stays on the wire for pre-upgrade tabs,
	// with its original formula: spend x coefficient x 1M / priced tokens.
	wantLegacy := int64(math.Round(700_000 * ConciseOutputCoefficient * 1e6 / 700_000))
	if p.ProjectedSavingsPerMillionTokensMicros == nil {
		t.Fatalf("legacy per-million rate absent, want %d (pre-upgrade tabs still read it)", wantLegacy)
	}
	if *p.ProjectedSavingsPerMillionTokensMicros != wantLegacy {
		t.Errorf("legacy per-million rate = %d, want %d",
			*p.ProjectedSavingsPerMillionTokensMicros, wantLegacy)
	}
}

// TestConciseOutputProjectionCarriesCoverageDenominator pins the whole
// volume roll-up through the service with values that cannot coincide:
// unit price is not 1, and the window mixes priced traffic with heavier
// unpriced traffic. Total output tokens, priced output tokens and spend are
// therefore all different, so mapping any of them to the wrong DTO field —
// or scaling the saved-token figure from the unpriced total — fails here.
func TestConciseOutputProjectionCarriesCoverageDenominator(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	svc := seedPricedOutputRowAt(t, "p1", 250_000, 4.0, at)
	// Same model name, never routed: unpriced, and four times the volume.
	unpriced := model.RequestLog{RequestID: "u1", ModelName: "m-priced",
		StatusCode: 200, OutputTokens: 1_000_000, CreatedAt: at}
	if err := repository.CreateRequestLog(svc.db, &unpriced); err != nil {
		t.Fatalf("seed unpriced log: %v", err)
	}

	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	f := &repository.RequestLogFilter{StartTime: &start, EndTime: &end}

	p, err := svc.GetConciseOutputProjection(context.Background(), f, AnalyticsOptions{}, end)
	if err != nil {
		t.Fatalf("GetConciseOutputProjection: %v", err)
	}
	if p.OutputRows != 2 || p.PricedRows != 1 {
		t.Errorf("rows = %d total / %d priced, want 2/1", p.OutputRows, p.PricedRows)
	}
	if p.OutputTokens != 1_250_000 {
		t.Errorf("OutputTokens = %d, want 1250000 (priced + unpriced — the coverage denominator)", p.OutputTokens)
	}
	if p.PricedOutputTokens != 250_000 {
		t.Errorf("PricedOutputTokens = %d, want 250000", p.PricedOutputTokens)
	}
	if p.OutputSpendMicros != 1_000_000 {
		t.Errorf("OutputSpendMicros = %d, want 1000000 (250K x 4 CNY/M)", p.OutputSpendMicros)
	}
	wantCost := int64(math.Round(1_000_000 * ConciseOutputCoefficient))
	if p.ProjectedSavedCostMicros != wantCost {
		t.Errorf("saved cost = %d, want %d (priced spend x coefficient)",
			p.ProjectedSavedCostMicros, wantCost)
	}
	// Scaled from the 250K PRICED tokens, never the 1.25M total: the unpriced
	// million must not inflate the saved-token figure.
	wantTokens := int64(math.Round(250_000 * ConciseOutputCoefficient))
	if p.ProjectedSavedOutputTokens != wantTokens {
		t.Errorf("saved tokens = %d, want %d (PRICED output tokens x coefficient)",
			p.ProjectedSavedOutputTokens, wantTokens)
	}
}

// TestConciseOutputProjectionShortWindowFloor pins the sub-day floor: a
// window shorter than 24h still counts as one day in the echoed window (no
// divide-by-zero in the echo, and the period totals never depended on the
// day count anyway).
func TestConciseOutputProjectionShortWindowFloor(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	svc := seedPricedOutputRow(t, "p1", 100, at)

	start := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)
	f := &repository.RequestLogFilter{StartTime: &start, EndTime: &end}

	p, err := svc.GetConciseOutputProjection(context.Background(), f, AnalyticsOptions{}, start)
	if err != nil {
		t.Fatalf("GetConciseOutputProjection: %v", err)
	}
	if p.Window.Days != 1 {
		t.Errorf("Window.Days = %d, want 1 (sub-day window floors at one day)", p.Window.Days)
	}
	want := int64(math.Round(100 * ConciseOutputCoefficient))
	if p.ProjectedSavedCostMicros != want {
		t.Errorf("saved cost = %d, want %d (totals independent of the day count)",
			p.ProjectedSavedCostMicros, want)
	}
	if p.ProjectedSavedOutputTokens != want {
		t.Errorf("saved tokens = %d, want %d (100 tokens x coefficient)",
			p.ProjectedSavedOutputTokens, want)
	}
}

// TestConciseOutputProjectionScalesUnroundedSpend pins the unrounded basis:
// the saved cost scales the exact spend, not the spend rounded to whole
// micros. The fixture is chosen so the two roundings land on different
// integers — one token at 3.9 CNY/M is an exact spend of 3.9 micros
// (reported rounded as 4), and 3.9 x coefficient rounds to a different
// figure than 4 x coefficient. Scaling the rounded spend turns this red.
func TestConciseOutputProjectionScalesUnroundedSpend(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	svc := seedPricedOutputRowAt(t, "p1", 1, 3.9, at)

	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	f := &repository.RequestLogFilter{StartTime: &start, EndTime: &end}

	p, err := svc.GetConciseOutputProjection(context.Background(), f, AnalyticsOptions{}, end)
	if err != nil {
		t.Fatalf("GetConciseOutputProjection: %v", err)
	}
	if p.OutputSpendMicros != 4 {
		t.Fatalf("OutputSpendMicros = %d, want 4 (3.9 micros exact, rounded on report)", p.OutputSpendMicros)
	}
	want := int64(math.Round(3.9 * ConciseOutputCoefficient))
	fromRounded := int64(math.Round(float64(p.OutputSpendMicros) * ConciseOutputCoefficient))
	if want == fromRounded {
		t.Fatal("fixture no longer discriminates exact from rounded spend — pick a new price")
	}
	if p.ProjectedSavedCostMicros != want {
		t.Errorf("saved cost = %d, want %d (must scale the exact spend, not the rounded report)",
			p.ProjectedSavedCostMicros, want)
	}
}

// TestConciseOutputProjectionAbsurdPriceStaysInRange backs the no-range-guard
// claim in the implementation: scaling never grows the spend, so even a spend
// near the top of the repository's range yields a finite, positive figure
// strictly below it. Reintroducing any amplification step (the kind a
// per-token normalization would need) pushes this fixture out of int64 and
// turns the assertion red.
func TestConciseOutputProjectionAbsurdPriceStaysInRange(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	svc := seedPricedOutputRowAt(t, "p1", 1, 2e14, at)

	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	f := &repository.RequestLogFilter{StartTime: &start, EndTime: &end}

	p, err := svc.GetConciseOutputProjection(context.Background(), f, AnalyticsOptions{}, end)
	if err != nil {
		t.Fatalf("GetConciseOutputProjection: %v", err)
	}
	want := int64(math.Round(2e14 * ConciseOutputCoefficient))
	if p.ProjectedSavedCostMicros != want {
		t.Errorf("saved cost = %d, want %d (spend x coefficient, no amplification)",
			p.ProjectedSavedCostMicros, want)
	}
	if p.ProjectedSavedCostMicros <= 0 || p.ProjectedSavedCostMicros >= 2e14 {
		t.Errorf("saved cost = %d, want a positive figure strictly below the 2e14 spend",
			p.ProjectedSavedCostMicros)
	}
	// The legacy per-million rate's 1e6 scale-up DOES leave int64 here, so
	// its original absent-not-zero contract must hold for pre-upgrade tabs.
	if p.ProjectedSavingsPerMillionTokensMicros != nil {
		t.Errorf("legacy per-million rate = %d, want absent (scaled figure out of int64 range)",
			*p.ProjectedSavingsPerMillionTokensMicros)
	}
}

// TestConciseOutputWindowDaysDST pins the nearest-day rounding: a calendar
// week crossing a DST transition spans 167 or 169 actual hours, and the
// echoed day count must stay 7 — a caption reading "8 days" for the week the
// user picked would just look wrong. A sub-day window still floors at one day.
func TestConciseOutputWindowDaysDST(t *testing.T) {
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	if got := windowDays(base, base.Add(169*time.Hour)); got != 7 {
		t.Errorf("windowDays(169h) = %d, want 7", got)
	}
	if got := windowDays(base, base.Add(167*time.Hour)); got != 7 {
		t.Errorf("windowDays(167h) = %d, want 7", got)
	}
	if got := windowDays(base, base.Add(2*time.Hour)); got != 1 {
		t.Errorf("windowDays(2h) = %d, want 1 (sub-day floor)", got)
	}
}

// TestConciseOutputProjectionEmptyWindow pins the no-traffic shape: zero
// totals (never an error), and the default 7-day window resolved when the
// caller supplied none.
func TestConciseOutputProjectionEmptyWindow(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewAnalyticsService(db)
	now := time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC)

	p, err := svc.GetConciseOutputProjection(context.Background(), &repository.RequestLogFilter{}, AnalyticsOptions{}, now)
	if err != nil {
		t.Fatalf("GetConciseOutputProjection on empty db: %v", err)
	}
	if p.OutputSpendMicros != 0 || p.OutputRows != 0 || p.PricedRows != 0 || p.PricedOutputTokens != 0 ||
		p.OutputTokens != 0 {
		t.Errorf("empty db: got %+v, want all-zero totals", p)
	}
	if p.ProjectedSavedCostMicros != 0 || p.ProjectedSavedOutputTokens != 0 {
		t.Errorf("saved totals = %d micros / %d tokens, want 0/0 on an empty window",
			p.ProjectedSavedCostMicros, p.ProjectedSavedOutputTokens)
	}
	// No priced tokens to divide by, so the legacy rate has nothing to report.
	if p.ProjectedSavingsPerMillionTokensMicros != nil {
		t.Errorf("legacy per-million rate = %d, want absent on an empty window",
			*p.ProjectedSavingsPerMillionTokensMicros)
	}
	if p.Window.Days != 7 {
		t.Errorf("Window.Days = %d, want 7 (default lookback)", p.Window.Days)
	}
	if p.Window.End.Before(now.Add(-time.Hour)) {
		t.Errorf("default window end %v should sit at the current day boundary near %v", p.Window.End, now)
	}
}
