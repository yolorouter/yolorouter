package audit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// Production defaults for the asynchronous writer. The buffer holds a
// burst of console writes (console traffic is human-paced, so 256 pending
// records already means something is badly wrong — which is exactly when
// the drop counter's warning line earns its place in the log). The flush
// interval keeps idle cost at one tiny query every couple of seconds even
// when nothing arrives.
const (
	DefaultBufferSize    = 256
	DefaultFlushInterval = 2 * time.Second
	DefaultBatchSize     = 64
)

// Record is one audit event in flight: everything the middleware (or the
// explicit-event layer) captured, before it becomes a table row. It is a
// value, not a *model.AdminAuditLog, so the capture path never shares
// mutable state with the flush path.
type Record struct {
	// At is when the event was captured (the request's own clock, not the
	// flusher's): a row's created_at must mean "when did this happen",
	// never "when did the worker get around to writing it" — under a
	// burst the two can drift by whole flush intervals.
	At            time.Time
	AdminUserID   *uint
	AdminUsername string
	Action        string
	ResourceType  string
	ResourceID    string
	// Detail is pre-serialized JSON that has already passed the redaction
	// pipeline by the time it is submitted; the worker stores it opaque.
	Detail     string
	Status     string
	ErrorBrief string
	SourceIP   string
	UserAgent  string
	RequestID  string
}

// toRow produces the table row, stamping persistence metadata. A zero At
// (a caller that had no meaningful event time) falls back to the
// worker's now so the row can never carry a zero timestamp.
func (r Record) toRow(now time.Time) model.AdminAuditLog {
	at := r.At
	if at.IsZero() {
		at = now
	}
	return model.AdminAuditLog{
		CreatedAt:     at.UTC(),
		AdminUserID:   r.AdminUserID,
		AdminUsername: r.AdminUsername,
		Action:        r.Action,
		ResourceType:  r.ResourceType,
		ResourceID:    r.ResourceID,
		Detail:        r.Detail,
		Status:        r.Status,
		ErrorBrief:    r.ErrorBrief,
		SourceIP:      r.SourceIP,
		UserAgent:     r.UserAgent,
		RequestID:     r.RequestID,
	}
}

// Config carries the worker's dependencies and knobs. DB is required;
// BufferSize, FlushInterval, and BatchSize default when zero or negative;
// a nil Logger routes the worker's output to the process-global
// pkg/logger (the production path — cmd/yolorouter/serve.go builds the
// worker this way, and for a self-hosted deployment the log line is the
// whole observability surface for this pipeline), while tests that assert
// on log output inject an observer-backed logger.
type Config struct {
	DB            *gorm.DB
	Logger        *zap.Logger
	BufferSize    int
	FlushInterval time.Duration
	BatchSize     int
}

// Worker is the audit pipeline's persistence half: a bounded queue of
// records and one background goroutine that batch-inserts them. The
// contract with the request path is strict: Submit never blocks, never
// allocates unboundedly, and never touches the database. When the queue
// is full the NEWEST event is dropped — the alternative (blocking or
// dropping old rows) would trade a healthy console for an audit buffer —
// and one structured warning line per drop carries a running count, so
// the loss is visible in the log, which for a self-hosted deployment is
// the whole observability surface for this failure mode.
type Worker struct {
	queue        chan Record
	db           *gorm.DB
	log          *zap.Logger
	flushEvery   time.Duration
	batchSize    int
	droppedTotal atomic.Int64
}

// NewWorker builds the worker. It starts nothing — Start owns the
// goroutine — and performs no I/O, so constructing it during boot before
// migrations have run is safe; only a flush would need the schema.
func NewWorker(cfg Config) *Worker {
	size := cfg.BufferSize
	if size <= 0 {
		size = DefaultBufferSize
	}
	interval := cfg.FlushInterval
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = DefaultBatchSize
	}
	log := cfg.Logger
	if log == nil {
		log = zap.New(globalCore{})
	}
	return &Worker{
		queue:      make(chan Record, size),
		db:         cfg.DB,
		log:        log,
		flushEvery: interval,
		batchSize:  batch,
	}
}

// forwardToGlobal routes one entry to the process-global pkg/logger — the
// destination of the nil-Logger fallback below. A package-level variable
// only so the fallback's test can observe the entries; production never
// replaces it. Dispatching per entry (rather than capturing the global
// logger once, which pkg/logger deliberately does not expose as a raw
// *zap.Logger) also means a later logger.Init that swaps the global is
// honored — the same bridge shape pkg/database.GormLogger uses to route
// GORM's logging into the application logger.
var forwardToGlobal = func(ent zapcore.Entry, fields []zapcore.Field) {
	switch ent.Level {
	case zapcore.DebugLevel:
		logger.Debug(ent.Message, fields...)
	case zapcore.InfoLevel:
		logger.Info(ent.Message, fields...)
	case zapcore.WarnLevel:
		logger.Warn(ent.Message, fields...)
	default:
		logger.Error(ent.Message, fields...)
	}
}

