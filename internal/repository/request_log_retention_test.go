package repository

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway/capture"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// retentionFixture is one sweep test's world: a fully migrated temp SQLite
// database plus a temp bodies directory holding the stream files the sweep
// is expected to unlink. now is pinned per test so every seed offset is
// measured against the exact instant the sweep will be called with.
type retentionFixture struct {
	db        *gorm.DB
	bodiesDir string
	now       time.Time
}

func newRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	return &retentionFixture{
		db:        testutil.NewSQLiteDB(t),
		bodiesDir: t.TempDir(),
		now:       time.Now().UTC().Truncate(time.Second),
	}
}

// seedTriple inserts one request's whole audit record: the request_logs
// row, its request_log_bodies row, and (unless keepFileOut is set) the
// stream capture file under the bodies directory — the exact shape the
// gateway leaves behind for a streamed request.
func (f *retentionFixture) seedTriple(t *testing.T, requestID string, createdAt time.Time, keepFileOut bool) {
	t.Helper()
	testutil.SeedRequestLog(t, f.db, requestID, createdAt, nil)
	if err := UpsertRequestLogBody(f.db, &model.RequestLogBody{
		RequestID:      requestID,
		StreamBodyPath: capture.StreamFileName(requestID),
		CreatedAt:      createdAt,
	}); err != nil {
		t.Fatalf("seed body row %s: %v", requestID, err)
	}
	if !keepFileOut {
		f.writeStreamFile(t, requestID)
	}
}

func (f *retentionFixture) writeStreamFile(t *testing.T, requestID string) {
	t.Helper()
	path := filepath.Join(f.bodiesDir, capture.StreamFileName(requestID))
	if err := os.WriteFile(path, []byte("data: "+requestID+"\n\n"), 0o644); err != nil {
		t.Fatalf("write stream file %s: %v", path, err)
	}
}

// streamFileExists reports whether the request's capture file is still on
// disk under the fixture's bodies directory.
func (f *retentionFixture) streamFileExists(t *testing.T, requestID string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(f.bodiesDir, capture.StreamFileName(requestID)))
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat stream file for %s: %v", requestID, err)
	return false
}

// countRows counts the rows one table still holds for a request id.
func (f *retentionFixture) countRows(t *testing.T, table, requestID string) int64 {
	t.Helper()
	var n int64
	if err := f.db.Table(table).Where("request_id = ?", requestID).Count(&n).Error; err != nil {
		t.Fatalf("count %s for %s: %v", table, requestID, err)
	}
	return n
}

func TestRequestLogExpiredBoundaryTable(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
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
			name:      "negative retention never deletes (corrupt value fails toward keeping)",
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
			name:      "row one day older than the boundary is deleted",
			days:      7,
			createdAt: now.Add(-8 * 24 * time.Hour),
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
			if got := RequestLogExpired(tc.createdAt, now, tc.days); got != tc.want {
				t.Fatalf("RequestLogExpired(createdAt=%s, now=%s, days=%d) = %v, want %v",
					tc.createdAt.Format(time.RFC3339), now.Format(time.RFC3339), tc.days, got, tc.want)
			}
		})
	}
}

