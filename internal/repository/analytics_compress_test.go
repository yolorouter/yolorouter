package repository

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// createCompressLog inserts one request_logs row with the given created_at
// (UTC), compressors_applied, and compress_estimated_tokens_saved. Used by
// the daily-series / totals tests to build a known data set.
func createCompressLog(t *testing.T, db *gorm.DB, requestID string, createdAt time.Time, tokensSaved int, costSavedMicros int64) {
	t.Helper()
	row := &model.RequestLog{
		RequestID:                        requestID,
		ModelName:                        "test-model",
		StatusCode:                       200,
		IsStream:                         false,
		InputTokens:                      1000,
		OutputTokens:                     500,
		CostMicros:                       100,
		CostKnown:                        true,
		CompressorsApplied:               "log",
		CompressEstimatedTokensSaved:     tokensSaved,
		CompressEstimatedCostSavedMicros: costSavedMicros,
		CreatedAt:                        createdAt,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("create log %s: %v", requestID, err)
	}
}

// TestAggregateCompressDailySeriesGapFillNoDroppedBucket: a custom range
// starting mid-day and ending mid-day must still produce one bucket per
// calendar day that intersects [start, end) — the old code advanced the
// cursor by 24h from the exact start instant, dropping the tail day.
func TestAggregateCompressDailySeriesGapFillNoDroppedBucket(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	loc, _ := time.LoadLocation("Asia/Shanghai")

	// Window: 2026-07-20 14:00 -> 2026-07-22 10:00 (local). Three calendar
	// days intersect this range: 20, 21, 22. Data on each day.
	day20 := time.Date(2026, 7, 20, 10, 0, 0, 0, loc).UTC()
	day21 := time.Date(2026, 7, 21, 3, 0, 0, 0, loc).UTC()
	day22 := time.Date(2026, 7, 22, 8, 0, 0, 0, loc).UTC()

	createCompressLog(t, db, "r1", day20, 100, 200)
	createCompressLog(t, db, "r2", day21, 200, 400)
	createCompressLog(t, db, "r3", day22, 300, 600)

	start := time.Date(2026, 7, 20, 14, 0, 0, 0, loc)
	end := time.Date(2026, 7, 22, 10, 0, 0, 0, loc)
	f := &RequestLogFilter{StartTime: &start, EndTime: &end}

	// now is deliberately far from the filter window: an explicit
	// [start, end) must win over the clock, so if the implementation ever
	// let now leak into the windowing, every bucket assertion below fails.
	rows, err := AggregateCompressDailySeries(context.Background(), db, f, loc, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateCompressDailySeries: %v", err)
	}
	// Expect exactly 3 buckets: 2026-07-20, 2026-07-21, 2026-07-22.
	if len(rows) != 3 {
		t.Fatalf("expected 3 daily buckets, got %d: %+v", len(rows), rows)
	}
	want := []string{"2026-07-20", "2026-07-21", "2026-07-22"}
	for i, w := range want {
		if rows[i].Bucket != w {
			t.Errorf("bucket[%d] = %q, want %q", i, rows[i].Bucket, w)
		}
	}
	// day22 has data before end (08:00 < 10:00) and must NOT be dropped.
	if rows[2].Bucket != "2026-07-22" {
		t.Errorf("tail bucket dropped: last = %q, want 2026-07-22", rows[2].Bucket)
	}
}

// TestAggregateCompressDailySeriesSumEqualsTotals: the sum of daily
// tokens_saved across all gap-filled buckets MUST equal
// AggregateCompressTotals.tokens_saved for the same filter — this is the
// invariant the dashboard relies on (chart + card must agree).
func TestAggregateCompressDailySeriesSumEqualsTotals(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	loc := time.UTC

	// Window: 3 consecutive days, data on days 1 and 3 (day 2 is a gap).
	d1 := time.Date(2026, 7, 20, 6, 0, 0, 0, loc)
	d3 := time.Date(2026, 7, 22, 18, 0, 0, 0, loc)
	createCompressLog(t, db, "s1", d1, 500, 1000)
	createCompressLog(t, db, "s2", d3, 300, 600)

	start := time.Date(2026, 7, 20, 0, 0, 0, 0, loc)
	end := time.Date(2026, 7, 23, 0, 0, 0, 0, loc)
	f := &RequestLogFilter{StartTime: &start, EndTime: &end}

	// now is deliberately far from the filter window: an explicit
	// [start, end) must win over the clock, so if the implementation ever
	// let now leak into the windowing, every bucket assertion below fails.
	rows, err := AggregateCompressDailySeries(context.Background(), db, f, loc, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateCompressDailySeries: %v", err)
	}
	var dailySum int64
	for _, r := range rows {
		dailySum += r.TokensSaved
	}

	totals, err := AggregateCompressTotals(context.Background(), db, f)
	if err != nil {
		t.Fatalf("AggregateCompressTotals: %v", err)
	}
	if dailySum != totals.TokensSaved {
		t.Errorf("daily sum (%d) != totals (%d)", dailySum, totals.TokensSaved)
	}
}

