// Package model — the admin audit log row. Schema lives in
// migrations/{sqlite,postgres}/00052_admin_audit_logs.sql — goose owns
// DDL, GORM here is query-only (no AutoMigrate).
package model

import "time"

// Audit statuses. "success" is the normal completed write; "denied" marks
// an access-control refusal (401/403 — the attempt itself is the signal
// worth keeping); "error" marks a write that failed for any other reason
// (validation, handler failure, panic), with error_brief carrying a short
// non-sensitive reason. Exactly these three — stored as strings so a row
// in a DB dump or log line needs no decoder ring, same convention as user
// roles.
const (
	AuditStatusSuccess = "success"
	AuditStatusDenied  = "denied"
	AuditStatusError   = "error"
)

// AdminAuditLog is one console-audit row: who did what to which resource,
// from where, under which request id. Immutable by construction — the only
// writer paths are the asynchronous capture pipeline's inserts and the
// retention sweep's deletes; no handler ever updates a row.
//
// AdminUserID is NULL when the request carried no resolved identity
// (public auth endpoints before a session exists, or an unauthenticated
// probe that RequireSession refused). AdminUsername is a snapshot taken
// at capture time: renaming or deleting the account later must not make
// history unreadable, so the value is denormalized here and never joined
// live. Detail is a JSON document (field-level diffs, event context)
// already passed the redaction pipeline by the time it reaches this
// struct; the model layer does not interpret it.
type AdminAuditLog struct {
	ID uint `gorm:"column:id;primaryKey" json:"id"`
	// CreatedAt is when the event was captured — the request's own clock,
	// not the flusher's — even though persistence itself is asynchronous
	// and happens shortly afterwards. Under a write burst the queue can
	// delay the flush by whole intervals; the row must never let that
	// drift rewrite history.
	CreatedAt     time.Time `gorm:"column:created_at" json:"created_at"`
	AdminUserID   *uint     `gorm:"column:admin_user_id" json:"admin_user_id"`
	AdminUsername string    `gorm:"column:admin_username" json:"admin_username"`
	Action        string    `gorm:"column:action" json:"action"`
	ResourceType  string    `gorm:"column:resource_type" json:"resource_type"`
	ResourceID    string    `gorm:"column:resource_id" json:"resource_id"`
	Detail        string    `gorm:"column:detail" json:"detail"`
	Status        string    `gorm:"column:status" json:"status"`
	ErrorBrief    string    `gorm:"column:error_brief" json:"error_brief"`
	SourceIP      string    `gorm:"column:source_ip" json:"source_ip"`
	UserAgent     string    `gorm:"column:user_agent" json:"user_agent"`
	// RequestID is the bare UUID the request-id middleware issued for the
	// audited request — identical to that response's X-Request-Id header,
	// which makes it the reconciliation key between an audit row and one
	// specific console request. Admin requests never write request_logs,
	// so this is a correlation key, not a foreign key.
	RequestID string `gorm:"column:request_id" json:"request_id"`
}

// TableName pins the table name explicitly rather than relying on GORM's
// default pluralization — the migration created "admin_audit_logs", and an
// implicit mismatch here would only surface as a runtime "no such table"
// error.
func (AdminAuditLog) TableName() string { return "admin_audit_logs" }