func TestSweepExpiredRequestLogsClearsWholeTripleInOneRound(t *testing.T) {
	f := newRetentionFixture(t)
	const days = 7

	// Two expired rows. The first has the whole triple on disk; the second
	// has its stream file deliberately missing — the state of a non-stream
	// request, or a crash between the capture file's creation and the body
	// row's write.
	expiredWithFile := "req-ret-old-present"
	expiredMissingFile := "req-ret-old-missing"
	f.seedTriple(t, expiredWithFile, f.now.Add(-8*24*time.Hour), false)
	f.seedTriple(t, expiredMissingFile, f.now.Add(-9*24*time.Hour), true)

	// Two fresh rows, comfortably inside the window, whole triple on disk.
	fresh1 := "req-ret-new-1"
	fresh2 := "req-ret-new-2"
	f.seedTriple(t, fresh1, f.now.Add(-24*time.Hour), false)
	f.seedTriple(t, fresh2, f.now, false)

	// Turn rows ride every request (the replay store's half): the sweep
	// must take the expired requests' turns with the rest of the record,
	// and keep the fresh ones'.
	for _, id := range []string{expiredWithFile, expiredMissingFile, fresh1, fresh2} {
		if err := f.db.Create(&model.AgentSessionTurn{
			AgentSessionID: "ret-sess", RequestID: id, RequestCreatedAt: f.now, Seq: 0,
			EventType: "assistant_reply", Content: `{"source":"none"}`,
		}).Error; err != nil {
			t.Fatalf("seed turn row %s: %v", id, err)
		}
	}

	deleted, err := SweepExpiredRequestLogs(f.db, f.bodiesDir, f.now, days, 0)
	if err != nil {
		t.Fatalf("sweep round: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("round deleted %d request_logs rows, want 2", deleted)
	}

	for _, id := range []string{expiredWithFile, expiredMissingFile} {
		if n := f.countRows(t, "request_logs", id); n != 0 {
			t.Errorf("expired row %s: request_logs still has %d rows, want 0", id, n)
		}
		if n := f.countRows(t, "request_log_bodies", id); n != 0 {
			t.Errorf("expired row %s: request_log_bodies still has %d rows, want 0", id, n)
		}
		if f.streamFileExists(t, id) {
			t.Errorf("expired row %s: stream file still on disk, want removed", id)
		}
		if n := f.countRows(t, "agent_session_turns", id); n != 0 {
			t.Errorf("expired row %s: agent_session_turns still has %d rows, want 0 (the turn half leaves with the record)", id, n)
		}
	}
	for _, id := range []string{fresh1, fresh2} {
		if n := f.countRows(t, "request_logs", id); n != 1 {
			t.Errorf("fresh row %s: request_logs has %d rows, want 1", id, n)
		}
		if n := f.countRows(t, "request_log_bodies", id); n != 1 {
			t.Errorf("fresh row %s: request_log_bodies has %d rows, want 1", id, n)
		}
		if !f.streamFileExists(t, id) {
			t.Errorf("fresh row %s: stream file missing, want kept", id)
		}
		if n := f.countRows(t, "agent_session_turns", id); n != 1 {
			t.Errorf("fresh row %s: agent_session_turns has %d rows, want 1 kept", id, n)
		}
	}
}

func TestSweepExpiredRequestLogsZeroDaysRemovesNothing(t *testing.T) {
	f := newRetentionFixture(t)

	ancient := "req-ret-zero-days"
	f.seedTriple(t, ancient, f.now.Add(-365*24*time.Hour), false)

	deleted, err := SweepExpiredRequestLogs(f.db, f.bodiesDir, f.now, 0, 0)
	if err != nil {
		t.Fatalf("sweep round: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("zero-retention round deleted %d rows, want 0", deleted)
	}
	if n := f.countRows(t, "request_logs", ancient); n != 1 {
		t.Fatalf("request_logs has %d rows for %s, want 1 (keep forever)", n, ancient)
	}
	if n := f.countRows(t, "request_log_bodies", ancient); n != 1 {
		t.Fatalf("request_log_bodies has %d rows for %s, want 1 (keep forever)", n, ancient)
	}
	if !f.streamFileExists(t, ancient) {
		t.Fatalf("stream file for %s missing, want kept (keep forever)", ancient)
	}
}

func TestSweepExpiredRequestLogsBoundedRoundsConverge(t *testing.T) {
	f := newRetentionFixture(t)
	const days = 1
	const limit = 3
	const extra = 2
	total := limit + extra // the backlog: bound + K expired rows

	for i := 0; i < total; i++ {
		id := fmt.Sprintf("req-ret-bulk-%02d", i)
		f.seedTriple(t, id, f.now.Add(-time.Duration(30+i)*24*time.Hour), i == 0)
	}

	remaining := func() int64 {
		var n int64
		if err := f.db.Table("request_logs").Count(&n).Error; err != nil {
			t.Fatalf("count request_logs: %v", err)
		}
		return n
	}

	// Round one: the cap bites — exactly `limit` rows leave, never more.
	deleted, err := SweepExpiredRequestLogs(f.db, f.bodiesDir, f.now, days, limit)
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if deleted != limit {
		t.Fatalf("first round deleted %d rows, want %d (the batch limit)", deleted, limit)
	}
	if got := remaining(); got != extra {
		t.Fatalf("after first round %d rows remain, want %d", got, extra)
	}

	// Round two drains the rest (fewer than the limit this time).
	deleted, err = SweepExpiredRequestLogs(f.db, f.bodiesDir, f.now, days, limit)
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
	deleted, err = SweepExpiredRequestLogs(f.db, f.bodiesDir, f.now, days, limit)
	if err != nil {
		t.Fatalf("third round: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("converged round deleted %d rows, want 0", deleted)
	}

	// The bodies table drained with it — the triple left together.
	var bodies int64
	if err := f.db.Table("request_log_bodies").Count(&bodies).Error; err != nil {
		t.Fatalf("count request_log_bodies: %v", err)
	}
	if bodies != 0 {
		t.Fatalf("request_log_bodies still holds %d rows, want 0", bodies)
	}
}

func TestSweepExpiredRequestLogsZeroLimitFallsBackToNamedDefault(t *testing.T) {
	f := newRetentionFixture(t)
	const days = 1
	total := DefaultRequestLogRetentionBatchLimit + 1

	// Seeded inside one transaction: a thousand standalone INSERTs each pay
	// their own fsync, which alone made this test take ten seconds.
	seedErr := f.db.Transaction(func(tx *gorm.DB) error {
		for i := 0; i < total; i++ {
			id := fmt.Sprintf("req-ret-default-%04d", i)
			if err := tx.Create(&model.RequestLog{
				RequestID: id, ModelName: "gpt-4o-mini", StatusCode: 200,
				CostKnown: true, Attempts: 1,
				CreatedAt: f.now.Add(-90 * 24 * time.Hour),
			}).Error; err != nil {
				return fmt.Errorf("seed request_logs row %s: %w", id, err)
			}
		}
		return nil
	})
	if seedErr != nil {
		t.Fatalf("seed backlog: %v", seedErr)
	}

	deleted, err := SweepExpiredRequestLogs(f.db, f.bodiesDir, f.now, days, 0)
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if deleted != DefaultRequestLogRetentionBatchLimit {
		t.Fatalf("round with injected limit 0 deleted %d rows, want the named default %d",
			deleted, DefaultRequestLogRetentionBatchLimit)
	}

	deleted, err = SweepExpiredRequestLogs(f.db, f.bodiesDir, f.now, days, 0)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("second round deleted %d rows, want the 1 leftover", deleted)
	}
}
