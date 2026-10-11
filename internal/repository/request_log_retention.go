// Package repository provides the request-log rolling retention: the pure
// deletion predicate and the bounded single-round sweep the background
// retention loop drives. The retention window itself (days, 0 = keep
// forever) is the request-log-retention system setting; this file owns
// what "expired" means and how one round removes an expired row's whole
// audit triple — the request_logs row, its request_log_bodies row, and the
// stream capture file under the bodies directory.
package repository

import (
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway/capture"
	"github.com/yolorouter/yolorouter/internal/model"
)

// DefaultRequestLogRetentionBatchLimit caps how many expired request_logs
// rows one retention round deletes (the body rows and stream files riding
// along make it three removals per row, so the cap bounds the whole
// triple). The cap keeps every round a small, quick transaction instead of
// one unbounded DELETE that would pin the audit table (and its writer, the
// gateway's live request path) while years of backlog drain; rounds simply
// repeat, so any backlog converges over time. Named constant with an
// injected override: callers pass their own limit, and a zero or negative
// limit falls back to this default.
const DefaultRequestLogRetentionBatchLimit = 500

// requestLogRetentionCutoff returns the deletion boundary for a retention
// of retentionDays at instant now: a row is expired when its created_at is
// STRICTLY before the cutoff. Both the pure predicate below and the
// sweep's SQL go through this one function, so the tested definition of
// "expired" and the query that executes it can never drift apart.
func requestLogRetentionCutoff(now time.Time, retentionDays int) time.Time {
	return now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
}

// RequestLogExpired reports whether a request_logs row with the given
// created_at must be deleted under a retention of retentionDays at instant
// now. Zero means keep forever — the shipped default, so an upgraded
// deployment loses nothing until an admin opts in. Any negative value
// (which the write side's 0..3650 domain cannot produce — only a corrupt
// row could) also keeps forever: a nonsensical setting must fail toward
// keeping data, never toward "delete everything". A row created exactly at
// the cutoff is NOT expired — keeping N days includes day N itself.
func RequestLogExpired(createdAt, now time.Time, retentionDays int) bool {
	if retentionDays <= 0 {
		return false
	}
	return createdAt.Before(requestLogRetentionCutoff(now, retentionDays))
}

// SweepExpiredRequestLogs deletes ONE bounded round of expired request
// logs: up to batchLimit request_logs rows whose created_at is strictly
// older than now − retentionDays×24h (oldest first), each selected row's
// request_log_bodies row in the same transaction, and each row's stream
// capture file (bodiesDir/<request_id>.stream) right after the commit.
// Returns how many request_logs rows the round removed.
//
// retentionDays <= 0 removes nothing (keep forever — the same guard the
// predicate applies, enforced here again so the query itself can never run
// with a window that would match every row). batchLimit <= 0 falls back to
// DefaultRequestLogRetentionBatchLimit. Rounds are idempotent and meant to
// be repeated by the background loop until one returns 0; shrinking the
// retention creates a backlog that drains over several rounds, never in
// one synchronous purge.
//
// Stream files are removed strictly best-effort AFTER the database rows
// commit. A file that is already gone is the expected case (non-stream
// rows never had one); any other removal error is ignored and the file is
// left behind as an orphan — reclaiming crash-window orphans is
// deliberately out of scope, because the opposite order (file first, rows
// after) would leave committed body rows pointing at deleted files
// whenever the transaction fails. An empty bodiesDir skips the file pass
// entirely — unresolved directory, the same convention the gateway's
// stream capture uses — rather than unlinking paths relative to the
// working directory.
func SweepExpiredRequestLogs(db *gorm.DB, bodiesDir string, now time.Time, retentionDays, batchLimit int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	if batchLimit <= 0 {
		batchLimit = DefaultRequestLogRetentionBatchLimit
	}

	// Oldest first, so when the cap bites the most-expired rows leave first
	// and a backlog drains in arrival order.
	var expired []model.RequestLog
	if err := db.Where("created_at < ?", requestLogRetentionCutoff(now, retentionDays)).
		Order("created_at ASC, id ASC").
		Limit(batchLimit).
		Find(&expired).Error; err != nil {
		return 0, err
	}
	if len(expired) == 0 {
		return 0, nil
	}
	requestIDs := make([]string, 0, len(expired))
	for _, row := range expired {
		requestIDs = append(requestIDs, row.RequestID)
	}

	// The two DELETEs share one transaction so a body row can never survive
	// its request_logs row (or vice versa): the triple leaves together or
	// not at all. There is no foreign key between the tables — the 1:1 is
	// enforced by the unique index and this cascade, not by the schema.
	// The agent-session turn rows join the same cascade: a turn row's
	// request ids must never outlive the request_logs row they name (and a
	// resend supplement references its PREDECESSOR's request id, which this
	// same request_ids list covers because retention deletes oldest-first
	// — the predecessor of an expired request is at least as old).
	var deleted int64
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("request_id IN ?", requestIDs).Delete(&model.RequestLogBody{}).Error; err != nil {
			return err
		}
		if err := tx.Where("request_id IN ?", requestIDs).Delete(&model.AgentSessionTurn{}).Error; err != nil {
			return err
		}
		res := tx.Where("request_id IN ?", requestIDs).Delete(&model.RequestLog{})
		if res.Error != nil {
			return res.Error
		}
		deleted = res.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}

	if bodiesDir != "" {
		for _, id := range requestIDs {
			path := filepath.Join(bodiesDir, capture.StreamFileName(id))
			// Best-effort by design; see the function comment for why the
			// error is deliberately dropped instead of failing the round.
			_ = os.Remove(path)
		}
	}
	return int(deleted), nil
}
