// The one-shot startup backfill: the pass that derives turn rows for
// history written before the turn store existed (and for any request whose
// live derivation degraded to a pending marker). It replays each candidate
// request through the SAME engine the live write path uses — the previous
// body is read from request_log_bodies either way — so a backfilled
// session's turns can never disagree with a freshly written one's; the
// parity tests pin exactly that equivalence against the whole-session pass.
//
// The shape follows the other startup passes (agent attribution, provider
// key suffix): small batched rounds with a pause between them, a cursor
// walking request_logs oldest-first, and a self-exit when the residue is
// drained, so a converged deployment's pass is one cheap candidate query.
// The task owns ONLY the schedule; what a round scans and how the write is
// guarded is the engine's below.
package agentturns

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// DefaultBackfillBatchSize is the per-round candidate cap in production.
// Each candidate costs one previous-body read + parse (the same price the
// live write pays per request), so the round budget doubles as the
// database-load throttle.
const DefaultBackfillBatchSize = 50

// DefaultBackfillBatchDelay is the pause inserted between two consecutive
// rounds, so a large history drains as many small quiet queries rather
// than one continuous scan competing with the gateway's live write path —
// the same throttle idea every other startup pass here uses.
const DefaultBackfillBatchDelay = 100 * time.Millisecond

// BackfillRound processes ONE bounded round of candidate requests:
// up to limit request_logs rows carrying an agent_session_id whose turn
// store half is missing — no own-batch rows at all, or only the degraded
// pending marker — oldest first (id ASC, the same direction the live path
// writes in), each replayed through the production write engine. It
// returns the highest request id the round scanned (the caller's cursor)
// and how many rows it scanned.
//
// Idempotence is structural, like every other backfill engine here. The
// candidate query only ever names requests without a non-pending own-batch
// row (seq < SupplementSeqBase), and a successful write is exactly such a
// batch — so a re-run, or a race against the live write path, can only
// fill a request that still lacks its batch; the UNIQUE(request_id, seq)
// guard turns a genuinely raced batch into a no-op.
func BackfillRound(db *gorm.DB, afterID uint, limit int) (lastID uint, scanned int, err error) {
	if limit <= 0 {
		limit = DefaultBackfillBatchSize
	}
	var batch []model.RequestLog
	if err := db.Model(&model.RequestLog{}).
		Select("id", "request_id", "agent_session_id", "created_at").
		Where("agent_session_id IS NOT NULL AND id > ?", afterID).
		Where("NOT EXISTS (SELECT 1 FROM agent_session_turns t WHERE t.request_id = request_logs.request_id AND t.event_type <> ? AND t.seq < ?)", EventPending, SupplementSeqBase).
		Order("id ASC").
		Limit(limit).
		Find(&batch).Error; err != nil {
		return 0, 0, err
	}
	if len(batch) == 0 {
		return afterID, 0, nil
	}

	for i := range batch {
		lastID = batch[i].ID
		scanned++
		body := fetchOwnBody(db, batch[i].RequestID)
		in := RequestInput{
			SessionID:        derefSessionID(batch[i].AgentSessionID),
			RequestID:        batch[i].RequestID,
			RequestPK:        batch[i].ID,
			CreatedAt:        batch[i].CreatedAt,
			RequestBody:      body.requestBody,
			HasInlineReply:   body.hasInlineReply,
			HasStreamCapture: body.hasStreamCapture,
		}
		// The engine's error is the degrade path's cause: a pending marker
		// lands (or nothing does when the database refuses even that), and
		// the round moves on — one unprocessable request must never stop
		// the pass behind it.
		if err := RecordTurnsForRequest(db, in); err != nil {
			logger.Warn("agent turns backfill: request degraded to pending",
				zap.String("request_id", batch[i].RequestID), zap.Error(err))
		}
	}
	return lastID, scanned, nil
}

// ownBody is one request's body row projection for the backfill: the
// captured request body and the two reply-capture presences. A missing
// body row reads as an empty body — the write engine then produces the
// request's assistant_reply row (and nothing else), which is exactly how
// the live path treats a request whose body could not be captured, and it
// still marks the request as processed so the candidate query stops
// naming it.
type ownBody struct {
	requestBody      string
	hasInlineReply   bool
	hasStreamCapture bool
}