// TestAggregateCompressDailySeriesNonUTCOffset: with a non-UTC timezone
// (UTC+8), a row at 23:00 UTC falls on the next local day. The SQL day
// expression must shift by the offset so the bucket label matches the
// gap-fill label.
func TestAggregateCompressDailySeriesNonUTCOffset(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	loc, _ := time.LoadLocation("Asia/Shanghai") // UTC+8

	// 2026-07-20 23:00 UTC = 2026-07-21 07:00 Shanghai. This row must land
	// in the 2026-07-21 bucket, NOT 2026-07-20.
	rowTime := time.Date(2026, 7, 20, 23, 0, 0, 0, time.UTC)
	createCompressLog(t, db, "tz1", rowTime, 100, 200)

	start := time.Date(2026, 7, 20, 0, 0, 0, 0, loc)
	end := time.Date(2026, 7, 23, 0, 0, 0, 0, loc)
	f := &RequestLogFilter{StartTime: &start, EndTime: &end}

	// now is deliberately far from the filter window: an explicit
	// [start, end) must win over the clock, so if the implementation ever
	// let now leak into the windowing, every bucket assertion below fails.
	rows, err := AggregateCompressDailySeries(context.Background(), db, f, loc, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateCompressDailySeries: %v", err)
	}
	// The row must be in the 2026-07-21 bucket.
	found := false
	for _, r := range rows {
		if r.Bucket == "2026-07-21" && r.TokensSaved == 100 {
			found = true
		}
		if r.Bucket == "2026-07-20" && r.TokensSaved != 0 {
			t.Errorf("row at 23:00 UTC was bucketed into 2026-07-20 (should be 2026-07-21 in UTC+8)")
		}
	}
	if !found {
		t.Errorf("row at 23:00 UTC not found in 2026-07-21 bucket (UTC+8 offset misapplied)")
	}
}

// TestAggregateCompressDailySeriesDSTBoundary: a range that crosses a DST
// transition must still produce the correct bucket count. The offset is
// fixed from window start (documented limitation — a boundary day may shift
// by one bucket, which is acceptable for a trend chart), but the gap-fill
// must not drop any bucket.
func TestAggregateCompressDailySeriesDSTBoundary(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	// Use a DST-observing timezone. America/New_York transitions from
	// EST (UTC-5) to EDT (UTC-4) in March and back in November.
	loc, _ := time.LoadLocation("America/New_York")

	// Window spanning the March 2026 DST transition (second Sunday of March
	// = March 8, 2026 at 02:00 local -> clocks spring forward to 03:00).
	// March 7 is EST (UTC-5); March 9 is EDT (UTC-4).
	start := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	end := time.Date(2026, 3, 10, 12, 0, 0, 0, loc)
	f := &RequestLogFilter{StartTime: &start, EndTime: &end}

	// No data — just verify gap-fill produces 4 buckets (7, 8, 9, 10)
	// without dropping any.
	// now is deliberately far from the filter window: an explicit
	// [start, end) must win over the clock, so if the implementation ever
	// let now leak into the windowing, every bucket assertion below fails.
	rows, err := AggregateCompressDailySeries(context.Background(), db, f, loc, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateCompressDailySeries: %v", err)
	}
	if len(rows) != 4 {
		t.Errorf("expected 4 buckets across DST boundary, got %d: %+v", len(rows), rows)
	}
	wantBuckets := []string{"2026-03-07", "2026-03-08", "2026-03-09", "2026-03-10"}
	for i, w := range wantBuckets {
		if rows[i].Bucket != w {
			t.Errorf("bucket[%d] = %q, want %q", i, rows[i].Bucket, w)
		}
	}
}

// TestAggregateCompressTotalsIncludesCacheInVolume: input_tokens stores the
// net (non-cached) count, but the compression-rate denominator must reflect
// the full prompt volume compression acts on, so total_estimated_tokens has to
// add the cache columns back. Without that, a cache-heavy request understates
// the denominator and inflates the compression rate.
func TestAggregateCompressTotalsIncludesCacheInVolume(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	at := time.Date(2026, 7, 20, 6, 0, 0, 0, time.UTC)
	row := &model.RequestLog{
		RequestID:                    "cache1",
		ModelName:                    "test-model",
		StatusCode:                   200,
		InputTokens:                  200,   // net, non-cached
		OutputTokens:                 50,    //
		CacheReadTokens:              1000,  // cached portion, not in InputTokens
		CacheWriteTokens:             300,   //
		CostKnown:                    true,  //
		CompressorsApplied:           "log", //
		CompressEstimatedTokensSaved: 100,
		CreatedAt:                    at,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	f := &RequestLogFilter{StartTime: &start, EndTime: &end}

	totals, err := AggregateCompressTotals(context.Background(), db, f)
	if err != nil {
		t.Fatalf("AggregateCompressTotals: %v", err)
	}
	// Full input volume = net input + cache read + cache write = 200+1000+300.
	if totals.TotalEstimatedTokens != 1500 {
		t.Errorf("total_estimated_tokens = %d, want 1500 (net input + cache read + cache write)", totals.TotalEstimatedTokens)
	}
}

// TestAggregateCompressorHitsCoversAllChainMembersWithCommaBoundaries: every
// compressor the chain can apply must be attributable in the stats, and the
// match must respect comma boundaries — a row that used pnpm must not also
// count as an npm hit (npm is a substring of pnpm).
func TestAggregateCompressorHitsCoversAllChainMembersWithCommaBoundaries(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	seed := func(id, applied string) {
		row := &model.RequestLog{
			RequestID: id, ModelName: "m", StatusCode: 200,
			CompressorsApplied: applied, CreatedAt: now,
		}
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("c-1", "pytest")
	seed("c-2", "vitest,log")
	seed("c-3", "pnpm")
	seed("c-4", "npm")

	rows, err := AggregateCompressorHits(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCompressorHits: %v", err)
	}
	hits := map[string]int64{}
	for _, r := range rows {
		hits[r.Name] = r.Hits
	}
	for name, want := range map[string]int64{
		"pytest": 1, "vitest": 1, "log": 1, "pnpm": 1, "npm": 1,
		"gotest": 0, "diff": 0, "grep": 0,
	} {
		got, ok := hits[name]
		if !ok {
			t.Errorf("compressor %q missing from the stats entirely", name)
			continue
		}
		if got != want {
			t.Errorf("%s hits = %d, want %d", name, got, want)
		}
	}
}
