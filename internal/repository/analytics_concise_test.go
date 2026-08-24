package repository

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedConciseModel inserts one models row plus (optionally) one candidate
// with the given output price, returning the model id. providerID 0 means
// "no candidate at all". The provider row is created on demand — the
// candidates table carries a foreign key to providers.
func seedConciseModel(t *testing.T, db *gorm.DB, name string, providerID uint, outputPrice float64) uint {
	t.Helper()
	m := model.Model{Name: name, ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover}
	if err := db.Create(&m).Error; err != nil {
		t.Fatalf("create model %s: %v", name, err)
	}
	if providerID != 0 {
		p := model.Provider{ID: providerID, Name: "provider-" + fmt.Sprint(providerID), ProviderType: "openai"}
		if err := db.Where(model.Provider{ID: providerID}).FirstOrCreate(&p).Error; err != nil {
			t.Fatalf("ensure provider %d: %v", providerID, err)
		}
		c := model.ModelCandidate{ModelID: m.ID, ProviderID: providerID,
			ProviderModelName: "upstream/" + name, OutputPrice: outputPrice}
		if err := db.Create(&c).Error; err != nil {
			t.Fatalf("create candidate for %s: %v", name, err)
		}
	}
	return m.ID
}

// seedConciseLog inserts one request_logs row with the given output volume
// and routing. providerID 0 means NULL provider_id.
func seedConciseLog(t *testing.T, db *gorm.DB, requestID, modelName string, providerID uint, outputTokens int, createdAt time.Time) {
	t.Helper()
	row := &model.RequestLog{
		RequestID:    requestID,
		ModelName:    modelName,
		ProviderID:   nil,
		StatusCode:   200,
		OutputTokens: outputTokens,
		CostKnown:    true,
		CreatedAt:    createdAt,
	}
	if providerID != 0 {
		id := providerID
		row.ProviderID = &id
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("create log %s: %v", requestID, err)
	}
}

// TestAggregatePricedOutputVolumeCoverageAndSpend pins the pricing and
// coverage rules: only (model_name, provider_id) pairs that resolve to a
// candidate with output_price > 0 contribute spend and priced rows, while
// every row with output_tokens > 0 counts toward coverage — the zero-price,
// no-candidate, wrong-provider and NULL-provider buckets are the unpriced
// cases in the wild. Rows with output_tokens = 0 are excluded entirely.
func TestAggregatePricedOutputVolumeCoverageAndSpend(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedConciseModel(t, db, "m-priced", 1, 2.5)
	seedConciseModel(t, db, "m-zero", 2, 0)
	seedConciseModel(t, db, "m-orphan", 0, 0) // 0 = model exists, no candidate at all
	// Providers 3 and 9 exist but serve no candidate for the model each of
	// their rows names: 3 is the orphan model's route, 9 is a priced model
	// routed through a provider that never offered it. Both have to exist as
	// rows because request_logs.provider_id carries an FK to providers.
	for _, id := range []uint{3, 9} {
		p := model.Provider{ID: id, Name: "provider-" + fmt.Sprint(id), ProviderType: "openai"}
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("create provider %d: %v", id, err)
		}
	}
	now := time.Now().UTC()

	seedConciseLog(t, db, "p1", "m-priced", 1, 1_000_000, now)
	seedConciseLog(t, db, "p2", "m-priced", 1, 500_000, now)
	seedConciseLog(t, db, "z1", "m-zero", 2, 100, now)
	seedConciseLog(t, db, "o1", "m-orphan", 3, 100, now)
	seedConciseLog(t, db, "w1", "m-priced", 9, 100, now) // model exists, provider has no candidate
	seedConciseLog(t, db, "n1", "m-priced", 0, 100, now) // NULL provider
	seedConciseLog(t, db, "x1", "m-priced", 1, 0, now)   // zero output: excluded

	v, err := AggregatePricedOutputVolume(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregatePricedOutputVolume: %v", err)
	}
	if v.OutputRows != 6 {
		t.Errorf("OutputRows = %d, want 6 (all rows with output_tokens > 0)", v.OutputRows)
	}
	// Coverage denominator counts TOKENS over every one of those rows, not
	// requests: 1.5M priced + four 100-token unpriced rows.
	if v.OutputTokens != 1_500_400 {
		t.Errorf("OutputTokens = %d, want 1500400 (priced + unpriced output tokens)", v.OutputTokens)
	}
	if v.PricedRows != 2 {
		t.Errorf("PricedRows = %d, want 2 (p1, p2)", v.PricedRows)
	}
	if v.PricedOutputTokens != 1_500_000 {
		t.Errorf("PricedOutputTokens = %d, want 1500000", v.PricedOutputTokens)
	}
	// 1.5M tokens x 2.5 CNY/M: per-M pricing and the micros scale cancel,
	// so the product is already micros — 3,750,000.
	if v.OutputSpendMicros != 3_750_000 {
		t.Errorf("OutputSpendMicros = %d, want 3750000", v.OutputSpendMicros)
	}
}

