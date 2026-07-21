// Package repository additions for M5: RequestLog write path. Query/filter
// (PRD §6.8) is a separate module — M5 only writes rows. See design doc
// .claude/docs/2026-07-20-m5-gateway-design.md §3.2.
package repository

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/yolorouter/yolorouter-ce/internal/model"
)

// CreateRequestLog inserts one gateway request audit row. The gateway always
// has a complete RequestLog to write (even on failure — status_code +
// fail_reason record what happened), so there is no sparse-update path here.
func CreateRequestLog(db *gorm.DB, log *model.RequestLog) error {
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now().UTC()
	}
	return db.Create(log).Error
}

// IncrementAPIKeyBudgetSpent atomically adds cents to one key's cumulative
// spend. The gateway is the only writer (M4 stored the column but never wrote
// it). Used after a successful upstream response so budget exhaustion is
// visible to the next request's pre-check (PRD §6.5 step 3 / GATE-02).
//
// UPDATE ... SET budget_spent_cents = budget_spent_cents + ? is a single
// statement, so concurrent gateway requests on the same key accumulate
// correctly without a read-then-write race.
func IncrementAPIKeyBudgetSpent(db *gorm.DB, apiKeyID uint, cents int64) error {
	return db.Model(&model.APIKey{}).Where("id = ?", apiKeyID).
		UpdateColumn("budget_spent_cents", gorm.Expr("budget_spent_cents + ?", cents)).Error
}

// UpsertRequestLogBody inserts or (on duplicate request_id) updates the 1:1
// body row for one gateway request (PRD §6.8.4, Codex #5). UNIQUE(request_id)
// + ON CONFLICT DO UPDATE makes finalize idempotent under retry/double-call
// and enforces true 1:1. Best-effort caller (gateway finalize): a failure is
// logged, not escalated — the request_logs billing row is already written.
//
// created_at is deliberately excluded from DoUpdates (code-review finding):
// it must keep recording when the row was FIRST created, not get bumped
// forward by a later conflicting write.
func UpsertRequestLogBody(db *gorm.DB, body *model.RequestLogBody) error {
	if body.CreatedAt.IsZero() {
		body.CreatedAt = time.Now().UTC()
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "request_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"request_headers", "request_body", "upstream_request_body",
			"response_body", "upstream_response_body",
			"stream_body_path", "stream_body_truncated",
		}),
	}).Create(body).Error
}
