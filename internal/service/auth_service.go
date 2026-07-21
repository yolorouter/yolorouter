package service

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

const (
	// LoginLockThreshold and LoginLockDuration implement PRD AUTH-04's
	// tentative "lock for 15 minutes after 5 consecutive failures" rule
	// (design doc §9 / PRD TBD-03).
	LoginLockThreshold = 5
	LoginLockDuration  = 15 * time.Minute
	// SessionTTL implements PRD AUTH-08's tentative 24-hour session TTL.
	SessionTTL = 24 * time.Hour
)

// LockedError carries the exact unlock time for an AccountLoginLocked
// response — a plain sentinel error can't carry this per-call value, and
// the frontend needs it to render a countdown (design doc §5).
type LockedError struct {
	LockedUntil time.Time
}

func (e *LockedError) Error() string { return errcode.ErrorMessages[errcode.AccountLoginLocked] }

// CheckState reports whether first-run setup has already been completed.
func CheckState(db *gorm.DB) (bool, error) {
	count, err := repository.CountAdmins(db)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// Setup creates the first (and, in v0.1, only) admin and immediately signs
// them in — PRD §5.1 step 4: creation success lands on the empty-state
// overview. Returns the new admin and a freshly issued session id.
//
// The CountAdmins check below is only a fast-path optimization (skip
// hashing/inserting when setup is obviously already done) — it does NOT by
// itself prevent two concurrent first-run requests from both observing
// count==0 and both attempting to insert. The real guarantee is the
// admins.singleton_guard UNIQUE constraint (migration
// 00002_create_admin_auth.sql): at most one of any concurrent CreateAdmin
// calls can ever succeed, and admin creation + session issuance run in one
// transaction so a losing/failed attempt never leaves a half-initialized
// state (an admin row with no session, or vice versa).
func Setup(db *gorm.DB, username, password string, now time.Time) (*model.Admin, string, error) {
	count, err := repository.CountAdmins(db)
	if err != nil {
		return nil, "", err
	}
	if count > 0 {
		return nil, "", errcode.ErrAccountSetupAlreadyDone
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, "", err
	}

	admin := &model.Admin{Username: username, PasswordHash: hash, CreatedAt: now, UpdatedAt: now}
	var sessionID string
	txErr := db.Transaction(func(tx *gorm.DB) error {
		if err := repository.CreateAdmin(tx, admin); err != nil {
			return err
		}
		var sessErr error
		sessionID, sessErr = createSession(tx, admin.ID, now)
		return sessErr
	})
	if txErr != nil {
		// A concurrent request may have already won the singleton_guard
		// race — if an admin now exists, report the same
		// AccountSetupAlreadyDone a sequential retry would see, rather
		// than leaking the raw constraint-violation error.
		if recount, recountErr := repository.CountAdmins(db); recountErr == nil && recount > 0 {
			return nil, "", errcode.ErrAccountSetupAlreadyDone
		}
		return nil, "", txErr
	}
	return admin, sessionID, nil
}

// dummyPasswordHashForTiming is a fixed, valid bcrypt hash (of an arbitrary
// password, unrelated to any real account) used only to burn a comparable
// amount of CPU time as a real CheckPassword call when the username
// doesn't exist — bcrypt is deliberately slow, so skipping it entirely for
// unknown usernames would make "wrong password" and "no such account"
// distinguishable by response time alone, even though both already return
// the identical errcode.ErrAccountInvalidCredentials (PRD §6.1.3: never
// reveal whether an account exists). This does not eliminate every timing
// signal (DB query cost, GC, network jitter still vary), just the
// dominant one.
const dummyPasswordHashForTiming = "$2a$10$vpIoHknMZAeHODNlCkCaIOQl4f3oxTgUd1mR3rKuDld2LOwsXakbu"

// Login verifies username+password, applies the lockout state machine
// (design doc §4), and on success issues a new session. A wrong password
// and an unknown username return the exact same
// errcode.ErrAccountInvalidCredentials — PRD §6.1.3: never reveal whether
// an account exists.
func Login(db *gorm.DB, username, password string, now time.Time) (*model.Admin, string, error) {
	admin, err := repository.FindAdminByUsername(db, username)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		CheckPassword(dummyPasswordHashForTiming, password)
		return nil, "", errcode.ErrAccountInvalidCredentials
	}
	if err != nil {
		return nil, "", err
	}

	if admin.LockedUntil != nil && admin.LockedUntil.After(now) {
		return nil, "", &LockedError{LockedUntil: *admin.LockedUntil}
	}

	if !CheckPassword(admin.PasswordHash, password) {
		newLockedUntil, err := repository.RecordLoginFailure(db, admin.ID, now, LoginLockThreshold, LoginLockDuration)
		if err != nil {
			return nil, "", err
		}
		if newLockedUntil != nil && newLockedUntil.After(now) {
			return nil, "", &LockedError{LockedUntil: *newLockedUntil}
		}
		return nil, "", errcode.ErrAccountInvalidCredentials
	}

	// RecordLoginSuccess (clearing the lockout state) and createSession
	// (issuing the new session) run in one transaction — same reasoning as
	// Setup/ChangePassword: a partial failure here must not leave the
	// lockout cleared with no session actually issued. DeleteExpiredSessions
	// piggybacks on the same transaction, amortizing admin_sessions's
	// unbounded-growth cleanup across every successful login instead of
	// needing a separate cleanup worker/cron.
	var sessionID string
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := repository.DeleteExpiredSessions(tx, now); err != nil {
			return err
		}
		if err := repository.RecordLoginSuccess(tx, admin.ID, now); err != nil {
			return err
		}
		var sessErr error
		sessionID, sessErr = createSession(tx, admin.ID, now)
		return sessErr
	}); err != nil {
		return nil, "", err
	}
	return admin, sessionID, nil
}

