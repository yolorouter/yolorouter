// Package repository is the pure data-access layer for the user/session
// tables — no business judgment here (that's internal/service's job), just
// reads and writes against internal/model structs.
package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// CountBootstrapUsers reports how many bootstrap accounts exist — 0 or 1
// by schema (partial unique index on is_bootstrap), used to decide whether
// first-run setup is still available and to settle the concurrent-setup
// race. Counting local accounts instead would be wrong once admins can
// provision local members: those don't make setup "already done" on an
// instance whose bootstrap admin was somehow never created, and the setup
// race must stay keyed to the flag the unique index actually guards.
func CountBootstrapUsers(db *gorm.DB) (int64, error) {
	var count int64
	err := db.Model(&model.User{}).Where("is_bootstrap = ?", true).Count(&count).Error
	return count, err
}

// CreateUser inserts a new user row, populating user.ID on success.
func CreateUser(db *gorm.DB, user *model.User) error {
	return db.Create(user).Error
}

// FindLocalUserByUsername returns the local (password-login) account with
// that username, or gorm.ErrRecordNotFound. Password login deliberately
// only ever matches a local account: externally-provisioned users have
// an empty password hash and must never be reachable through the
// password form, even by username collision. Callers must not
// distinguish not-found from a wrong password: never reveal whether an
// account exists, only "invalid username or password".
func FindLocalUserByUsername(db *gorm.DB, username string) (*model.User, error) {
	var user model.User
	if err := db.Where("username = ? AND is_local = ?", username, true).First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// FindUserByID returns gorm.ErrRecordNotFound if id doesn't exist.
func FindUserByID(db *gorm.DB, id uint) (*model.User, error) {
	var user model.User
	if err := db.Where("id = ?", id).First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// UpdateUserPasswordHash overwrites the stored password hash.
func UpdateUserPasswordHash(db *gorm.DB, id uint, passwordHash string, now time.Time) error {
	return db.Model(&model.User{}).Where("id = ?", id).
		Updates(map[string]interface{}{"password_hash": passwordHash, "updated_at": now}).Error
}

// ListUsers returns every account in creation order (id ascending) — the
// admin's user directory and the data source for "filter by user"
// dropdowns. The user base is company-internal (tens, not millions), so
// no pagination yet.
func ListUsers(db *gorm.DB) ([]model.User, error) {
	var users []model.User
	if err := db.Order("id ASC").Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// RecordLoginFailure atomically increments the user's consecutive
// failed-login counter and applies a lock once it reaches lockThreshold.
// If the user's
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
func RecordLoginFailure(db *gorm.DB, userID uint, now time.Time, lockThreshold int, lockDuration time.Duration) (*time.Time, error) {
	lockedUntil := now.Add(lockDuration)
	var result struct {
		LockedUntil *time.Time `gorm:"column:locked_until"`
	}
	err := db.Raw(`
		UPDATE users
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
	`, now, now, lockThreshold, lockedUntil, now, userID).Scan(&result).Error
	if err != nil {
		return nil, err
	}
	return result.LockedUntil, nil
}

// RecordLoginSuccess atomically clears the failed-login counter and lock,
// and stamps last_login_at.
func RecordLoginSuccess(db *gorm.DB, userID uint, now time.Time) error {
	return db.Exec(`
		UPDATE users SET failed_login_count = 0, locked_until = NULL, last_login_at = ?, updated_at = ?
		WHERE id = ?
	`, now, now, userID).Error
}

// UpdateUserStatus flips one account's enabled/disabled flag inside tx.
// The last-admin guard lives in the service layer (it needs the whole
// decision context); this writer only performs the mutation.
func UpdateUserStatus(tx *gorm.DB, id uint, status int, now time.Time) error {
	return tx.Model(&model.User{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "updated_at": now}).Error
}

// UpdateUserRole rewrites one account's role inside tx.
func UpdateUserRole(tx *gorm.DB, id uint, role string, now time.Time) error {
	return tx.Model(&model.User{}).Where("id = ?", id).
		Updates(map[string]any{"role": role, "updated_at": now}).Error
}

// CountEnabledAdminsExcluding counts enabled administrators other than
// excludeID — the input to the "never leave zero active administrators"
// guard. Run inside the same transaction as the mutation it protects, so
// two concurrent demotions cannot both observe each other as the
// remaining admin.
func CountEnabledAdminsExcluding(tx *gorm.DB, excludeID uint) (int64, error) {
	var count int64
	err := tx.Model(&model.User{}).
		Where("role = ? AND status = ? AND id <> ?", model.RoleAdmin, model.UserStatusEnabled, excludeID).
		Count(&count).Error
	return count, err
}

// UserDirectoryAggregates carries the per-user figures the admin's user
// directory shows next to each account: how many API keys the account
// owns, its lifetime known spend, and which login providers it arrived
// through. All three are keyed by user id and fetched in one grouped
// query each — never per-row.
type UserDirectoryAggregates struct {
	KeyCounts     map[uint]int64
	SpendMicros   map[uint]int64
	ProviderNames map[uint][]string
}

// LoadUserDirectoryAggregates fetches the directory aggregates for every
// account in three grouped queries.
func LoadUserDirectoryAggregates(db *gorm.DB) (*UserDirectoryAggregates, error) {
	agg := &UserDirectoryAggregates{
		KeyCounts:     map[uint]int64{},
		SpendMicros:   map[uint]int64{},
		ProviderNames: map[uint][]string{},
	}

	var keyRows []struct {
		UserID uint  `gorm:"column:user_id"`
		N      int64 `gorm:"column:n"`
	}
	if err := db.Model(&model.APIKey{}).
		Select("user_id, COUNT(*) AS n").Group("user_id").Scan(&keyRows).Error; err != nil {
		return nil, err
	}
	for _, r := range keyRows {
		agg.KeyCounts[r.UserID] = r.N
	}

	var spendRows []struct {
		UserID uint  `gorm:"column:user_id"`
		Spend  int64 `gorm:"column:spend"`
	}
	if err := db.Model(&model.RequestLog{}).
		Select("user_id, COALESCE(SUM(cost_micros), 0) AS spend").
		Where("user_id IS NOT NULL").Group("user_id").Scan(&spendRows).Error; err != nil {
		return nil, err
	}
	for _, r := range spendRows {
		agg.SpendMicros[r.UserID] = r.Spend
	}

	var identityRows []struct {
		UserID uint   `gorm:"column:user_id"`
		Name   string `gorm:"column:name"`
	}
	if err := db.Table("user_identities").
		Select("user_identities.user_id, oauth_providers.name").
		Joins("JOIN oauth_providers ON oauth_providers.id = user_identities.oauth_provider_id").
		Order("user_identities.user_id, oauth_providers.name").
		Scan(&identityRows).Error; err != nil {
		return nil, err
	}
	for _, r := range identityRows {
		agg.ProviderNames[r.UserID] = append(agg.ProviderNames[r.UserID], r.Name)
	}
	return agg, nil
}
