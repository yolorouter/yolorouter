package repository

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedOwnedRequestLog inserts one request_logs row owned by userID (nil =
// an unauthenticated audit row).
func seedOwnedRequestLog(t *testing.T, db *gorm.DB, requestID string, userID *uint) {
	t.Helper()
	row := &model.RequestLog{
		RequestID:  requestID,
		UserID:     userID,
		ModelName:  "m",
		StatusCode: 200,
		CostMicros: 1000,
		CostKnown:  true,
		CreatedAt:  time.Now().UTC(),
	}
	if err := CreateRequestLog(db, row); err != nil {
		t.Fatalf("seed request log %s: %v", requestID, err)
	}
}

// TestRequestLogFilterByUserIsolatesOwners is the data-isolation floor for
// every per-user statistics view: a user filter must return exactly that
// user's rows — never another user's, never the ownerless audit rows.
// Remove the user_id clause from applyFilter and this goes red.
func TestRequestLogFilterByUserIsolatesOwners(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	alice := seedUser(t, db, "alice-owner")
	bob := newMemberUser("bob-owner", time.Now().UTC())
	if err := CreateUser(db, bob); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	seedOwnedRequestLog(t, db, "req-alice-1", &alice.ID)
	seedOwnedRequestLog(t, db, "req-alice-2", &alice.ID)
	seedOwnedRequestLog(t, db, "req-bob-1", &bob.ID)
	seedOwnedRequestLog(t, db, "req-anon", nil)

	rows, total, err := ListRequestLogs(db, &RequestLogFilter{UserID: &alice.ID, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("expected exactly alice's 2 rows, got total=%d len=%d", total, len(rows))
	}
	for _, r := range rows {
		if r.UserID == nil || *r.UserID != alice.ID {
			t.Fatalf("row %s leaked into alice's view (user_id=%v)", r.RequestID, r.UserID)
		}
	}

	// The aggregate path shares applyFilter — its totals must isolate too.
	m, err := AggregateRequestLogMetrics(db, &RequestLogFilter{UserID: &bob.ID})
	if err != nil {
		t.Fatalf("AggregateRequestLogMetrics: %v", err)
	}
	if m.TotalCalls != 1 {
		t.Fatalf("expected bob's aggregate to count 1 call, got %d", m.TotalCalls)
	}

	// No filter = everything, including the ownerless audit row.
	_, allTotal, err := ListRequestLogs(db, &RequestLogFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListRequestLogs (unfiltered): %v", err)
	}
	if allTotal != 4 {
		t.Fatalf("expected the unfiltered view to keep all 4 rows, got %d", allTotal)
	}
}

// TestDashboardQueriesFilterByUser pins the per-account scope on every
// traffic-backed dashboard section: metrics, trend, top callers, and
// recent failures must each honor the user filter. Remove any one
// function's user_id clause and its assertion goes red.
func TestDashboardQueriesFilterByUser(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	alice := seedUser(t, db, "alice-dash")
	bob := newMemberUser("bob-dash", now)
	if err := CreateUser(db, bob); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	keyID := uint(41)
	aliceKey := &model.APIKey{ID: keyID, KeyHash: "kh-dash-a", KeyPrefix: "sk-yr-da", UserID: alice.ID,
		Status: model.APIKeyStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := CreateAPIKey(db, aliceKey, nil, now); err != nil {
		t.Fatalf("seed alice key: %v", err)
	}

	failReason := "boom"
	rows := []model.RequestLog{
		{RequestID: "d-a1", APIKeyID: &keyID, UserID: &alice.ID, ModelName: "m", StatusCode: 200, CostMicros: 100, CostKnown: true, CreatedAt: now},
		{RequestID: "d-a2", APIKeyID: &keyID, UserID: &alice.ID, ModelName: "m", StatusCode: 500, FailReason: &failReason, CreatedAt: now},
		{RequestID: "d-b1", UserID: &bob.ID, ModelName: "m", StatusCode: 500, FailReason: &failReason, CostMicros: 900, CreatedAt: now},
	}
	for i := range rows {
		if err := CreateRequestLog(db, &rows[i]); err != nil {
			t.Fatalf("seed log %s: %v", rows[i].RequestID, err)
		}
	}

	start, end := now.Add(-time.Hour), now.Add(time.Hour)

	m, err := GetRangeMetrics(db, start, end, &alice.ID)
	if err != nil {
		t.Fatalf("GetRangeMetrics: %v", err)
	}
	if m.Calls != 2 {
		t.Fatalf("metrics: expected alice's 2 calls, got %d", m.Calls)
	}

	trend, err := GetTrendRange(db, start, end, time.UTC, &alice.ID)
	if err != nil {
		t.Fatalf("GetTrendRange: %v", err)
	}
	var trendCalls int64
	for _, p := range trend {
		trendCalls += p.Calls
	}
	if trendCalls != 2 {
		t.Fatalf("trend: expected alice's 2 calls, got %d", trendCalls)
	}

	top, err := GetTopCallers(db, start, end, 5, &bob.ID)
	if err != nil {
		t.Fatalf("GetTopCallers: %v", err)
	}
	if len(top) != 0 {
		t.Fatalf("top callers: bob has no keyed rows, got %d entries", len(top))
	}

	failures, err := GetRecentFailures(db, 5, &alice.ID)
	if err != nil {
		t.Fatalf("GetRecentFailures: %v", err)
	}
	if len(failures) != 1 || failures[0].RequestID != "d-a2" {
		t.Fatalf("failures: expected only alice's d-a2, got %+v", failures)
	}
}

// TestAPIKeyFilterByUserIsolatesOwners pins the key-list half of the same
// floor: filtering by owner returns only that owner's keys.
func TestAPIKeyFilterByUserIsolatesOwners(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	alice := seedUser(t, db, "alice-keys")
	bob := newMemberUser("bob-keys", now)
	if err := CreateUser(db, bob); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	for i, spec := range []struct {
		hash  string
		owner uint
	}{
		{"kh-alice-1", alice.ID},
		{"kh-alice-2", alice.ID},
		{"kh-bob-1", bob.ID},
	} {
		key := &model.APIKey{
			KeyHash:   spec.hash,
			KeyPrefix: "sk-yr-iso",
			UserID:    spec.owner,
			Status:    model.APIKeyStatusActive,
			CreatedAt: now.Add(time.Duration(i) * time.Second),
			UpdatedAt: now,
		}
		if err := CreateAPIKey(db, key, nil, now); err != nil {
			t.Fatalf("seed key %s: %v", spec.hash, err)
		}
	}

	keys, err := SearchAPIKeys(db, APIKeyFilter{UserID: &alice.ID, Now: now}, 0, 10)
	if err != nil {
		t.Fatalf("SearchAPIKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected alice's 2 keys, got %d", len(keys))
	}
	for _, k := range keys {
		if k.UserID != alice.ID {
			t.Fatalf("key %s leaked into alice's view (user_id=%d)", k.KeyPrefix, k.UserID)
		}
	}
	total, err := CountAPIKeys(db, APIKeyFilter{UserID: &bob.ID, Now: now})
	if err != nil {
		t.Fatalf("CountAPIKeys: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected bob to count 1 key, got %d", total)
	}
}
