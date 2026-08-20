package auth

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

const (
	// LoginLockThreshold and LoginLockDuration implement the tentative
	// "lock for 15 minutes after 5 consecutive failures" rule.
	LoginLockThreshold = 5
	LoginLockDuration  = 15 * time.Minute
	// SessionTTL is the tentative 24-hour session TTL.
	SessionTTL = 24 * time.Hour
)

// LockedError carries the exact unlock time for an AccountLoginLocked
// response — a plain sentinel error can't carry this per-call value, and
// the frontend needs it to render a countdown.
type LockedError struct {
	LockedUntil time.Time
}

func (e *LockedError) Error() string { return errcode.ErrorMessages[errcode.AccountLoginLocked] }

// CheckState reports whether first-run setup has already been completed.
// "Completed" means the local password account exists — accounts
// provisioned through external identity providers don't count, because
// an instance without its local admin still needs the setup page.
func CheckState(db *gorm.DB) (bool, error) {
	count, err := repository.CountLocalUsers(db)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// Setup creates the local admin account and immediately signs
// them in — creation success lands on the empty-state
// overview. Returns the new user and a freshly issued session id.
//
// The CountLocalUsers check below is only a fast-path optimization (skip
// hashing/inserting when setup is obviously already done) — it does NOT by
// itself prevent two concurrent first-run requests from both observing
// count==0 and both attempting to insert. The real guarantee is the
// partial unique index on users.is_local (migration
// 00023_users_multi_account.sql): at most one of any concurrent
// CreateUser(is_local=true) calls can ever succeed, and user creation +
// session issuance run in one transaction so a losing/failed attempt
// never leaves a half-initialized state (a user row with no session, or
// vice versa).
func Setup(db *gorm.DB, username, password string, now time.Time) (*model.User, string, error) {
	count, err := repository.CountLocalUsers(db)
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

	user := &model.User{
		Username:     username,
		PasswordHash: hash,
		Role:         model.RoleAdmin,
		Status:       model.UserStatusEnabled,
		IsLocal:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	var sessionID string
	txErr := db.Transaction(func(tx *gorm.DB) error {
		if err := repository.CreateUser(tx, user); err != nil {
			return err
		}
		var sessErr error
		sessionID, sessErr = CreateSession(tx, user.ID, now)
		return sessErr
	})
	if txErr != nil {
		// A concurrent request may have already won the single-local-user
		// race — if a local account now exists, report the same
		// AccountSetupAlreadyDone a sequential retry would see, rather
		// than leaking the raw constraint-violation error.
		if recount, recountErr := repository.CountLocalUsers(db); recountErr == nil && recount > 0 {
			return nil, "", errcode.ErrAccountSetupAlreadyDone
		}
		return nil, "", txErr
	}
	return user, sessionID, nil
}

// dummyPasswordHashForTiming is a fixed, valid bcrypt hash (of an arbitrary
// password, unrelated to any real account) used only to burn a comparable
// amount of CPU time as a real CheckPassword call when the username
// doesn't exist — bcrypt is deliberately slow, so skipping it entirely for
// unknown usernames would make "wrong password" and "no such account"
// distinguishable by response time alone, even though both already return
// the identical errcode.ErrAccountInvalidCredentials (never
// reveal whether an account exists). This does not eliminate every timing
// signal (DB query cost, GC, network jitter still vary), just the
// dominant one.
const dummyPasswordHashForTiming = "$2a$10$vpIoHknMZAeHODNlCkCaIOQl4f3oxTgUd1mR3rKuDld2LOwsXakbu"

// Login verifies username+password against the local account, applies
// the lockout state machine, and on success issues a new session. A
// wrong password, an unknown username, and a non-local username all
// return the exact same errcode.ErrAccountInvalidCredentials — never
// reveal whether an account exists or how it authenticates.
//
// A disabled local account is rejected with an explicit
// account-disabled error only AFTER the password check succeeds:
// reporting it on username match alone would confirm the account exists
// to someone who doesn't hold the password.
func Login(db *gorm.DB, username, password string, now time.Time) (*model.User, string, error) {
	user, err := repository.FindLocalUserByUsername(db, username)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		CheckPassword(dummyPasswordHashForTiming, password)
		return nil, "", errcode.ErrAccountInvalidCredentials
	}
	if err != nil {
		return nil, "", err
	}

	if user.LockedUntil != nil && user.LockedUntil.After(now) {
		return nil, "", &LockedError{LockedUntil: *user.LockedUntil}
	}

	if !CheckPassword(user.PasswordHash, password) {
		newLockedUntil, err := repository.RecordLoginFailure(db, user.ID, now, LoginLockThreshold, LoginLockDuration)
		if err != nil {
			return nil, "", err
		}
		if newLockedUntil != nil && newLockedUntil.After(now) {
			return nil, "", &LockedError{LockedUntil: *newLockedUntil}
		}
		return nil, "", errcode.ErrAccountInvalidCredentials
	}

	if user.Status != model.UserStatusEnabled {
		return nil, "", errcode.ErrAccountDisabled
	}

	// RecordLoginSuccess (clearing the lockout state) and CreateSession
	// (issuing the new session) run in one transaction — same reasoning as
	// Setup/ChangePassword: a partial failure here must not leave the
	// lockout cleared with no session actually issued. DeleteExpiredSessions
	// piggybacks on the same transaction, amortizing user_sessions's
	// unbounded-growth cleanup across every successful login instead of
	// needing a separate cleanup worker/cron.
	var sessionID string
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := repository.DeleteExpiredSessions(tx, now); err != nil {
			return err
		}
		if err := repository.RecordLoginSuccess(tx, user.ID, now); err != nil {
			return err
		}
		var sessErr error
		sessionID, sessErr = CreateSession(tx, user.ID, now)
		return sessErr
	}); err != nil {
		return nil, "", err
	}
	return user, sessionID, nil
}

