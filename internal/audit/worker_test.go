package audit

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// observedLogger builds a zap logger whose Warn+ output lands in an
// in-memory ring the test can assert on — the drop-counter warning line is
// an operator-facing contract, so the test reads the actual log entries
// rather than a getter that could drift from what the logger emits.
func observedLogger() (*zap.Logger, *observerSink) {
	core, logs := newObserverCore(zapcore.WarnLevel)
	return zap.New(core), &observerSink{logs: logs}
}

func sampleRecord(action string) Record {
	return Record{Action: action, Status: model.AuditStatusSuccess}
}

// TestSubmitPersistsAsynchronouslyAndNeverBlocks is the async half of the
// worker contract: a submit returns without doing database work, nothing
// is persisted before the flusher acts, and stopping the worker flushes
// everything still queued (the exit drain). The flush interval is set far
// beyond the test's lifetime so the only thing that can persist rows is
// the stop-driven drain — making "0 rows right after submit" a
// deterministic assertion about asynchrony, not a race with a timer.
func TestSubmitPersistsAsynchronouslyAndNeverBlocks(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	w := NewWorker(Config{DB: db, FlushInterval: time.Hour})
	stop := w.Start(context.Background())
	defer stop()

	start := time.Now()
	for i := 0; i < 3; i++ {
		w.Submit(sampleRecord("provider.update"))
	}
	submitElapsed := time.Since(start)

	if submitElapsed > 50*time.Millisecond {
		t.Fatalf("3 submits took %v; Submit must be a channel send, never a database round-trip", submitElapsed)
	}
	if n, err := repository.CountAdminAuditLogs(db); err != nil || n != 0 {
		t.Fatalf("immediately after submit, database must hold 0 rows (persistence is asynchronous), got %d (err=%v)", n, err)
	}

	stop()

	var rows []model.AdminAuditLog
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("list rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("after stop, the exit drain must have persisted all 3 queued records, got %d", len(rows))
	}
	// stop is idempotent: a second call must not panic or double-flush.
	stop()
	if n, _ := repository.CountAdminAuditLogs(db); n != 3 {
		t.Fatalf("second stop changed the row count: %d", n)
	}
}

// TestFullQueueDropsNewestAndWarnsWithIncrementingCount is the overload
// half of the contract: when the queue is full the NEWEST event is
// dropped, never a queued one, and each drop emits one structured warning
// line whose dropped_total counter strictly increases — the log is the
// only observability surface a self-hosted deployment has for this loss.
func TestFullQueueDropsNewestAndWarnsWithIncrementingCount(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	log, sink := observedLogger()
	w := NewWorker(Config{DB: db, Logger: log, BufferSize: 2, FlushInterval: time.Hour})
	stop := w.Start(context.Background())
	defer stop()

	w.Submit(sampleRecord("kept-1"))
	w.Submit(sampleRecord("kept-2"))
	w.Submit(sampleRecord("dropped-1"))
	w.Submit(sampleRecord("dropped-2"))
	w.Submit(sampleRecord("dropped-3"))

	if got := w.DroppedTotal(); got != 3 {
		t.Fatalf("expected 3 dropped events, counter reports %d", got)
	}

	entries := sink.warnEntries("admin audit queue full")
	if len(entries) != 3 {
		t.Fatalf("expected 3 drop-warning log lines, got %d: %v", len(entries), sink.allMessages())
	}
	last := int64(0)
	for i, e := range entries {
		count, ok := int64Field(e, "dropped_total")
		if !ok {
			t.Fatalf("warning line %d carries no dropped_total field: %v", i, e.fields)
		}
		if count <= last {
			t.Fatalf("drop counter must strictly increase across warning lines, got %d after %d", count, last)
		}
		last = count
	}

	stop()
	var rows []model.AdminAuditLog
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("list rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("queue held 2 records when the overload began; the drain must persist those 2, not the 3 dropped newest — got %d rows", len(rows))
	}
	if w.DroppedTotal() != 3 {
		t.Fatalf("drain must not reset the drop counter (it reports process-lifetime loss), got %d", w.DroppedTotal())
	}
}

