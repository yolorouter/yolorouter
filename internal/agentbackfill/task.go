// Package agentbackfill runs the one-shot startup task that attributes
// historical request logs in two passes over the same table:
//
//   - the client pass promotes the calling tool's NAME from the masked
//     header snapshot request_log_bodies stores — rows written before the
//     agent_client column existed get it re-derived by the same pure
//     recognizer the live write path uses, at any time;
//   - the session pass then promotes the tool's own SESSION id from the raw
//     data a historical row kept beside it — the verbatim request body and
//     the same snapshot — for the two tools whose captures still carry one
//     (claude-code's body metadata, codex's unmasked turn-metadata header),
//     because the rows the live path could not fill had their session
//     headers masked before the capture allowlist existed.
//
// Both passes share one shape: small batched rounds with a pause between
// them (the rolling-retention loop's throttle shape), a cursor walking the
// table oldest-first, and a self-exit when the residue is drained, so a
// converged deployment's pass is one cheap empty query. The task owns ONLY
// the schedule: when a round happens and when a pass ends. What a round
// scans and how it guards its update is entirely the engines'
// (repository.BackfillAgentClientRound, repository.BackfillAgentSessionRound);
// nothing here re-implements any of it.
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
// Recognize, ExtractSession, BatchSize, and BatchDelay default when zero or
// nil.
type Config struct {
	DB *gorm.DB
	// Recognize promotes a stored masked header snapshot to a tool name.
	// nil means the production recognizer below; tests inject a stub to
	// pin the schedule without the parsing rules in play.
	Recognize repository.AgentClientRecognizer
	// ExtractSession promotes one already-attributed row's stored capture
	// (raw request body + masked header snapshot) to the tool's own session
	// id. nil means the production extractor in session_extract.go; tests
	// inject a stub the same way they do for Recognize.
	ExtractSession repository.AgentSessionExtractor
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
// repeat because both engines only ever fill NULL columns.
type Task struct {
	db             *gorm.DB
	recognize      repository.AgentClientRecognizer
	extractSession repository.AgentSessionExtractor
	batchSize      int
	batchDelay     time.Duration
}

// NewTask builds the task with defaults applied. It performs no I/O and
// reads no rows; everything happens once Run is called.
func NewTask(cfg Config) *Task {
	recognize := cfg.Recognize
	if recognize == nil {
		recognize = recognizeAgentClient
	}
	extractSession := cfg.ExtractSession
	if extractSession == nil {
		extractSession = ExtractAgentSession
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
		db:             cfg.DB,
		recognize:      recognize,
		extractSession: extractSession,
		batchSize:      batchSize,
		batchDelay:     batchDelay,
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
// bounded walk of the table per stage, not a loop: it exits by itself once
// the residue is drained (or a round fails), so the goroutine's lifetime is
// the pass's lifetime. Whatever the pass left behind — rows it could not
// attribute, or a round that failed — is exactly what the next startup's
// pass rescans, because both engines only ever consider rows whose column
// is still NULL. Safe to call the stop function any number of times; the
// pass dies with the passed context as well.
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

// backfillRound is one engine round as the schedule sees it: bounded scan
// after the cursor, reporting the highest id it scanned, how many rows it
// scanned, and how many it attributed.
type backfillRound func(afterID uint) (lastID uint, scanned, updated int, err error)

// driveRounds is the shared schedule of both passes: rounds under a cursor
// until one comes back short, a round fails, or the context dies. It
// returns how many rows the whole pass attributed, the highest id the
// failing round scanned (for the failure log), and the error — ctx.Err()
// itself when the pass was stopped at the throttle, which the callers treat
// as the one silent exit: a stopped task is a routine shutdown, not a
// failure worth a warning. Whatever the exit, the untouched rows keep
// their NULL columns, which is precisely the residue marker the next
// startup's pass picks up, so an in-process retry loop would only duplicate
// what the boot cycle already provides.
func (t *Task) driveRounds(ctx context.Context, round backfillRound) (attributed int, lastID uint, err error) {
	var cursor uint
	for {
		var scanned, updated int
		lastID, scanned, updated, err = round(cursor)
		if err != nil {
			return attributed, lastID, err
		}
		cursor = lastID
		attributed += updated
		if scanned < t.batchSize {
			return attributed, cursor, nil
		}
		// The throttle: yield between rounds so the live write path never
		// contends with one continuous scan. Cancellation is checked here
		// rather than only via the query context so a stopped task waits at
		// most one round's work, not a delay, before its goroutine exits.
		select {
		case <-ctx.Done():
			return attributed, cursor, ctx.Err()
		case <-time.After(t.batchDelay):
		}
	}
}

// Run drives the whole task synchronously: the client pass first, then the
// session pass on its output — the session extractor dispatches on the tool
// name, so running it after attribution lets one boot recover both columns
// for rows that predate them. A failed round ends ITS pass with a warning
// rather than retrying in process; the other pass still runs, because a
// failure one engine hits (say, a query shape) says nothing about the
// other's chances, and any rows the failed pass left behind keep their NULL
// columns for the next startup either way. Each pass that attributed
// nothing stays at debug level: every converged boot runs one empty round,
// and an info line per boot saying "0 rows" would be pure noise. A pass
// that attributed rows reports the count once, the operator's "where did
// the tool names and sessions on my old logs come from" audit trail.
func (t *Task) Run(ctx context.Context) {
	t.runClientPass(ctx)
	t.runSessionPass(ctx)
}

// runClientPass is the tool-NAME pass over rows whose agent_client is still
// NULL (see repository.BackfillAgentClientRound for the engine's contract).
func (t *Task) runClientPass(ctx context.Context) {
	attributed, lastID, err := t.driveRounds(ctx, func(afterID uint) (uint, int, int, error) {
		return repository.BackfillAgentClientRound(t.db.WithContext(ctx), afterID, t.batchSize, t.recognize)
	})
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("agent attribution backfill round failed; remaining rows stay NULL for the next startup",
				zap.Error(err),
				zap.Uint("scanned_through_id", lastID))
		}
		return
	}
	if attributed == 0 {
		logger.Debug("agent attribution backfill found nothing to attribute")
		return
	}
	logger.Info("agent attribution backfill attributed historical request logs",
		zap.Int("rows", attributed))
}

// runSessionPass is the tool-SESSION pass over rows the first pass (or an
// earlier boot) already attributed to claude-code or codex whose
// agent_session_id is still NULL (see repository.BackfillAgentSessionRound
// for the engine's contract). Its log lines are its own: an operator
// watching a boot can tell the two recoveries apart.
func (t *Task) runSessionPass(ctx context.Context) {
	attributed, lastID, err := t.driveRounds(ctx, func(afterID uint) (uint, int, int, error) {
		return repository.BackfillAgentSessionRound(t.db.WithContext(ctx), afterID, t.batchSize, t.extractSession)
	})
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("agent session backfill round failed; remaining rows keep NULL sessions for the next startup",
				zap.Error(err),
				zap.Uint("scanned_through_id", lastID))
		}
		return
	}
	if attributed == 0 {
		logger.Debug("agent session backfill found nothing to backfill")
		return
	}
	logger.Info("agent session backfill restored sessions on historical request logs",
		zap.Int("rows", attributed))
}