// Logout deletes a single session. Deleting an id that's already gone is
// not an error — logout must be idempotent.
func Logout(db *gorm.DB, sessionID string) error {
	return repository.DeleteSession(db, sessionID)
}

// Me returns the user identified by id (set into gin.Context by
// RequireSession).
func Me(db *gorm.DB, userID uint) (*model.User, error) {
	return repository.FindUserByID(db, userID)
}

// ChangePassword verifies the caller's current password, stores the new
// hash, and deletes every session belonging to this user — including the
// caller's own — changing the password invalidates the
// current login and returns to the login page.
//
// Only the local account can ever pass the current-password check:
// externally-provisioned accounts have an empty stored hash, which
// CheckPassword rejects for any input, so they fall out as
// invalid-credentials without needing an explicit is_local branch.
//
// The password update and session deletion run inside one transaction: if
// the caller ended up "changed but still logged in on the old sessions"
// because the second write failed after the first committed, that would
// directly violate that invariant — a partial failure here must undo the
// password change too, not leave it half-applied.
func ChangePassword(db *gorm.DB, userID uint, currentPassword, newPassword string, now time.Time) error {
	user, err := repository.FindUserByID(db, userID)
	if err != nil {
		return err
	}
	if !CheckPassword(user.PasswordHash, currentPassword) {
		return errcode.ErrAccountInvalidCredentials
	}

	newHash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := repository.UpdateUserPasswordHash(tx, userID, newHash, now); err != nil {
			return err
		}
		return repository.DeleteAllSessionsForUser(tx, userID)
	})
}

// CreateSession generates a fresh opaque token and persists it with a
// SessionTTL expiry. The raw token is the exact value stored as both
// user_sessions.id (hashed) and the session cookie's value. This is the one
// session-minting recipe: password login and the OAuth callback both issue
// their sessions here, so the two flows stay indistinguishable.
func CreateSession(db *gorm.DB, userID uint, now time.Time) (string, error) {
	sessionID, err := crypto.GenerateRandomToken(32, "")
	if err != nil {
		return "", err
	}
	if err := repository.CreateSession(db, sessionID, userID, now.Add(SessionTTL), now); err != nil {
		return "", err
	}
	return sessionID, nil
}
