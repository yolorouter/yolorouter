package repository

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

func int64Ptr(v int64) *int64 { return &v }

func TestUpsertObservedRateLimitLearnsAndOnlyMovesDown(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "openai-main")
	at := time.Now().UTC().Truncate(time.Second)

	// First observation of a number sets it.
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterRequests, int64Ptr(100), nil, int64Ptr(7), nil, at); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	// A larger number must not raise the remembered ceiling.
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterRequests, int64Ptr(200), nil, nil, nil, at); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	// A smaller one does.
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterRequests, int64Ptr(60), nil, int64Ptr(0), nil, at); err != nil {
		t.Fatalf("upsert 3: %v", err)
	}
	// An absent number (NULL evidence) keeps the learned one.
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterRequests, nil, nil, int64Ptr(3), nil, at); err != nil {
		t.Fatalf("upsert 4: %v", err)
	}

	var row model.ObservedRateLimit
	if err := db.Where("provider_key_id = ? AND meter = ?", key.ID, model.MeterRequests).First(&row).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if row.LimitValue == nil || *row.LimitValue != 60 {
		t.Fatalf("limit = %v, want 60 (100 → 200 refused → 60 accepted → NULL kept)", row.LimitValue)
	}
	if row.LastRemaining == nil || *row.LastRemaining != 3 {
		t.Fatalf("remaining = %v, want 3 (freshest gauge wins)", row.LastRemaining)
	}
}

func TestUpsertObservedRateLimitSeparateMetersAndWindows(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "openai-main")
	at := time.Now().UTC().Truncate(time.Second)

	window := int64(86400)
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterRequests, int64Ptr(60), &window, nil, nil, at); err != nil {
		t.Fatalf("upsert requests: %v", err)
	}
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterTokens, int64Ptr(100000), nil, nil, nil, at); err != nil {
		t.Fatalf("upsert tokens: %v", err)
	}
	rows, err := ListObservedRateLimits(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.ProviderKeyID != key.ID || row.KeyLabel != "primary" || row.ProviderName != "openai-main" {
			t.Fatalf("joined row = %+v, want names of the seeded key", row)
		}
		switch row.Meter {
		case model.MeterRequests:
			if row.WindowSeconds == nil || *row.WindowSeconds != 86400 {
				t.Fatalf("requests window = %v, want 86400", row.WindowSeconds)
			}
		case model.MeterTokens:
			if row.WindowSeconds != nil {
				t.Fatalf("tokens window = %v, want nil", row.WindowSeconds)
			}
		}
	}
}

func TestDeleteObservedRateLimitIsIdempotent(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "openai-main")
	at := time.Now().UTC()
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterRequests, int64Ptr(60), nil, nil, nil, at); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := DeleteObservedRateLimit(db, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := DeleteObservedRateLimit(db, 1); err != nil {
		t.Fatalf("delete again (idempotent): %v", err)
	}
	if rows, err := ListObservedRateLimits(db); err != nil || len(rows) != 0 {
		t.Fatalf("after delete rows = %v err = %v, want none", rows, err)
	}
}

// Deleting a key carries its evidence away — the cascade is the delete
// path's transaction, not an FK pragma.
func TestDeleteProviderKeyCascadesObservedRateLimits(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, key := seedProviderWithKey(t, db, "openai-main")
	at := time.Now().UTC()
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterRequests, int64Ptr(60), nil, nil, nil, at); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterTokens, int64Ptr(1000), nil, nil, nil, at); err != nil {
		t.Fatalf("upsert tokens: %v", err)
	}

	removed, err := DeleteProviderKey(db, provider.ID, key.ID)
	if err != nil || !removed {
		t.Fatalf("DeleteProviderKey = %v, %v; want true, nil", removed, err)
	}
	if rows, err := ListObservedRateLimits(db); err != nil || len(rows) != 0 {
		t.Fatalf("after key delete rows = %v err = %v, want none", rows, err)
	}
}

// Deleting the whole provider carries every key's evidence away too — the
// same rule one level up, or a provider delete would strand rows no join
// can name.
func TestDeleteProviderCascadeCascadesObservedRateLimits(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, key := seedProviderWithKey(t, db, "openai-main")
	if err := UpsertObservedRateLimit(db, key.ID, model.MeterRequests, int64Ptr(60), nil, nil, nil, time.Now().UTC()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	removed, err := DeleteProviderCascade(db, provider.ID)
	if err != nil || !removed {
		t.Fatalf("DeleteProviderCascade = %v, %v; want true, nil", removed, err)
	}
	if rows, err := ListObservedRateLimits(db); err != nil || len(rows) != 0 {
		t.Fatalf("after provider delete rows = %v err = %v, want none", rows, err)
	}
}