// TestAggregatePricedOutputVolumeRoundsOnce pins the rounding rule: spend
// is summed as float and rounded ONCE at the end, so group-level products
// like 1 x 2.6 = 2.6 don't each truncate to 2 before summing.
func TestAggregatePricedOutputVolumeRoundsOnce(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedConciseModel(t, db, "m-frac", 5, 2.6)
	now := time.Now().UTC()
	seedConciseLog(t, db, "f1", "m-frac", 5, 1, now)
	seedConciseLog(t, db, "f2", "m-frac", 5, 1, now)

	v, err := AggregatePricedOutputVolume(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregatePricedOutputVolume: %v", err)
	}
	// Same group, so SUM(tokens)=2 x 2.6 = 5.2 -> 5. Rounding per row first
	// would give 3+3=6; rounding once after the sum gives 5.
	if v.OutputSpendMicros != 5 {
		t.Errorf("OutputSpendMicros = %d, want 5 (round after summing)", v.OutputSpendMicros)
	}
}

// TestAggregatePricedOutputVolumeKeepsExactSpend pins that the unrounded
// total survives alongside the rounded one: a window holding a few sub-micro
// tokens rounds to 0 micros, and the per-million rate divides by the token
// count, so dropping the exact figure would collapse a real unit rate to
// zero on a lightly-used instance.
func TestAggregatePricedOutputVolumeKeepsExactSpend(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedConciseModel(t, db, "m-cheap", 1, 0.4)
	now := time.Now().UTC()
	seedConciseLog(t, db, "c1", "m-cheap", 1, 1, now)

	v, err := AggregatePricedOutputVolume(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregatePricedOutputVolume: %v", err)
	}
	// 1 token x 0.4 CNY/M = 0.4 micros: rounds to zero, exact keeps the 0.4.
	if v.OutputSpendMicros != 0 {
		t.Errorf("OutputSpendMicros = %d, want 0 (0.4 rounds down)", v.OutputSpendMicros)
	}
	if math.Abs(v.OutputSpendMicrosExact-0.4) > 1e-9 {
		t.Errorf("OutputSpendMicrosExact = %v, want 0.4 (the pre-rounding total)", v.OutputSpendMicrosExact)
	}
}

