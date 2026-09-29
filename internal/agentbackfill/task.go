// Package agentbackfill runs the one-shot startup task that attributes
// historical request logs: rows written before the agent_client column
// existed carry the caller's masked header snapshot in request_log_bodies,
// and the same pure recognizer the live write path uses can re-derive the
// calling tool's name from that snapshot at any time. The task makes that
// re-derivation happen once per boot — small batched rounds with a pause
// between them (the rolling-retention loop's throttle shape), a cursor
// walking the table oldest-first, and a self-exit when the residue is
// drained, so a converged deployment's pass is one cheap empty query.
//
// The task owns ONLY the schedule: when a round happens and when the pass
// ends. What a round scans and how it guards its update is entirely the
// engine's (repository.BackfillAgentClientRound); nothing here re-implements
// any of it.
package agentbackfill

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// DefaultBatchSize is the per-round scan cap in production; it aliases the
// engine's named default so the two never drift apart.
const DefaultBatchSize = repository.DefaultAgentBackfillBatchSize

// DefaultBatchDelay is the pause inserted between two consecutive rounds of
// one pass, so a large history drains as many small quiet queries rather
// than one continuous scan competing with the gateway's live write path —
// the same idea as the key auto-recovery loop's probe gap, applied to
// database work instead of upstream calls.
const DefaultBatchDelay = 100 * time.Millisecond

// Config carries the task's dependencies and knobs. DB is required;
// Recognize, BatchSize, and BatchDelay default when zero or nil.
type Config struct {
	DB *gorm.DB
	// Recognize promotes a stored masked header snapshot to a tool name.
	// nil means the production recognizer below; tests inject a stub to
	// pin the schedule without the parsing rules in play.
	Recognize repository.AgentClientRecognizer
	// BatchSize caps how many rows one round scans; zero or negative falls
	// back to DefaultBatchSize.
	BatchSize int
	// BatchDelay is the pause between rounds of one pass; negative values
	// fall back to DefaultBatchDelay, zero is honored (no pause — the test
	// seam, and harmless for a converged pass that runs a single round).
	BatchDelay time.Duration
}

// Task is the single-background-goroutine startup pass. Construct with
// NewTask, launch with Start (or drive synchronously with Run); it holds no
// goroutine until started, cannot be started twice usefully — a second
// Start simply runs another idempotent pass — and every pass is safe to
// repeat because the engine only ever fills NULL columns.
type Task struct {
	db         *gorm.DB
	recognize  repository.AgentClientRecognizer
	batchSize  int
	batchDelay time.Duration
}

// NewTask builds the task with defaults applied. It performs no I/O and
// reads no rows; everything happens once Run is called.
func NewTask(cfg Config) *Task {
	recognize := cfg.Recognize
	if recognize == nil {
		recognize = recognizeAgentClient
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	batchDelay := cfg.BatchDelay
	if batchDelay < 0 {
		batchDelay = DefaultBatchDelay
	}
	return &Task{
		db:         cfg.DB,
		recognize:  recognize,
		batchSize:  batchSize,
		batchDelay: batchDelay,
	}
}

// recognizeAgentClient is the production recognizer adapter. It keeps only
// the client half of gateway.AgentFromHeaderSnapshot on purpose: the
// session half of a historical snapshot is the sanitizer's redaction
// sentinel (pre-allowlist captures masked the session headers), and the
// backfill must never persist that literal as if it were an id. Dropping
// the value here makes the engine's input incapable of carrying it.
func recognizeAgentClient(snapshot []byte) string {
	client, _ := gateway.AgentFromHeaderSnapshot(snapshot)
	return client
}

// Start launches the pass on its own goroutine and returns a stop function
// that cancels it and waits for the goroutine to exit. The pass is one
// bounded walk of the table, not a loop: it exits by itself once the
// residue is drained (or a round fails), so the goroutine's lifetime is
// the pass's lifetime. Whatever the pass left behind — rows it could not
// attribute, or a round that failed — is exactly what the next startup's
// pass rescans, because the engine only ever considers rows whose
// agent_client is still NULL. Safe to call the stop function any number of
// times; the pass dies with the passed context as well.
func (t *Task) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		t.Run(ctx)
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(cancel)
		<-done
	}
}

// Run drives the whole pass synchronously: batched rounds under a cursor
// until one comes back short, a round fails, or ctx is done. A failed or
// cancelled round logs a warning and ends the pass rather than retrying in
// process — the untouched rows keep their NULL columns, which is precisely
// the residue marker the next startup's pass picks up, so an in-process
// retry loop would only duplicate what the boot cycle already provides. A
// pass that attributed nothing stays at debug level: every converged boot
// runs one empty round, and an info line per boot saying "0 rows" would be
// pure noise. A pass that attributed rows reports the count once, the
// operator's "where did the tool names on my old logs come from" audit
// trail.
func (t *Task) Run(ctx context.Context) {
	var cursor uint
	attributed := 0
	for {
		lastID, scanned, updated, err := repository.BackfillAgentClientRound(t.db.WithContext(ctx), cursor, t.batchSize, t.recognize)
		if err != nil {
			logger.Warn("agent attribution backfill round failed; remaining rows stay NULL for the next startup",
				zap.Error(err),
				zap.Uint("scanned_through_id", lastID))
			return
		}
		cursor = lastID
		attributed += updated
		if scanned < t.batchSize {
			break
		}
		// The throttle: yield between rounds so the live write path never
		// contends with one continuous scan. Cancellation is checked here
		// rather than only via the query context so a stopped task waits at
		// most one round's work, not a delay, before its goroutine exits.
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.batchDelay):
		}
	}
	if attributed == 0 {
		logger.Debug("agent attribution backfill found nothing to attribute")
		return
	}
	logger.Info("agent attribution backfill attributed historical request logs",
		zap.Int("rows", attributed))
}
