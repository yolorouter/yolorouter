package repository

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// auditRetentionFixture is one audit sweep test's world: a fully migrated
// temp SQLite database and a pinned now, so every seed offset is measured
// against the exact instant the sweep will be called with.
type auditRetentionFixture struct {
	db  *gorm.DB
	now time.Time
}

func newAuditRetentionFixture(t *testing.T) *auditRetentionFixture {
	t.Helper()
	return &auditRetentionFixture{
		db:  testutil.NewSQLiteDB(t),
		now: time.Now().UTC().Truncate(time.Second),
	}
}

// seedAudit inserts one audit row with a distinct action so tests can tell
// the rows apart without relying on ids.
func (f *auditRetentionFixture) seedAudit(t *testing.T, action string, createdAt time.Time) {
	t.Helper()
	if err := CreateAdminAuditLogs(f.db, []model.AdminAuditLog{{
		CreatedAt: createdAt, AdminUsername: "boss", Action: action,
		ResourceType: "provider", Status: model.AuditStatusSuccess,
	}}); err != nil {
		t.Fatalf("seed audit row %s: %v", action, err)
	}
}

// auditActionsLeft returns the set of actions still present, so a test can
// say exactly WHICH rows left rather than only counting.
func (f *auditRetentionFixture) auditActionsLeft(t *testing.T) map[string]bool {
	t.Helper()
	var actions []string
	if err := f.db.Model(&model.AdminAuditLog{}).Distinct().Pluck("action", &actions).Error; err != nil {
		t.Fatalf("pluck remaining actions: %v", err)
	}
	left := make(map[string]bool, len(actions))
	for _, a := range actions {
		left[a] = true
	}
	return left
}

// TestAdminAuditExpiredBoundaryTable pins the pure predicate: the domain
// edge cases that would otherwise only surface through the sweep's SQL.
func TestAdminAuditExpiredBoundaryTable(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		days      int
		createdAt time.Time
		want      bool
	}{
		{
			name:      "retention zero never deletes even an ancient row",
			days:      0,
			createdAt: now.AddDate(-10, 0, 0),
			want:      false,
		},
		{
			name:      "negative retention never deletes (corrupt value fails toward keeping evidence)",
			days:      -3,
			createdAt: now.AddDate(-10, 0, 0),
			want:      false,
		},
		{
			name:      "row exactly on the boundary is kept (keeping N days includes day N)",
			days:      7,
			createdAt: now.Add(-7 * 24 * time.Hour),
			want:      false,
		},
		{
			name:      "row one second older than the boundary is deleted",
			days:      7,
			createdAt: now.Add(-7*24*time.Hour - time.Second),
			want:      true,
		},
		{
			name:      "row one day newer than the boundary is kept",
			days:      7,
			createdAt: now.Add(-6 * 24 * time.Hour),
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AdminAuditExpired(tc.createdAt, now, tc.days); got != tc.want {
				t.Fatalf("AdminAuditExpired(createdAt=%s, now=%s, days=%d) = %v, want %v",
					tc.createdAt.Format(time.RFC3339), now.Format(time.RFC3339), tc.days, got, tc.want)
			}
		})
	}
}