// TestFlusherPersistsWhileRunning proves the periodic flush: with a short
// interval, submitted records reach the database without any stop call —
// the worker keeps up on its own while serving.
func TestFlusherPersistsWhileRunning(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	w := NewWorker(Config{DB: db, FlushInterval: 15 * time.Millisecond})
	stop := w.Start(context.Background())
	defer stop()

	for i := 0; i < 5; i++ {
		w.Submit(sampleRecord("model.update"))
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := repository.CountAdminAuditLogs(db)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n == 5 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic flush did not persist 5 records within 5s (have %d)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFlushFailureDropsBatchWithWarning pins the failure policy: a flush
// that cannot write loses its batch loudly (one warning line with the
// record count) and the worker keeps serving — audit persistence failure
// must never take the console down with it.
func TestFlushFailureDropsBatchWithWarning(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	log, sink := observedLogger()
	w := NewWorker(Config{DB: db, Logger: log, FlushInterval: time.Hour})
	stop := w.Start(context.Background())
	defer stop()

	w.Submit(sampleRecord("doomed"))

	// Close the underlying connection so the drain's insert fails.
	testutil.CloseDB(t, db)
	stop()

	entries := sink.warnEntries("admin audit flush failed")
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 flush-failure warning line, got %d: %v", len(entries), sink.allMessages())
	}
	if n, ok := int64Field(entries[0], "records"); !ok || n != 1 {
		t.Fatalf("flush-failure warning must report records=1, got field present=%v value=%v", ok, entries[0].fields)
	}
	if w.DroppedTotal() != 0 {
		t.Fatalf("a failed flush is not a queue drop; the drop counter must stay 0, got %d", w.DroppedTotal())
	}
}

// TestRowsKeepCaptureTimeNotFlushTime pins the timestamp contract: a
// record's created_at is when the event happened (capture time), not when
// the worker got around to writing it — under a backlog the two drift, and
// the audit trail must not let that drift rewrite history.
func TestRowsKeepCaptureTimeNotFlushTime(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	w := NewWorker(Config{DB: db, FlushInterval: time.Hour})
	stop := w.Start(context.Background())
	defer stop()

	capturedAt := time.Now().UTC().Add(-90 * time.Second).Truncate(time.Microsecond)
	rec := sampleRecord("provider.status")
	rec.At = capturedAt
	w.Submit(rec)

	// Let the wall clock visibly move past capturedAt before flushing.
	time.Sleep(25 * time.Millisecond)
	stop()

	var rows []model.AdminAuditLog
	if err := db.Find(&rows).Error; err != nil || len(rows) != 1 {
		t.Fatalf("expected 1 row after drain, got %d (err=%v)", len(rows), err)
	}
	if !rows[0].CreatedAt.Equal(capturedAt) {
		t.Fatalf("created_at must equal the capture time %v, got %v (flush time)", capturedAt, rows[0].CreatedAt)
	}
}

// TestNilLoggerFallsBackToGlobalLogger pins the production wiring:
// serve.go builds the worker WITHOUT an explicit Logger, and that path
// must still surface the loss-reporting warnings — the nil fallback
// forwards them to the process-global pkg/logger (the same surface the
// retention and key-recovery loops log to), never to a discard. Without
// this guard the observer-based tests above could stay green while the
// warnings existed only in tests.
func TestNilLoggerFallsBackToGlobalLogger(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var mu sync.Mutex
	var forwarded []logEntry
	restore := swapForwardToGlobal(func(ent zapcore.Entry, fields []zapcore.Field) {
		mu.Lock()
		defer mu.Unlock()
		forwarded = append(forwarded, logEntry{message: ent.Message, fields: fields})
	})
	defer restore()

	w := NewWorker(Config{DB: db, BufferSize: 1, FlushInterval: time.Hour})
	stop := w.Start(context.Background())
	defer stop()

	w.Submit(sampleRecord("kept"))
	w.Submit(sampleRecord("dropped"))

	if got := w.DroppedTotal(); got != 1 {
		t.Fatalf("expected 1 dropped event, counter reports %d", got)
	}

	mu.Lock()
	defer mu.Unlock()
	var warns []logEntry
	for _, e := range forwarded {
		if e.message == "admin audit queue full; dropping the newest event" {
			warns = append(warns, e)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("the nil-Logger fallback must forward the drop warning to the global logger, got %d: %v", len(warns), forwarded)
	}
	if n, ok := int64Field(warns[0], "dropped_total"); !ok || n != 1 {
		t.Fatalf("forwarded drop warning must carry dropped_total=1, got present=%v value=%v", ok, n)
	}
}

// TestDrainSplitsOversizedQueueIntoBatches is the batch-flush leg of the
// worker contract: when a drain faces more queued records than one batch
// (the default 64), takeBatch's loop must split them across several
// batch-sized inserts — never one unbounded statement, never one batch per
// drain round with the rest silently abandoned. The row total alone cannot
// pin that shape (an implementation that drained everything in a single
// oversized INSERT would also end with the right count), so the test
// observes each insert's size at the database boundary: a GORM create
// callback fires once per CreateAdminAuditLogs call with the destination
// slice attached, which is exactly the output of one takeBatch round. The
// flush interval is set far beyond the test's lifetime, so the only thing
// that can produce inserts is the stop-driven drain.
func TestDrainSplitsOversizedQueueIntoBatches(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	// Record the size of every batch insert this connection performs. Runs
	// only on the flusher goroutine; the assertions below all happen after
	// stop() has waited for that goroutine to exit, so no lock is needed.
	var insertSizes []int
	if err := db.Callback().Create().Before("gorm:create").Register(
		"audit_test:observe_batch_size",
		func(tx *gorm.DB) {
			rv := reflect.Indirect(reflect.ValueOf(tx.Statement.Dest))
			if rv.Kind() == reflect.Slice {
				insertSizes = append(insertSizes, rv.Len())
			}
		},
	); err != nil {
		t.Fatalf("register batch-size observer callback: %v", err)
	}

	// Default knobs: BufferSize 256 comfortably holds the burst, BatchSize
	// 64 is the production batch the split must respect.
	w := NewWorker(Config{DB: db, FlushInterval: time.Hour})
	stop := w.Start(context.Background())
	defer stop()

	const total = 2*DefaultBatchSize + 7 // two full batches plus a short tail
	for i := 0; i < total; i++ {
		w.Submit(sampleRecord(fmt.Sprintf("batch.fill.%03d", i)))
	}

	if n, err := repository.CountAdminAuditLogs(db); err != nil || n != 0 {
		t.Fatalf("before stop, no periodic flush may have run (interval is 1h), got %d rows (err=%v)", n, err)
	}
	if got := w.DroppedTotal(); got != 0 {
		t.Fatalf("the burst fits the buffer (%d < %d), so nothing may be dropped, got %d", total, DefaultBufferSize, got)
	}

	stop()

	// The split shape: exactly three inserts — two capped at the batch
	// size, then the remainder — and no fourth empty round.
	wantSizes := []int{DefaultBatchSize, DefaultBatchSize, total - 2*DefaultBatchSize}
	if !reflect.DeepEqual(insertSizes, wantSizes) {
		t.Fatalf("a drain of %d records at batch size %d must insert %v, got %v", total, DefaultBatchSize, wantSizes, insertSizes)
	}

	// Every record landed exactly once across the split rounds.
	if n, err := repository.CountAdminAuditLogs(db); err != nil || n != total {
		t.Fatalf("after stop, all %d records must be persisted across the batch split, got %d (err=%v)", total, n, err)
	}
	var distinct int64
	if err := db.Model(&model.AdminAuditLog{}).Distinct("action").Count(&distinct).Error; err != nil {
		t.Fatalf("count distinct actions: %v", err)
	}
	if distinct != total {
		t.Fatalf("all %d submitted actions are distinct, so distinct=%d means some record was duplicated or lost in the split", total, distinct)
	}
}
