// Package keysuffixbackfill runs the one-shot startup task that fills the
// provider_keys.key_suffix column for keys stored before it existed: a
// bounded walk of the empty-suffix residue that decrypts each row's
// ciphertext in-process and writes the plaintext's last four characters,
// using the exact derivation the insert path uses so a backfilled column
// can never disagree with a freshly written one.
//
// The zero-plaintext rule is the package's defining constraint. The
// decryption happens here, in memory, and the plaintext's only journey is
// ciphertext -> string -> last four runes -> column: it never reaches a
// log line, an error message, or any persistence but the four display
// characters themselves. Log output counts rows, and nothing else.
//
// The shape follows the agent-attribution backfill: small batched rounds
// with a pause between them, a cursor walking the table oldest-first, and
// a self-exit when the residue is drained, so a converged deployment's
// pass is one cheap empty query. The task owns ONLY the schedule; what a
// round scans and how it guards its update is entirely the engine's
// (repository.BackfillProviderKeySuffixRound).
package keysuffixbackfill

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// DefaultBatchSize is the per-round scan cap in production; it aliases the
// engine's named default so the two never drift apart.
const DefaultBatchSize = repository.DefaultProviderKeySuffixBatchSize

// DefaultBatchDelay is the pause inserted between two consecutive rounds,
// so a large key pool drains as many small quiet queries instead of one
// continuous scan (each row doing an AES-GCM decryption) competing with the
// console's live write path.
const DefaultBatchDelay = 100 * time.Millisecond

// Config carries the task's dependencies and knobs. DB and Secrets are
// required; BatchSize and BatchDelay default when zero or negative.
type Config struct {
	DB *gorm.DB
	// Secrets decrypts each row's stored ciphertext. The startup
	// master-key fingerprint check has already validated the key by the
	// time this task runs, so a decryption failure here means a corrupt
	// row, not a wrong process key — the row keeps its empty suffix for
	// the next startup to retry.
	Secrets crypto.SecretBox
	// BatchSize caps how many rows one round scans; zero or negative falls
	// back to DefaultBatchSize.
	BatchSize int
	// BatchDelay is the pause between rounds; negative values fall back to
	// DefaultBatchDelay, zero is honored (no pause — the test seam, and
	// harmless for a converged pass that runs a single round).
	BatchDelay time.Duration
}

// Task is the single-background-goroutine startup pass. Construct with
// NewTask, launch with Start (or drive synchronously with Run); it holds
// no goroutine until started, and every pass is safe to repeat because the
// engine only ever fills still-empty suffixes.
type Task struct {
	db         *gorm.DB
	secrets    crypto.SecretBox
	batchSize  int
	batchDelay time.Duration
}

// NewTask builds the task with defaults applied. It performs no I/O,
// reads no rows, and decrypts nothing; everything happens once Run is
// called.
func NewTask(cfg Config) *Task {
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
		secrets:    cfg.Secrets,
		batchSize:  batchSize,
		batchDelay: batchDelay,
	}
}

// Start launches the pass on its own goroutine and returns a stop function
// that cancels it and waits for the goroutine to exit. The pass is one
// bounded walk of the empty-suffix residue, not a loop: it exits by itself
// once the residue is drained (or a round fails), so the goroutine's
// lifetime is the pass's lifetime. Whatever the pass left behind — rows it
// could not derive, or a round that failed — keeps the empty suffix that
// is precisely the marker the next startup's pass picks up. Safe to call
// the stop function any number of times; the pass dies with the passed
// context as well.
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

// logInfo and logWarn route the task's reporting to the process-global
// pkg/logger — the same surface every other startup pass here logs to.
// Package-level variables only so tests can observe (and assert the
// zero-plaintext rule on) every line the task emits; production never
// replaces them.
var (
	logInfo = logger.Info
	logWarn = logger.Warn
)

// Run drives the whole pass synchronously: rounds under a cursor until one
// comes back short, a round fails, or the context dies. A failed round
// ends the pass with a warning rather than retrying in process; the rows it
// left behind keep their empty suffixes for the next startup, so an
// in-process retry loop would only duplicate what the boot cycle already
// provides. A pass that filled nothing stays at debug-quiet by reporting
// nothing at all: every converged boot runs one empty round, and an info
// line per boot saying "0 rows" would be pure noise. A pass that filled
// rows reports the count once — the operator's "where did the suffixes on
// my old keys come from" audit trail.
func (t *Task) Run(ctx context.Context) {
	filled := 0
	underivable := 0
	var cursor uint
	for {
		lastID, scanned, updated, undervied, err := t.round(ctx, cursor)
		filled += updated
		underivable += undervied
		if err != nil {
			if ctx.Err() == nil {
				logWarn("provider key suffix backfill round failed; remaining rows stay empty for the next startup",
					zap.Error(err),
					zap.Uint("scanned_through_id", lastID))
			}
			break
		}
		cursor = lastID
		if scanned < t.batchSize {
			break
		}
		// The throttle: yield between rounds so the live write path never
		// contends with one continuous decrypting scan. Cancellation is
		// checked here rather than only via the query context so a stopped
		// task waits at most one round's work before its goroutine exits.
		select {
		case <-ctx.Done():
		case <-time.After(t.batchDelay):
		}
	}
	if underivable > 0 {
		// Count only — never the ids (harmless but unnecessary), never any
		// key material (which is the point): a row whose ciphertext will
		// not decrypt is a corruption signal, and the count is all an
		// operator needs to go looking.
		logWarn("provider key suffix backfill could not derive some suffixes; rows stay empty for the next startup",
			zap.Int("rows", underivable))
	}
	if filled > 0 {
		logInfo("provider key suffix backfill filled historical keys",
			zap.Int("rows", filled))
	}
}

// round runs one engine round. The deriver it injects is the whole
// zero-plaintext story in miniature: decrypt, derive the last four runes,
// return — the plaintext lives in one local variable for one call, and no
// path out of this function carries it.
func (t *Task) round(ctx context.Context, afterID uint) (lastID uint, scanned, updated, underivable int, err error) {
	derive := func(encrypted string) string {
		plaintext, decErr := t.secrets.Decrypt(encrypted)
		suffix := ""
		if decErr == nil {
			suffix = provider.KeySuffixFor(plaintext)
		}
		if suffix == "" {
			underivable++
		}
		return suffix
	}
	lastID, scanned, updated, err = repository.BackfillProviderKeySuffixRound(t.db.WithContext(ctx), afterID, t.batchSize, derive)
	return lastID, scanned, updated, underivable, err
}
