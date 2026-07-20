// Package repository additions for M5: RequestLog write path. Query/filter
// (PRD §6.8) is a separate module — M5 only writes rows. See design doc
// .claude/docs/2026-07-20-m5-gateway-design.md §3.2.
package repository

import (
	"time"

	"gorm.io/gorm"

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