// Logout deletes a single session. Deleting an id that's already gone is
// not an error — logout must be idempotent.
func Logout(db *gorm.DB, sessionID string) error {
	return repository.DeleteSession(db, sessionID)
}

// Me returns the admin identified by id (set into gin.Context by
// RequireAdminSession).
func Me(db *gorm.DB, adminID uint) (*model.Admin, error) {
	return repository.FindAdminByID(db, adminID)
}

// ChangePassword verifies the caller's current password, stores the new
// hash, and deletes every session belonging to this admin — including the
// caller's own — per PRD AUTH-07: changing the password invalidates the
// current login and returns to the login page.
//
// The password update and session deletion run inside one transaction: if
// the caller ended up "changed but still logged in on the old sessions"
// because the second write failed after the first committed, that would
// directly violate AUTH-07 — a partial failure here must undo the password
// change too, not leave it half-applied.
func ChangePassword(db *gorm.DB, adminID uint, currentPassword, newPassword string, now time.Time) error {
	admin, err := repository.FindAdminByID(db, adminID)
	if err != nil {
		return err
	}
	if !CheckPassword(admin.PasswordHash, currentPassword) {
		return errcode.ErrAccountInvalidCredentials
	}

	newHash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := repository.UpdateAdminPasswordHash(tx, adminID, newHash, now); err != nil {
			return err
		}
		return repository.DeleteAllSessionsForAdmin(tx, adminID)
	})
}

// createSession generates a fresh opaque token and persists it with a
// SessionTTL expiry. The raw token is the exact value stored as both
// admin_sessions.id (hashed) and the session cookie's value (design doc
// §4/§6).
func createSession(db *gorm.DB, adminID uint, now time.Time) (string, error) {
	sessionID, err := generateRandomToken(32, "")
	if err != nil {
		return "", err
	}
	if err := repository.CreateSession(db, sessionID, adminID, now.Add(SessionTTL), now); err != nil {
		return "", err
	}
	return sessionID, nil
}
