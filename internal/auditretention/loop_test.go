// Black-box tests for the admin-audit retention loop: an external test
// package so the loop is driven only through its exported surface (NewLoop
// + Start), against a real migrated SQLite database, the real sweep
// engine, and the real settings service — the same fixture shape the
// request-log retention loop tests use. The settings service is ONE
// instance shared by the test's update path and the loop's read path: a
// mid-run settings change goes through the real update path (domain
// validation, CAS, immediate cache publish), and the loop's next read
// picks it up — which is exactly the per-round re-read contract under
// test.
package auditretention_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/auditretention"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// ancient is far outside every retention window used in these scenarios,
// so a seeded row counts as expired under any positive setting.
var ancient = time.Now().UTC().Add(-365 * 24 * time.Hour)

// fixture is one loop test's world: a fully migrated temp SQLite database
// and the shared settings service instance (see the package comment for
// why sharing it is what makes mid-run changes observable at test speed).
type fixture struct {
	db       *gorm.DB
	settings *systemsettings.SystemSettingsService
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	return &fixture{
		db:       db,
		settings: systemsettings.NewSystemSettingsService(db),
	}
}

// seedAudit inserts one audit row with a distinct action so tests can tell
// the rows apart without relying on ids.
func (f *fixture) seedAudit(t *testing.T, action string, createdAt time.Time) {
	t.Helper()
	if err := repository.CreateAdminAuditLogs(f.db, []model.AdminAuditLog{{
		CreatedAt: createdAt, AdminUsername: "boss", Action: action,
		ResourceType: "provider", Status: model.AuditStatusSuccess,
	}}); err != nil {
		t.Fatalf("seed audit row %s: %v", action, err)
	}
}

