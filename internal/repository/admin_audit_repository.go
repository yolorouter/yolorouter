// Package repository — admin audit log persistence. This file owns the
// INSERT shape only: rows are append-only evidence, so there is no update
// or delete helper here by design. The read side (filtering, pagination,
// CSV) lives with the admin query endpoint; the only removals are the
// retention sweep's, and that task carries its own delete statement rather
// than importing one from here.
package repository

import (
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// CreateAdminAuditLogs inserts a batch of audit rows in one statement.
// The asynchronous capture worker is the only caller, and batches are how
// it keeps flush cost flat regardless of write bursts. A zero-length slice
// is a no-op so the worker can call this unconditionally after a drain
// round that found nothing. Callers keep their own CreatedAt stamping:
// rows arrive with the field already set (the worker stamps at drain time
// so a row's timestamp reflects when it was captured, not a later retry).
func CreateAdminAuditLogs(db *gorm.DB, rows []model.AdminAuditLog) error {
	if len(rows) == 0 {
		return nil
	}
	return db.Create(&rows).Error
}

// CountAdminAuditLogs is the plain row count. Test-support shaped (the
// sweep test asserts exactly-once capture per endpoint against it), but
// kept in the repository rather than raw SQL in tests so the table name
// and model stay pinned in exactly one place outside the model itself.
func CountAdminAuditLogs(db *gorm.DB) (int64, error) {
	var n int64
	err := db.Model(&model.AdminAuditLog{}).Count(&n).Error
	return n, err
}
