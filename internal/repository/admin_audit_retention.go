// Package repository provides the admin-audit retention: the pure deletion
// predicate and the bounded single-round sweep the audit retention loop
// drives. The retention window itself (days, 0 = keep forever) is the
// admin_audit_retention_days system setting; this file owns what "expired"
// means for an admin_audit_logs row and how one round removes them. Kept
// apart from admin_audit_repository.go on purpose — that file is the
// append-only INSERT shape, and the deletion statement lives here with the
// sweep that is its only sanctioned caller.
package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// DefaultAdminAuditRetentionBatchLimit caps how many expired
// admin_audit_logs rows one retention round deletes. Console audit volume is
// human-paced, so the cap is less about load than about keeping every round
// a small, quick transaction instead of one unbounded DELETE that would pin
// the table (and its writer, the audit capture worker's flush) while a
// backlog drains; rounds simply repeat, so any backlog converges over time.
// Named constant with an injected override, same contract as the
// request-log retention batch limit.
const DefaultAdminAuditRetentionBatchLimit = 500

// adminAuditRetentionCutoff returns the deletion boundary for a retention
// of retentionDays at instant now: a row is expired when its created_at is
// STRICTLY before the cutoff. Both the pure predicate below and the sweep's
// SQL go through this one function, so the tested definition of "expired"
// and the query that executes it can never drift apart.
func adminAuditRetentionCutoff(now time.Time, retentionDays int) time.Time {
	return now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
}

// AdminAuditExpired reports whether an admin_audit_logs row with the given
// created_at must be deleted under a retention of retentionDays at instant
// now. Zero means keep forever. Any negative value (which the write side's
// 0..3650 domain cannot produce — only a corrupt row could) also keeps
// forever: a nonsensical setting must fail toward keeping evidence, never
// toward "delete everything". A row created exactly at the cutoff is NOT
// expired — keeping N days includes day N itself.
func AdminAuditExpired(createdAt, now time.Time, retentionDays int) bool {
	if retentionDays <= 0 {
		return false
	}
	return createdAt.Before(adminAuditRetentionCutoff(now, retentionDays))
}

// SweepExpiredAdminAuditLogs deletes ONE bounded round of expired admin
// audit rows: up to batchLimit rows whose created_at is strictly older than
// now − retentionDays×24h (oldest first), removed by id in one statement.
// Returns how many rows the round removed.
//
// retentionDays <= 0 removes nothing (keep forever — the same guard the
// predicate applies, enforced here again so the query itself can never run
// with a window that would match every row). batchLimit <= 0 falls back to
// DefaultAdminAuditRetentionBatchLimit. Rounds are idempotent and meant to
// be repeated by the background loop until one returns 0; shrinking the
// retention creates a backlog that drains over several rounds, never in one
// synchronous purge. Unlike the request-log sweep there is no body row or
// capture file riding along — the audit row is the whole record — so the
// round is a single DELETE on ids selected in the same transaction.
func SweepExpiredAdminAuditLogs(db *gorm.DB, now time.Time, retentionDays, batchLimit int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	if batchLimit <= 0 {
		batchLimit = DefaultAdminAuditRetentionBatchLimit
	}

	// Oldest first, so when the cap bites the most-expired rows leave first
	// and a backlog drains in arrival order. The select and the delete share
	// one transaction so a row inserted between the two statements with a
	// created_at older than the cutoff (a clock-skewed capture) can never be
	// deleted without having been selected, nor selected and skipped.
	var deleted int64
	err := db.Transaction(func(tx *gorm.DB) error {
		var ids []int64
		if err := tx.Model(&model.AdminAuditLog{}).
			Where("created_at < ?", adminAuditRetentionCutoff(now, retentionDays)).
			Order("created_at ASC, id ASC").
			Limit(batchLimit).
			Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		res := tx.Where("id IN ?", ids).Delete(&model.AdminAuditLog{})
		if res.Error != nil {
			return res.Error
		}
		deleted = res.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(deleted), nil
}