// auditActionsLeft returns the set of actions still present.
func (f *fixture) auditActionsLeft(t *testing.T) map[string]bool {
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

// countAuditRows returns how many admin_audit_logs rows remain in total.
func (f *fixture) countAuditRows(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.db.Model(&model.AdminAuditLog{}).Count(&n).Error; err != nil {
		t.Fatalf("count admin_audit_logs: %v", err)
	}
	return n
}

// setRetention flips the audit retention setting mid-run through the REAL
// update path on the shared service instance: domain validation, the CAS
// against the current version, and the immediate cache publish the loop's
// next read picks up.
func (f *fixture) setRetention(t *testing.T, days int) {
	t.Helper()
	_, ver, err := f.settings.GetAdminAuditRetentionForHandler(context.Background())
	if err != nil {
		t.Fatalf("read audit retention version: %v", err)
	}
	if _, _, err := f.settings.UpdateAdminAuditRetention(context.Background(), ver, days); err != nil {
		t.Fatalf("update audit retention to %d days: %v", days, err)
	}
}

// startLoop starts a loop on the fixture with a millisecond-level beat and
// returns its stop function.
func startLoop(f *fixture) (stop func()) {
	loop := auditretention.NewLoop(auditretention.Config{
		DB:        f.db,
		Settings:  f.settings,
		Heartbeat: 2 * time.Millisecond,
	})
	return loop.Start(context.Background())
}

func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestLoopDeletesExpiredAndKeepsRecent pins the live loop: with a
// positive retention configured, the live loop removes the expired row and
// leaves the recent one — old leaves, new stays, driven end to end through
// the real settings service and the real sweep engine.
func TestLoopDeletesExpiredAndKeepsRecent(t *testing.T) {
	f := newFixture(t)
	f.setRetention(t, 30)

	f.seedAudit(t, "expired.old", ancient)
	f.seedAudit(t, "recent.fresh", time.Now().UTC())

	stop := startLoop(f)
	defer stop()

	waitFor(t, 3*time.Second, "the expired audit row to be cleared", func() bool {
		left := f.auditActionsLeft(t)
		return !left["expired.old"]
	})

	left := f.auditActionsLeft(t)
	if !left["recent.fresh"] {
		t.Fatalf("the recent row was deleted — retention must only take expired rows")
	}
	if got := f.countAuditRows(t); got != 1 {
		t.Fatalf("%d audit rows remain, want exactly the 1 recent row", got)
	}
}

// TestLoopReReadsTheRetentionSettingEveryRound pins setting re-reads: the
// retention value is re-read on every round, never captured at start —
// through the 0→N→0 toggle, against the real settings service and the real
// sweep engine:
//
//   - retention 0: an expired row sits through many beats untouched
//     (keep forever, and no value was baked in at construction);
//   - mid-run change to N=7: the very next round clears the expired row —
//     a loop caching the startup value would delete nothing, ever;
//   - mid-run change back to 0: a freshly seeded expired row again
//     survives many beats — deletion stops the moment the setting does.
func TestLoopReReadsTheRetentionSettingEveryRound(t *testing.T) {
	f := newFixture(t)

	// Phase 0: keep forever, written explicitly so the scenario does not
	// depend on the seed migration for its starting state.
	f.setRetention(t, 0)

	expired := "audit-ret-loop-toggle"
	f.seedAudit(t, expired, ancient)

	stop := startLoop(f)
	defer stop()

	// Phase 1: retention 0 — roughly a hundred beats pass, nothing leaves.
	time.Sleep(200 * time.Millisecond)
	if left := f.auditActionsLeft(t); !left[expired] {
		t.Fatalf("retention 0: the expired row was touched — keep-forever must hold on every beat")
	}

	// Phase 2: flip to N=7 mid-run — the next round clears the row.
	f.setRetention(t, 7)
	waitFor(t, 3*time.Second, "the expired row to be cleared after enabling retention", func() bool {
		left := f.auditActionsLeft(t)
		return !left[expired]
	})

	// Phase 3: flip back to 0 mid-run. The settle sleep first lets any beat
	// that already read 7 finish its round; once settled, every subsequent
	// beat reads 0, so a freshly seeded expired row must survive however
	// many beats follow.
	f.setRetention(t, 0)
	time.Sleep(50 * time.Millisecond)
	expiredAgain := "audit-ret-loop-toggle-back"
	f.seedAudit(t, expiredAgain, ancient)
	time.Sleep(200 * time.Millisecond)
	if left := f.auditActionsLeft(t); !left[expiredAgain] {
		t.Fatalf("retention back to 0: the newly expired row was deleted — deletion must stop the moment the setting returns to keep-forever")
	}
}

// TestLoopStopsSweepingAfterStop pins the stop half of the lifecycle: once
// the stop function has returned (the goroutine has exited — stop waits
// for it), no further round runs, even with retention active and an
// expired row sitting in plain sight.
func TestLoopStopsSweepingAfterStop(t *testing.T) {
	f := newFixture(t)
	f.setRetention(t, 30)

	first := "audit-ret-loop-stop-1"
	f.seedAudit(t, first, ancient)

	stop := startLoop(f)
	waitFor(t, 3*time.Second, "the first row to be cleared while the loop runs", func() bool {
		left := f.auditActionsLeft(t)
		return !left[first]
	})
	stop()

	second := "audit-ret-loop-stop-2"
	f.seedAudit(t, second, ancient)
	time.Sleep(200 * time.Millisecond) // roughly a hundred would-have-fired beats
	if left := f.auditActionsLeft(t); !left[second] {
		t.Fatalf("a round ran after the loop was stopped — stop must end the schedule")
	}
	// Calling the stop function again is documented as safe.
	stop()
}

// TestLoopDrainsABacklogOverSuccessiveBoundedRounds pins the gradual
// convergence: with the batch limit injected below the backlog size, the
// live loop clears the backlog over several rounds rather than in one
// unbounded purge. The per-round bound itself is the sweep engine's
// contract (covered by its own tests); what this pins at loop level is
// that rounds keep repeating until the backlog is gone.
func TestLoopDrainsABacklogOverSuccessiveBoundedRounds(t *testing.T) {
	f := newFixture(t)
	f.setRetention(t, 30)

	for i := 0; i < 3; i++ {
		f.seedAudit(t, "audit-ret-loop-drain", ancient)
	}

	loop := auditretention.NewLoop(auditretention.Config{
		DB:         f.db,
		Settings:   f.settings,
		Heartbeat:  2 * time.Millisecond,
		BatchLimit: 2, // below the backlog of 3: at least two rounds are required
	})
	stop := loop.Start(context.Background())
	defer stop()

	waitFor(t, 3*time.Second, "the backlog to drain through repeated rounds", func() bool {
		return f.countAuditRows(t) == 0
	})
}