// globalCore is the nil-Logger fallback: a zapcore.Core that forwards
// every entry to the process-global pkg/logger. Without it the fallback
// would be a discard (zap.NewNop), making the two loss-reporting warnings
// — queue-full drops and failed flushes, the only observability surface a
// self-hosted deployment has for this pipeline — invisible in production,
// where serve.go builds the worker without an explicit Logger. Enabled
// reports true unconditionally: level filtering stays single-sourced in
// pkg/logger's own cores, exactly as if the global entrypoints had been
// called directly (the retention and key-recovery loops' pattern).
type globalCore struct {
	with []zap.Field
}

func (globalCore) Enabled(zapcore.Level) bool { return true }

func (c globalCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c globalCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	all := make([]zap.Field, 0, len(c.with)+len(fields))
	all = append(all, c.with...)
	all = append(all, fields...)
	forwardToGlobal(ent, all)
	return nil
}

// Sync is a no-op: the underlying global logger's own Sync owns flushing.
func (globalCore) Sync() error { return nil }

func (c globalCore) With(fields []zapcore.Field) zapcore.Core {
	merged := make([]zap.Field, 0, len(c.with)+len(fields))
	merged = append(merged, c.with...)
	merged = append(merged, fields...)
	return globalCore{with: merged}
}

// Submit enqueues one record. It is intentionally a select-with-default:
// a full queue drops the record being submitted (not one already queued —
// pending records are committed evidence-in-waiting and keep their place)
// and bumps the running drop counter that the warning line reports. This
// is the "audit never slows the console" guarantee, and it is why Submit
// takes no context and returns no error: there is nothing the caller
// could productively do about either from a request path.
func (w *Worker) Submit(rec Record) {
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	select {
	case w.queue <- rec:
	default:
		n := w.droppedTotal.Add(1)
		w.log.Warn("admin audit queue full; dropping the newest event",
			zap.Int64("dropped_total", n),
			zap.Int("queue_capacity", cap(w.queue)),
		)
	}
}

// DroppedTotal reports how many records the queue has dropped since
// process start. Primarily for tests; the log line is the operator-facing
// surface.
func (w *Worker) DroppedTotal() int64 { return w.droppedTotal.Load() }

// Start launches the flusher goroutine and returns a stop function that
// cancels it, waits for it to exit, and — before that exit — drains every
// record still queued into one final flush. That drain is the shutdown
// half of the contract: events captured during the last moments of a
// process's life still land, so stopping the server is not a way to make
// the last console writes unauditable. Safe to call stop any number of
// times (idempotent via sync.Once, and it always waits).
func (w *Worker) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(w.flushEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Shutdown drain: flush everything still queued (in
				// batches, same as a normal tick), then exit. There is no
				// deadline here on purpose — the queue is bounded, so the
				// drain is bounded, and truncating it would silently break
				// the drain guarantee to save milliseconds.
				w.drainAndFlush()
				return
			case <-ticker.C:
				w.drainAndFlush()
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// drainAndFlush empties the queue in batch-sized inserts. One call takes
// everything currently queued — a burst that overfilled several batches
// still drains in this single tick round-trip rather than one batch per
// tick.
func (w *Worker) drainAndFlush() {
	now := time.Now().UTC()
	for {
		batch := w.takeBatch()
		if len(batch) == 0 {
			return
		}
		rows := make([]model.AdminAuditLog, len(batch))
		for i, rec := range batch {
			rows[i] = rec.toRow(now)
		}
		if err := repository.CreateAdminAuditLogs(w.db, rows); err != nil {
			// A failed flush drops the batch with a warning. Retrying
			// from the flusher would need either an unbounded retry queue
			// or a stall that backs up onto the request path; both are
			// worse than a logged, counted loss of audit rows. The next
			// tick flushes whatever arrives after.
			w.log.Warn("admin audit flush failed; batch dropped",
				zap.Int("records", len(rows)),
				zap.Error(err))
		}
	}
}

// takeBatch pulls up to batchSize records without ever blocking: the
// flusher is the only receiver and the queue is the only source, so an
// empty channel simply ends the batch.
func (w *Worker) takeBatch() []Record {
	batch := make([]Record, 0, w.batchSize)
	for len(batch) < w.batchSize {
		select {
		case rec := <-w.queue:
			batch = append(batch, rec)
		default:
			return batch
		}
	}
	return batch
}
