// Package model defines the GORM-mapped structs for M1's admin/session
// tables. Schema itself lives in migrations/{sqlite,postgres}/00002_create_admin_auth.sql
// — goose owns DDL, GORM here is query-only (no AutoMigrate).
package model

import "time"

// Admin is the single-row (v0.1) administrator account — see design doc
// .claude/docs/2026-07-17-m1-auth-design.md §4.
//
// PasswordHash and the lockout-state fields are explicitly json:"-":
// handlers today only ever expose a hand-picked gin.H{"username": ...},
// never this struct directly, but without the tag a future
// response.Success(c, admin) (or any other generic serialization) would
// silently leak the bcrypt hash and account-lockout internals.
type Admin struct {
	ID               uint       `gorm:"column:id;primaryKey" json:"id"`
	Username         string     `gorm:"column:username" json:"username"`
	PasswordHash     string     `gorm:"column:password_hash" json:"-"`
	FailedLoginCount int        `gorm:"column:failed_login_count" json:"-"`
	LockedUntil      *time.Time `gorm:"column:locked_until" json:"-"`
	CreatedAt        time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at" json:"-"`
}

// TableName pins the table name explicitly rather than relying on GORM's
// default pluralization — the migration created "admins", and an implicit
// mismatch here would only surface as a runtime "no such table" error.
func (Admin) TableName() string { return "admins" }
