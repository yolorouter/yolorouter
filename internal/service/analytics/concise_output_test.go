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

// rateOf dereferences the nullable per-million rate, failing the test when
// the backend reported no figure at all — the assertions below all expect a
// computed one, and a nil slipping through as 0 would hide exactly the
// confusion the nullable contract exists to prevent.
func rateOf(t *testing.T, p *ConciseOutputProjection) int64 {
	t.Helper()
	if p.ProjectedSavingsPerMillionTokensMicros == nil {
		t.Fatal("projected rate is absent, want a computed figure")
	}
	return *p.ProjectedSavingsPerMillionTokensMicros
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

// TestConciseOutputProjectionPerMillionMath pins the per-million-token
// formula: projected = spend x coefficient x 1M / priced tokens — i.e. the
// traffic-weighted output price scaled by the coefficient. A dropped
// coefficient factor or a wrong divisor here changes the card's figure
// silently, so the expected value is computed from the formula itself.
func TestConciseOutputProjectionPerMillionMath(t *testing.T) {
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
	// 700K tokens at 1 CNY/M = 700000 micros spend; per 1M tokens that is
	// coefficient x 1e6 micros = the coefficient in CNY per million tokens.
	want := int64(math.Round(700_000 * ConciseOutputCoefficient * 1e6 / 700_000))
	if got := rateOf(t, p); got != want {
		t.Errorf("per-million = %d, want %d (spend x coefficient x 1M / priced tokens)",
			got, want)
	}
	if p.Coefficient != ConciseOutputCoefficient {
		t.Errorf("Coefficient echo = %v, want %v (the UI renders the rate's basis from it)",
			p.Coefficient, ConciseOutputCoefficient)
	}
}

// TestConciseOutputProjectionCarriesCoverageDenominator pins the whole
// volume roll-up through the service with four values that cannot coincide:
// unit price is not 1, and the window mixes priced traffic with heavier
// unpriced traffic. Total output tokens, priced output tokens and spend are
// therefore all different, so mapping any of them to the wrong DTO field —
// the coverage ratio divides two of them — fails here.
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
	want := int64(4 * ConciseOutputCoefficient * 1e6)
	if got := rateOf(t, p); got != want {
		t.Errorf("per-million = %d, want %d (4 CNY/M x coefficient)",
			got, want)
	}
}

// TestConciseOutputProjectionShortWindowFloor pins the sub-day floor: a
// window shorter than 24h still counts as one day in the echoed window (no
// divide-by-zero in the echo, and the per-million figure never depended on
// the day count anyway).
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
	want := int64(math.Round(100 * ConciseOutputCoefficient * 1e6 / 100))
	if got := rateOf(t, p); got != want {
		t.Errorf("per-million = %d, want %d (unit rate independent of the day count)",
			got, want)
	}
}

// TestConciseOutputProjectionTinyWindowKeepsRate pins the traffic-independence
// contract at the low end: a window whose whole priced spend rounds to zero
// micros must still report the unit rate. Dividing the ROUNDED spend here
// would print ¥0.00 over real priced traffic — exactly the "meaningless on a
// small instance" failure the per-million basis exists to avoid.
func TestConciseOutputProjectionTinyWindowKeepsRate(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	svc := seedPricedOutputRowAt(t, "p1", 1, 0.4, at)

	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	f := &repository.RequestLogFilter{StartTime: &start, EndTime: &end}

	p, err := svc.GetConciseOutputProjection(context.Background(), f, AnalyticsOptions{}, end)
	if err != nil {
		t.Fatalf("GetConciseOutputProjection: %v", err)
	}
	if p.OutputSpendMicros != 0 {
		t.Fatalf("OutputSpendMicros = %d, want 0 (0.4 micros rounds down)", p.OutputSpendMicros)
	}
	// The rate is the unit price times the coefficient regardless of volume:
	// The rate is the unit price times the coefficient: 0.4 CNY/M x 0.126.
	want := int64(math.Round(0.4 * ConciseOutputCoefficient * 1e6))
	if got := rateOf(t, p); got != want {
		t.Errorf("per-million = %d, want %d (rate must not collapse when the spend rounds to zero)",
			got, want)
	}
}

// TestConciseOutputProjectionOutOfRangeRate pins the scaled-figure guard: the
// repository keeps the spend inside int64, but scaling it to a million tokens
// multiplies by 1e6, so an absurd unit price can still leave the range — and
// an out-of-range float64 conversion is undefined in Go. The rate reports 0
// rather than an arbitrary (possibly negative) amount.
func TestConciseOutputProjectionOutOfRangeRate(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	svc := seedPricedOutputRowAt(t, "p1", 1, 2e14, at)

	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	f := &repository.RequestLogFilter{StartTime: &start, EndTime: &end}

	p, err := svc.GetConciseOutputProjection(context.Background(), f, AnalyticsOptions{}, end)
	if err != nil {
		t.Fatalf("GetConciseOutputProjection: %v", err)
	}
	// 2e14 micros x the coefficient x 1e6 lands past math.MaxInt64 (~9.22e18).
	// Absent, not zero: the window HAS priced traffic, so a 0 here would read
	// as "this saves nothing" beside a full pricing-coverage figure.
	if p.ProjectedSavingsPerMillionTokensMicros != nil {
		t.Errorf("per-million = %d, want absent (out-of-range scaled figure is never converted)",
			*p.ProjectedSavingsPerMillionTokensMicros)
	}
	if p.PricedRows != 1 {
		t.Errorf("PricedRows = %d, want 1 (the spend itself is still in range)", p.PricedRows)
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
	// No priced tokens to divide by, so there is no rate to report.
	if p.ProjectedSavingsPerMillionTokensMicros != nil {
		t.Errorf("per-million = %d, want absent on an empty window",
			*p.ProjectedSavingsPerMillionTokensMicros)
	}
	if p.Window.Days != 7 {
		t.Errorf("Window.Days = %d, want 7 (default lookback)", p.Window.Days)
	}
	if p.Window.End.Before(now.Add(-time.Hour)) {
		t.Errorf("default window end %v should sit at the current day boundary near %v", p.Window.End, now)
	}
}