// fetchOwnBody reads one request's body row halves. Best-effort by design:
// a database failure reads as "no body" for the same reason the round
// continues past a failed write — the residue keeps the pending marker's
// contract (the next startup retries), not a stuck pass.
func fetchOwnBody(db *gorm.DB, requestID string) ownBody {
	var rows []struct {
		RequestBody    string `gorm:"column:request_body"`
		ResponseBody   string `gorm:"column:response_body"`
		StreamBodyPath string `gorm:"column:stream_body_path"`
	}
	if err := db.Raw(`
		SELECT COALESCE(request_body, '') AS request_body,
		       COALESCE(response_body, '') AS response_body,
		       COALESCE(stream_body_path, '') AS stream_body_path
		FROM request_log_bodies WHERE request_id = ? LIMIT 1`,
		requestID).Scan(&rows).Error; err != nil || len(rows) == 0 {
		return ownBody{}
	}
	return ownBody{
		requestBody:      rows[0].RequestBody,
		hasInlineReply:   rows[0].ResponseBody != "",
		hasStreamCapture: rows[0].StreamBodyPath != "",
	}
}

// derefSessionID flattens the nullable attribution column. The candidate
// query filters on IS NOT NULL, so nil is unreachable here; the flattening
// exists so a hand-mangled row degrades to the engine's no-op (empty
// session) instead of a panic.
func derefSessionID(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// Task is the single-background-goroutine startup pass. Construct with
// NewBackfillTask, launch with Start (or drive synchronously with Run);
// it holds no goroutine until started, and every pass is safe to repeat
// because the engine only ever writes requests still missing their batch.
type Task struct {
	db         *gorm.DB
	batchSize  int
	batchDelay time.Duration
}

// NewBackfillTask builds the task with defaults applied. It performs no
// I/O and reads no rows; everything happens once Run is called.
func NewBackfillTask(db *gorm.DB) *Task {
	return &Task{
		db:         db,
		batchSize:  DefaultBackfillBatchSize,
		batchDelay: DefaultBackfillBatchDelay,
	}
}

// Start launches the pass on its own goroutine and returns a stop function
// that cancels it and waits for the goroutine to exit. The pass is one
// bounded walk of the missing-turns residue, not a loop: it exits by
// itself once the residue is drained (or a round fails), so a converged
// deployment's every boot runs exactly one cheap candidate query.
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

// Run drives the whole pass synchronously: rounds under a cursor until one
// comes back short, a round fails, or the context dies. A failed round
// ends the pass with a warning rather than retrying in process; the rows
// it left behind keep their pending markers (or their absence) for the
// next startup, exactly the boot-cycle retry the other passes rely on. A
// pass that backfilled nothing stays quiet — every converged boot runs
// one empty round, and an info line per boot saying "0 requests" would be
// pure noise.
func (t *Task) Run(ctx context.Context) {
	processed := 0
	var cursor uint
	for {
		lastID, scanned, err := BackfillRound(t.db.WithContext(ctx), cursor, t.batchSize)
		processed += scanned
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("agent turns backfill round failed; remaining history stays for the next startup",
					zap.Error(err), zap.Uint("scanned_through_id", lastID))
			}
			break
		}
		cursor = lastID
		if scanned < t.batchSize {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(t.batchDelay):
		}
	}
	if processed > 0 {
		logger.Info("agent turns backfill derived historical requests",
			zap.Int("requests", processed))
	}
}

// BackfillAll drives ONE full synchronous pass over the missing-turns
// residue — every round back to back, no schedule, no pauses — returning
// how many requests it processed and the first round error (if any; the
// pass stops there, the same stop Task.Run applies). The Task's background
// schedule serves production; this serves callers that need the whole pass
// done now: tests standing in for the startup pass over seeded history,
// and any future one-shot command form.
func BackfillAll(ctx context.Context, db *gorm.DB) (int, error) {
	processed := 0
	var cursor uint
	for {
		lastID, scanned, err := BackfillRound(db.WithContext(ctx), cursor, DefaultBackfillBatchSize)
		processed += scanned
		if err != nil {
			return processed, err
		}
		cursor = lastID
		if scanned < DefaultBackfillBatchSize {
			return processed, nil
		}
		if err := ctx.Err(); err != nil {
			return processed, err
		}
	}
}
