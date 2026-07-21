// Package repository is the pure data-access layer for M1's admin/session
// tables — no business judgment here (that's internal/service's job), just
// reads and writes against internal/model structs.
package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// CountAdmins reports how many admin rows exist — v0.1 only ever has 0 or 1
// (see design doc §4), used to decide whether first-run setup is still
// available.
func CountAdmins(db *gorm.DB) (int64, error) {
	var count int64
	err := db.Model(&model.Admin{}).Count(&count).Error
	return count, err
}

// CreateAdmin inserts a new admin row, populating admin.ID on success.
func CreateAdmin(db *gorm.DB, admin *model.Admin) error {
	return db.Create(admin).Error
}

// FindAdminByUsername returns gorm.ErrRecordNotFound if no admin has that
// username — callers must not distinguish this from a wrong password (PRD
// §6.1.3: never reveal whether an account exists, only "invalid username
// or password").
func FindAdminByUsername(db *gorm.DB, username string) (*model.Admin, error) {
	var admin model.Admin
	if err := db.Where("username = ?", username).First(&admin).Error; err != nil {
		return nil, err
	}
	return &admin, nil
}

// FindAdminByID returns gorm.ErrRecordNotFound if id doesn't exist.
func FindAdminByID(db *gorm.DB, id uint) (*model.Admin, error) {
	var admin model.Admin
	if err := db.Where("id = ?", id).First(&admin).Error; err != nil {
		return nil, err
	}
	return &admin, nil
}

// UpdateAdminPasswordHash overwrites the stored password hash.
func UpdateAdminPasswordHash(db *gorm.DB, id uint, passwordHash string, now time.Time) error {
	return db.Model(&model.Admin{}).Where("id = ?", id).
		Updates(map[string]interface{}{"password_hash": passwordHash, "updated_at": now}).Error
}

// RecordLoginFailure atomically increments the admin's consecutive
// failed-login counter and applies a lock once it reaches lockThreshold —
// see design doc §4 for the full state-transition table. If the admin's
// previous lock has already expired (locked_until <= now), this failure
// starts a fresh count of 1 instead of continuing the old count: otherwise
// the very first retry after a lock expires would immediately re-trigger
// it, which in effect never lets the lock actually expire.
//
// now and the resulting unlock time are both computed in Go and passed in
// as bound parameters rather than using SQL date-arithmetic functions
// (SQLite's datetime(...) vs Postgres's `+ interval` have incompatible
// syntax) — this keeps the statement identical across both drivers.
//
// Returns the resulting locked_until (nil if this failure didn't lock the
// account) via `RETURNING`, so the caller doesn't need a second SELECT
// just to learn whether this exact call crossed the lock threshold —
// Postgres and SQLite 3.35+ (this project's minimum, via
// modernc.org/sqlite) both support the same RETURNING syntax.
func RecordLoginFailure(db *gorm.DB, adminID uint, now time.Time, lockThreshold int, lockDuration time.Duration) (*time.Time, error) {
	lockedUntil := now.Add(lockDuration)
	var result struct {
		LockedUntil *time.Time `gorm:"column:locked_until"`
	}
	err := db.Raw(`
		UPDATE admins
		SET failed_login_count = CASE
				WHEN locked_until IS NOT NULL AND locked_until <= ? THEN 1
				ELSE failed_login_count + 1
			END,
			locked_until = CASE
				-- A fresh count of 1 is never >= lockThreshold (a threshold
				-- of 1 would mean "lock on the very first failure ever",
				-- not a real configuration this feature supports), so a
				-- just-expired lock's reset case never needs to check the
				-- new count against the threshold — it's always NULL.
				WHEN locked_until IS NOT NULL AND locked_until <= ? THEN NULL
				WHEN failed_login_count + 1 >= ? THEN ?
				ELSE NULL
			END,
			updated_at = ?
		WHERE id = ?
		RETURNING locked_until
	`, now, now, lockThreshold, lockedUntil, now, adminID).Scan(&result).Error
	if err != nil {
		return nil, err
	}
	return result.LockedUntil, nil
}

// RecordLoginSuccess atomically clears the failed-login counter and lock.
func RecordLoginSuccess(db *gorm.DB, adminID uint, now time.Time) error {
	return db.Exec(`
		UPDATE admins SET failed_login_count = 0, locked_until = NULL, updated_at = ?
		WHERE id = ?
	`, now, adminID).Error
}
