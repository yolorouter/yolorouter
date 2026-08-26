package repository

import (
	"context"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/testutil"
)

// The dimension reports carry the settled cache-savings sums so the analytics
// tables can show a per-row hit rate and net saving without a second call.
// Unlike the platform cache totals, no provider restriction applies: these
// are per-row sums, and a row with no cache tokens simply renders as "no
// data" client-side.
func TestDimensionReportsCarryCacheSavings(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	seedCacheProvider(t, db, 1, true)
	keyA, userA := testutil.SeedAPIKey(t, db, "owner-a")

	seedCacheLogOwned(t, db, "s1", 1, userA, keyA, "m-a", 100, 900, 50, 2_430_000, 37_500, now)
	seedCacheLogOwned(t, db, "s2", 1, userA, keyA, "m-a", 200, 100, 0, 270_000, 0, now)

	f := &RequestLogFilter{}
	ctx := context.Background()

	users, err := AggregateByUser(ctx, db, f)
	if err != nil {
		t.Fatalf("AggregateByUser: %v", err)
	}
	if len(users) != 1 || users[0].CacheReadSavedMicros != 2_700_000 || users[0].CacheWriteExtraMicros != 37_500 {
		t.Errorf("user row savings = %+v, want read 2_700_000 / write 37_500", users)
	}

	models, err := AggregateByModel(ctx, db, f)
	if err != nil {
		t.Fatalf("AggregateByModel: %v", err)
	}
	if len(models) != 1 || models[0].CacheReadSavedMicros != 2_700_000 || models[0].CacheWriteExtraMicros != 37_500 {
		t.Errorf("model row savings = %+v, want read 2_700_000 / write 37_500", models)
	}

	callers, err := AggregateByCaller(ctx, db, f)
	if err != nil {
		t.Fatalf("AggregateByCaller: %v", err)
	}
	if len(callers) != 1 || callers[0].CacheReadSavedMicros != 2_700_000 || callers[0].CacheWriteExtraMicros != 37_500 {
		t.Errorf("caller row savings = %+v, want read 2_700_000 / write 37_500", callers)
	}

	times, err := AggregateByTime(ctx, db, f, time.UTC, "day", now)
	if err != nil {
		t.Fatalf("AggregateByTime: %v", err)
	}
	var readSum, writeSum int64
	for _, r := range times {
		readSum += r.CacheReadSavedMicros
		writeSum += r.CacheWriteExtraMicros
	}
	if readSum != 2_700_000 || writeSum != 37_500 {
		t.Errorf("time rows savings sum = read %d / write %d, want 2_700_000 / 37_500", readSum, writeSum)
	}

	providers, err := AggregateByProvider(ctx, db, f)
	if err != nil {
		t.Fatalf("AggregateByProvider: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("provider rows = %+v, want 1", providers)
	}
	p := providers[0]
	// The provider report needs the token sums too — its row has no
	// ReportTokenCost block, and the hit rate cannot be derived without them.
	if p.InputTokens != 300 || p.CacheReadTokens != 1000 || p.CacheWriteTokens != 50 {
		t.Errorf("provider row tokens = input %d / read %d / write %d, want 300 / 1000 / 50",
			p.InputTokens, p.CacheReadTokens, p.CacheWriteTokens)
	}
	if p.CacheReadSavedMicros != 2_700_000 || p.CacheWriteExtraMicros != 37_500 {
		t.Errorf("provider row savings = read %d / write %d, want 2_700_000 / 37_500",
			p.CacheReadSavedMicros, p.CacheWriteExtraMicros)
	}
}