// TestSweepExpiredAdminAuditLogsOldLeaveNewStay pins the sweep engine:
// under a positive retention the expired row leaves and the recent row
// stays, and the boundary row (created exactly retention-days ago) counts
// as recent — keeping N days includes day N itself.
func TestSweepExpiredAdminAuditLogsOldLeaveNewStay(t *testing.T) {
	f := newAuditRetentionFixture(t)
	const days = 7

	f.seedAudit(t, "expired.old", f.now.Add(-8*24*time.Hour))
	f.seedAudit(t, "boundary.exact", f.now.Add(-7*24*time.Hour))
	f.seedAudit(t, "recent.new", f.now.Add(-24*time.Hour))
	f.seedAudit(t, "recent.now", f.now)

	deleted, err := SweepExpiredAdminAuditLogs(f.db, f.now, days, 0)
	if err != nil {
		t.Fatalf("sweep round: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("round deleted %d rows, want 1 (only the strictly-expired row)", deleted)
	}

	left := f.auditActionsLeft(t)
	if left["expired.old"] {
		t.Errorf("the expired row survived the sweep")
	}
	for _, keep := range []string{"boundary.exact", "recent.new", "recent.now"} {
		if !left[keep] {
			t.Errorf("row %s was deleted, want kept", keep)
		}
	}
}

// TestSweepExpiredAdminAuditLogsZeroDaysRemovesNothing pins keep-forever
// at engine level: retention 0 removes nothing, no matter how ancient the
// row.
func TestSweepExpiredAdminAuditLogsZeroDaysRemovesNothing(t *testing.T) {
	f := newAuditRetentionFixture(t)

	f.seedAudit(t, "ancient.but.kept", f.now.Add(-3650*24*time.Hour))

	deleted, err := SweepExpiredAdminAuditLogs(f.db, f.now, 0, 0)
	if err != nil {
		t.Fatalf("sweep round: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("zero-retention round deleted %d rows, want 0", deleted)
	}
	if left := f.auditActionsLeft(t); !left["ancient.but.kept"] {
		t.Fatalf("the ancient row was deleted under retention 0 — keep forever must hold")
	}
}

// TestSweepExpiredAdminAuditLogsBoundedRoundsConverge pins the gradual
// convergence: the cap binds each round, rounds repeat until drained, and
// the oldest rows leave first so a backlog drains in arrival order.
func TestSweepExpiredAdminAuditLogsBoundedRoundsConverge(t *testing.T) {
	f := newAuditRetentionFixture(t)
	const days = 1
	const limit = 3
	const extra = 2
	total := limit + extra // the backlog: bound + K expired rows

	for i := 0; i < total; i++ {
		// Distinct ages seed a deterministic oldest-first order; the first
		// round must remove exactly the `limit` oldest.
		f.seedAudit(t, fmt.Sprintf("backlog.%02d", i), f.now.Add(-time.Duration(30+i)*24*time.Hour))
	}

	remaining := func() int64 {
		var n int64
		if err := f.db.Model(&model.AdminAuditLog{}).Count(&n).Error; err != nil {
			t.Fatalf("count admin_audit_logs: %v", err)
		}
		return n
	}

	// Round one: the cap bites, and it takes the OLDEST rows. Age runs
	// opposite to the index (row i sits 30+i days back), so round one —
	// limit 3 of 5 — must take backlog.04/.03/.02 and leave .01/.00.
	deleted, err := SweepExpiredAdminAuditLogs(f.db, f.now, days, limit)
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if deleted != limit {
		t.Fatalf("first round deleted %d rows, want %d (the batch limit)", deleted, limit)
	}
	left := f.auditActionsLeft(t)
	for i := total - limit; i < total; i++ {
		if left[fmt.Sprintf("backlog.%02d", i)] {
			t.Errorf("round one must take the oldest rows first; backlog.%02d survived", i)
		}
	}
	if got := remaining(); got != extra {
		t.Fatalf("after first round %d rows remain, want %d", got, extra)
	}

	// Round two drains the rest (fewer than the limit this time).
	deleted, err = SweepExpiredAdminAuditLogs(f.db, f.now, days, limit)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if deleted != extra {
		t.Fatalf("second round deleted %d rows, want %d", deleted, extra)
	}
	if got := remaining(); got != 0 {
		t.Fatalf("after second round %d rows remain, want 0", got)
	}

	// Round three: nothing left to do, and repeating is harmless.
	deleted, err = SweepExpiredAdminAuditLogs(f.db, f.now, days, limit)
	if err != nil {
		t.Fatalf("third round: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("converged round deleted %d rows, want 0", deleted)
	}
}

// TestSweepExpiredAdminAuditLogsZeroLimitFallsBackToNamedDefault pins the
// zero/negative batch limit fallback: the round removes exactly the named
// default cap, and the next round finishes the backlog.
func TestSweepExpiredAdminAuditLogsZeroLimitFallsBackToNamedDefault(t *testing.T) {
	f := newAuditRetentionFixture(t)
	const days = 1
	total := DefaultAdminAuditRetentionBatchLimit + 1

	seedErr := f.db.Transaction(func(tx *gorm.DB) error {
		rows := make([]model.AdminAuditLog, 0, total)
		for i := 0; i < total; i++ {
			rows = append(rows, model.AdminAuditLog{
				CreatedAt: f.now.Add(-90 * 24 * time.Hour),
				Action:    fmt.Sprintf("default.%04d", i), ResourceType: "provider",
				Status: model.AuditStatusSuccess,
			})
		}
		return CreateAdminAuditLogs(tx, rows)
	})
	if seedErr != nil {
		t.Fatalf("seed backlog: %v", seedErr)
	}

	deleted, err := SweepExpiredAdminAuditLogs(f.db, f.now, days, 0)
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if deleted != DefaultAdminAuditRetentionBatchLimit {
		t.Fatalf("round with injected limit 0 deleted %d rows, want the named default %d",
			deleted, DefaultAdminAuditRetentionBatchLimit)
	}

	deleted, err = SweepExpiredAdminAuditLogs(f.db, f.now, days, 0)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("second round deleted %d rows, want the 1 leftover", deleted)
	}
}