// TestAggregatePricedOutputVolumeSkipsOutOfRangePrice pins the overflow
// guard: unit prices are only validated as non-negative, and an absurd one
// times a real token count leaves the int64 micros range — where a float64
// conversion is undefined in Go and can yield a negative saving. Such a
// group must be treated like a missing price: out of the spend, still in the
// coverage denominator.
func TestAggregatePricedOutputVolumeSkipsOutOfRangePrice(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedConciseModel(t, db, "m-absurd", 1, 1e300)
	seedConciseModel(t, db, "m-sane", 2, 2.0)
	now := time.Now().UTC()
	seedConciseLog(t, db, "a1", "m-absurd", 1, 1_000_000, now)
	seedConciseLog(t, db, "s1", "m-sane", 2, 1_000_000, now)

	v, err := AggregatePricedOutputVolume(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregatePricedOutputVolume: %v", err)
	}
	if v.OutputSpendMicros != 2_000_000 {
		t.Errorf("OutputSpendMicros = %d, want 2000000 (only the sane group)", v.OutputSpendMicros)
	}
	if v.PricedRows != 1 || v.PricedOutputTokens != 1_000_000 {
		t.Errorf("priced totals: got rows=%d tokens=%d, want 1/1000000", v.PricedRows, v.PricedOutputTokens)
	}
	if v.OutputRows != 2 || v.OutputTokens != 2_000_000 {
		t.Errorf("coverage denominator: got rows=%d tokens=%d, want 2/2000000 (the skipped group still counts toward coverage)",
			v.OutputRows, v.OutputTokens)
	}
}

// TestAggregatePricedOutputVolumeRespectsFilter pins that the shared filter
// shape reaches this aggregate too: a user filter and a time window narrow
// the rows, and a window with no rows returns all-zero totals (not an
// error, not nil).
func TestAggregatePricedOutputVolumeRespectsFilter(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedConciseModel(t, db, "m-priced", 1, 1.0)
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)

	uid := uint(7)
	pid := uint(1)
	inside := &model.RequestLog{RequestID: "mine", ModelName: "m-priced",
		UserID: &uid, ProviderID: &pid, StatusCode: 200, OutputTokens: 100, CreatedAt: now}
	theirs := &model.RequestLog{RequestID: "theirs", ModelName: "m-priced",
		ProviderID: &pid, StatusCode: 200, OutputTokens: 900, CreatedAt: now}
	stale := &model.RequestLog{RequestID: "stale", ModelName: "m-priced",
		UserID: &uid, ProviderID: &pid, StatusCode: 200, OutputTokens: 10_000, CreatedAt: old}
	for _, r := range []*model.RequestLog{inside, theirs, stale} {
		p := *r
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("create log %s: %v", r.RequestID, err)
		}
	}

	v, err := AggregatePricedOutputVolume(context.Background(), db, &RequestLogFilter{UserID: &uid})
	if err != nil {
		t.Fatalf("user-filtered aggregate: %v", err)
	}
	if v.OutputRows != 2 || v.PricedRows != 2 || v.OutputSpendMicros != 10_100 {
		t.Errorf("user filter: got rows=%d priced=%d spend=%d, want 2/2/10100 (mine+stale)", v.OutputRows, v.PricedRows, v.OutputSpendMicros)
	}

	start := now.Add(-time.Hour)
	end := now.Add(time.Hour)
	v, err = AggregatePricedOutputVolume(context.Background(), db, &RequestLogFilter{StartTime: &start, EndTime: &end})
	if err != nil {
		t.Fatalf("time-windowed aggregate: %v", err)
	}
	if v.OutputRows != 2 || v.OutputSpendMicros != 1_000 {
		t.Errorf("time window: got rows=%d spend=%d, want 2/1000 (mine+theirs only)", v.OutputRows, v.OutputSpendMicros)
	}

	emptyStart := now.Add(time.Hour)
	emptyEnd := now.Add(2 * time.Hour)
	v, err = AggregatePricedOutputVolume(context.Background(), db, &RequestLogFilter{StartTime: &emptyStart, EndTime: &emptyEnd})
	if err != nil {
		t.Fatalf("empty-window aggregate: %v", err)
	}
	if v == nil || v.OutputRows != 0 || v.PricedRows != 0 || v.OutputSpendMicros != 0 {
		t.Errorf("empty window: got %+v, want all-zero non-nil volume", v)
	}
}
