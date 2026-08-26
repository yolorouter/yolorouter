package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedCacheProvider ensures one provider row and one candidate carrying (or
// not carrying) a configured cache price. Whether a provider counts as
// cache-capable is decided from candidate pricing plus observed metering, so
// the tests need both knobs.
func seedCacheProvider(t *testing.T, db *gorm.DB, providerID uint, withCachePrice bool) {
	t.Helper()
	p := model.Provider{ID: providerID, Name: "provider-" + fmt.Sprint(providerID), ProviderType: "openai"}
	if err := db.Where(model.Provider{ID: providerID}).FirstOrCreate(&p).Error; err != nil {
		t.Fatalf("ensure provider %d: %v", providerID, err)
	}
	m := model.Model{Name: fmt.Sprintf("cache-model-%d", providerID), ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover}
	if err := db.Create(&m).Error; err != nil {
		t.Fatalf("create model: %v", err)
	}
	c := model.ModelCandidate{ModelID: m.ID, ProviderID: providerID,
		ProviderModelName: "upstream/" + m.Name, InputPrice: 3.0, OutputPrice: 6.0}
	if withCachePrice {
		read, write := 0.3, 3.75
		c.CacheReadPrice = &read
		c.CacheWritePrice = &write
	}
	if err := db.Create(&c).Error; err != nil {
		t.Fatalf("create candidate: %v", err)
	}
}

// seedCacheLog inserts one request_logs row with the given cache shape.
// input tokens here are the persisted NET (uncached) count, matching what the
// gateway writes.
func seedCacheLog(t *testing.T, db *gorm.DB, requestID string, providerID uint,
	uncachedInput, cacheRead, cacheWrite int, readSaved, writeExtra int64, createdAt time.Time,
) {
	t.Helper()
	pid := providerID
	row := &model.RequestLog{
		RequestID:             requestID,
		ModelName:             "gpt-4o",
		ProviderID:            &pid,
		StatusCode:            200,
		InputTokens:           uncachedInput,
		OutputTokens:          10,
		CacheReadTokens:       cacheRead,
		CacheWriteTokens:      cacheWrite,
		CacheReadSavedMicros:  readSaved,
		CacheWriteExtraMicros: writeExtra,
		CostMicros:            100,
		CostKnown:             true,
		CreatedAt:             createdAt,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("create log %s: %v", requestID, err)
	}
}

// TestAggregateCacheStatsIncludesOnlyCapableProviders: totals sum only the
// rows of providers that both reported cache metering and carry a configured
// cache price — a provider that never reported a cache token must not pour
// its uncached input into the hit-rate denominator (that dilution is exactly
// the fake-0% failure this feature exists to avoid), and it must be listed as
// unsupported instead of silently missing.
func TestAggregateCacheStatsIncludesOnlyCapableProviders(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	seedCacheProvider(t, db, 1, true)  // metered + priced -> included
	seedCacheProvider(t, db, 2, true)  // priced but never metered -> unsupported
	seedCacheProvider(t, db, 3, false) // metered but no cache price -> unsupported

	seedCacheLog(t, db, "r-in-1", 1, 100, 900, 50, 2_430_000, 37_500, now)
	seedCacheLog(t, db, "r-in-2", 1, 200, 100, 0, 270_000, 0, now)
	seedCacheLog(t, db, "r-unmetered", 2, 5_000, 0, 0, 0, 0, now)
	seedCacheLog(t, db, "r-unpriced", 3, 100, 400, 0, 0, 0, now)

	stats, err := AggregateCacheStats(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCacheStats: %v", err)
	}
	tot := stats.Totals
	if tot.CacheReadTokens != 1000 || tot.CacheWriteTokens != 50 || tot.UncachedInputTokens != 300 {
		t.Errorf("totals = read %d / write %d / uncached %d, want 1000 / 50 / 300 (provider 1 only)",
			tot.CacheReadTokens, tot.CacheWriteTokens, tot.UncachedInputTokens)
	}
	if tot.CacheReadSavedMicros != 2_700_000 || tot.CacheWriteExtraMicros != 37_500 {
		t.Errorf("savings = read %d / write %d, want 2_700_000 / 37_500",
			tot.CacheReadSavedMicros, tot.CacheWriteExtraMicros)
	}

	reasons := map[uint]string{}
	for _, u := range stats.UnsupportedProviders {
		reasons[u.ProviderID] = u.Reason
	}
	if reasons[2] != CacheUnsupportedNoMetering {
		t.Errorf("provider 2 reason = %q, want %q", reasons[2], CacheUnsupportedNoMetering)
	}
	if reasons[3] != CacheUnsupportedNoCachePrice {
		t.Errorf("provider 3 reason = %q, want %q", reasons[3], CacheUnsupportedNoCachePrice)
	}
	if _, ok := reasons[1]; ok {
		t.Error("provider 1 is cache-capable and must not be listed as unsupported")
	}
}

// TestAggregateCacheStatsNegativeNetSurvives: a window where the write
// premium exceeds the read saving must come back with both components intact
// so the client renders a negative net — never clamped to zero.
func TestAggregateCacheStatsNegativeNetSurvives(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	seedCacheProvider(t, db, 1, true)
	seedCacheLog(t, db, "r-neg", 1, 1000, 10, 2000, 27_000, 1_500_000, now)

	stats, err := AggregateCacheStats(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCacheStats: %v", err)
	}
	tot := stats.Totals
	if tot.CacheReadSavedMicros != 27_000 || tot.CacheWriteExtraMicros != 1_500_000 {
		t.Errorf("savings = read %d / write %d, want 27_000 / 1_500_000",
			tot.CacheReadSavedMicros, tot.CacheWriteExtraMicros)
	}
	if net := tot.CacheReadSavedMicros - tot.CacheWriteExtraMicros; net >= 0 {
		t.Errorf("net = %d, want negative (write premium exceeded read saving)", net)
	}
}

// TestAggregateCacheStatsEmptyWindow: no rows at all — zero totals, no
// unsupported providers (a provider with no traffic in the window has nothing
// to disclaim), and no error.
func TestAggregateCacheStatsEmptyWindow(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedCacheProvider(t, db, 1, true)

	stats, err := AggregateCacheStats(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCacheStats: %v", err)
	}
	if stats.Totals != (CacheTotals{}) {
		t.Errorf("totals = %+v, want all zero", stats.Totals)
	}
	if len(stats.UnsupportedProviders) != 0 {
		t.Errorf("unsupported = %+v, want empty (no traffic, nothing to disclaim)", stats.UnsupportedProviders)
	}
}

// TestAggregateCacheStatsResolvesProviderNames: the unsupported list carries
// the provider display name so the page never shows a bare id.
func TestAggregateCacheStatsResolvesProviderNames(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	seedCacheProvider(t, db, 7, true)
	seedCacheLog(t, db, "r-x", 7, 100, 0, 0, 0, 0, now)

	stats, err := AggregateCacheStats(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCacheStats: %v", err)
	}
	if len(stats.UnsupportedProviders) != 1 {
		t.Fatalf("unsupported = %+v, want exactly provider 7 (no metering in window)", stats.UnsupportedProviders)
	}
	if got := stats.UnsupportedProviders[0].ProviderName; got != "provider-7" {
		t.Errorf("provider name = %q, want provider-7", got)
	}
}

// seedCacheLogOwned is seedCacheLog with ownership and model routing set, so
// the dimension report tests have keys to group by.
func seedCacheLogOwned(t *testing.T, db *gorm.DB, requestID string, providerID uint,
	userID, apiKeyID uint, modelName string,
	uncachedInput, cacheRead, cacheWrite int, readSaved, writeExtra int64, createdAt time.Time,
) {
	t.Helper()
	pid, uid, kid := providerID, userID, apiKeyID
	row := &model.RequestLog{
		RequestID:             requestID,
		ModelName:             modelName,
		ProviderID:            &pid,
		UserID:                &uid,
		APIKeyID:              &kid,
		StatusCode:            200,
		InputTokens:           uncachedInput,
		OutputTokens:          10,
		CacheReadTokens:       cacheRead,
		CacheWriteTokens:      cacheWrite,
		CacheReadSavedMicros:  readSaved,
		CacheWriteExtraMicros: writeExtra,
		CostMicros:            100,
		CostKnown:             true,
		CreatedAt:             createdAt,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("create log %s: %v", requestID, err)
	}
}

// TestAggregateCacheStatsQuietWindowKeepsCapableProvider: metering evidence
// is judged over the provider's whole history, not the filtered window. A
// provider that has metered cache before must stay in the totals through a
// window with zero cache activity — excluding it would inflate the platform
// hit rate by dropping its uncached input from the denominator, and a true
// 0%-hit window could never display as one.
func TestAggregateCacheStatsQuietWindowKeepsCapableProvider(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	seedCacheProvider(t, db, 1, true)
	// Historical evidence OUTSIDE the queried window: the provider meters.
	seedCacheLog(t, db, "r-old", 1, 100, 900, 50, 2_430_000, 37_500, now.AddDate(0, 0, -30))
	// Inside the window: traffic, but not a single cache token.
	seedCacheLog(t, db, "r-quiet-1", 1, 4_000, 0, 0, 0, 0, now)
	seedCacheLog(t, db, "r-quiet-2", 1, 6_000, 0, 0, 0, 0, now)

	start := now.Add(-time.Hour)
	end := now.Add(time.Hour)
	stats, err := AggregateCacheStats(context.Background(), db, &RequestLogFilter{StartTime: &start, EndTime: &end})
	if err != nil {
		t.Fatalf("AggregateCacheStats: %v", err)
	}
	if len(stats.UnsupportedProviders) != 0 {
		t.Errorf("unsupported = %+v, want empty (provider has metered before)", stats.UnsupportedProviders)
	}
	tot := stats.Totals
	if tot.UncachedInputTokens != 10_000 || tot.CacheReadTokens != 0 {
		t.Errorf("totals = uncached %d / read %d, want 10_000 / 0 (quiet window still counts)",
			tot.UncachedInputTokens, tot.CacheReadTokens)
	}
}

// TestAggregateCacheStatsHistoricalSavingsKeepPriceEvidence: clearing a
// provider's cache price today must not silently rewrite history — a
// provider whose rows carry settled cache savings has proven the price list
// once distinguished cache from input, and past windows keep their totals.
func TestAggregateCacheStatsHistoricalSavingsKeepPriceEvidence(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	// Candidate WITHOUT a configured cache price (the admin cleared it) —
	// but the rows below carry settled savings from when it was priced.
	seedCacheProvider(t, db, 1, false)
	seedCacheLog(t, db, "r-hist", 1, 100, 900, 50, 2_430_000, 37_500, now)

	stats, err := AggregateCacheStats(context.Background(), db, &RequestLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCacheStats: %v", err)
	}
	if len(stats.UnsupportedProviders) != 0 {
		t.Errorf("unsupported = %+v, want empty (settled savings prove the price capability)", stats.UnsupportedProviders)
	}
	if stats.Totals.CacheReadSavedMicros != 2_430_000 {
		t.Errorf("read saved = %d, want 2_430_000 (historical rows stay in the totals)", stats.Totals.CacheReadSavedMicros)
	}
}
