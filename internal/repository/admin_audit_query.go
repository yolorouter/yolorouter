// Package repository provides the admin-audit query layer: the filter
// shape the audit console endpoints share (list + CSV export), its WHERE
// composition, and the count / offset-page / keyset-page helpers. Mirrors
// the request-log query layer's split — the INSERT shape lives in
// admin_audit_repository.go; this file is read-only.
package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// ValidAuditStatuses is the wire-level allowlist for the audit endpoints'
// ?status= query param: the model's three literal statuses plus "" = no
// constraint. Centralized here (same pattern as ValidStatusClasses) so the
// list endpoint and the CSV export cannot drift apart on what a legal
// status filter is.
var ValidAuditStatuses = map[string]struct{}{
	"":                       {},
	model.AuditStatusSuccess: {},
	model.AuditStatusDenied:  {},
	model.AuditStatusError:   {},
}

// AdminAuditLogFilter is the shared query shape for the audit list and CSV
// export endpoints. All fields optional; zero value = no constraint on that
// dimension. Multiple present dimensions compose as an intersection (the
// chained WHERE clauses AND together), which is the audit console's only
// composition rule.
type AdminAuditLogFilter struct {
	// AdminUserID narrows to rows one account produced. Unauthenticated
	// audit rows (public auth endpoints before a session exists) carry
	// NULL and never match. Pointer-shaped so "filter off" (nil) stays
	// distinguishable from a real id constraint.
	AdminUserID *uint
	// Action narrows to one exact action name (success and its denied
	// twin share the name — the status filter is the dimension that
	// separates them).
	Action string
	// ResourceType narrows to one exact resource namespace
	// (system_setting, provider_key, ...).
	ResourceType string
	// ResourceID narrows to one exact resource id as the path carried it.
	ResourceID string
	Status     string
	StartTime  *time.Time // inclusive
	EndTime    *time.Time // exclusive
	Page       int
	PageSize   int
}

// applyAdminAuditFilter returns a scoped query with the filter's WHERE
// conditions applied (no pagination / order — callers add those).
func (f *AdminAuditLogFilter) applyAdminAuditFilter(db *gorm.DB) *gorm.DB {
	q := db.Model(&model.AdminAuditLog{})
	if f.AdminUserID != nil {
		q = q.Where("admin_user_id = ?", *f.AdminUserID)
	}
	if f.Action != "" {
		q = q.Where("action = ?", f.Action)
	}
	if f.ResourceType != "" {
		q = q.Where("resource_type = ?", f.ResourceType)
	}
	if f.ResourceID != "" {
		q = q.Where("resource_id = ?", f.ResourceID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.StartTime != nil {
		q = q.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		q = q.Where("created_at < ?", *f.EndTime)
	}
	return q
}

// CountAdminAuditLogsByFilter returns the total row count matching the
// filter (ignores Page/PageSize) — the pagination envelope's total.
func CountAdminAuditLogsByFilter(db *gorm.DB, f *AdminAuditLogFilter) (int64, error) {
	var n int64
	err := f.applyAdminAuditFilter(db).Count(&n).Error
	return n, err
}

// ListAdminAuditLogsByFilter returns one page of rows (newest first) plus
// the total count for the filter, so the caller can render pagination.
// Page/PageSize default to 1/20 and clamp to 1..200 — the same contract as
// the request-log list.
func ListAdminAuditLogsByFilter(db *gorm.DB, f *AdminAuditLogFilter) (rows []model.AdminAuditLog, total int64, err error) {
	total, err = CountAdminAuditLogsByFilter(db, f)
	if err != nil {
		return nil, 0, err
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	pageSize := f.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}
	err = f.applyAdminAuditFilter(db).
		Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&rows).Error
	return rows, total, err
}

// AdminAuditCursor pins a position in the audit ordering for keyset
// pagination. CSV export uses it as a natural snapshot: once the first page
// is read, the cursor is fixed, and rows captured AFTER that point never
// satisfy the WHERE predicate on later pages — no offset drift, no
// duplicate/skipped rows while the capture worker keeps flushing.
type AdminAuditCursor struct {
	CreatedAt time.Time
	ID        uint
}

// ListAdminAuditLogsKeyset returns up to limit rows ordered newest-first,
// with (created_at, id) strictly less than cursor (nil cursor = start from
// newest). The filter's range still applies. The row-value comparison
// `(created_at, id) < (?, ?)` is supported by SQLite, Postgres, and MySQL.
func ListAdminAuditLogsKeyset(db *gorm.DB, f *AdminAuditLogFilter, cursor *AdminAuditCursor, limit int) ([]model.AdminAuditLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	q := f.applyAdminAuditFilter(db)
	if cursor != nil {
		q = q.Where("(created_at, id) < (?, ?)", cursor.CreatedAt, cursor.ID)
	}
	var rows []model.AdminAuditLog
	err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}
