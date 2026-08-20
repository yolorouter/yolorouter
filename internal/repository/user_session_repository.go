package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// hashSessionToken delegates to crypto.HashToken — the single SHA-256 hex
// recipe shared with the gateway's bearer-key lookup (middleware) and the
// API-key hash (service). user_sessions.id stores only this digest, never
// the raw token: the raw token is what's sent to the browser as the cookie
// value, so a leaked database file or backup (see
// pkg/database's db:backup) would otherwise hand out directly-replayable,
// still-valid sessions for up to SessionTTL. Reversing a SHA-256
// digest back to the original token is infeasible, so a leaked row alone
// isn't enough to impersonate a user — the same reasoning bcrypt-hashing
// the password already applies to credentials applies here to the session
// token too.
func hashSessionToken(token string) string {
	return crypto.HashToken(token)
}

// CreateSession inserts a new session row for rawToken — the
// caller-generated opaque token (the auth service never generates IDs
// itself here) that becomes the cookie value. This is the only function
// that ever constructs a user_sessions row: callers never see or set the
// token hash directly, so there's exactly one place the
// raw-token-to-hash transform happens.
//
// The insert is conditional on the owner still being an ENABLED account,
// checked in the same statement: a login flow races an admin's disable
// (the login read the user as enabled, the disable then committed and
// deleted every session), and an unconditional insert here would mint a
// session AFTER that deletion — dead while disabled, but silently valid
// again once the account is re-enabled. RowsAffected 0 means the guard
// failed; surfaced as ErrAccountDisabled so login paths report it
// exactly like a disable observed up front.
func CreateSession(db *gorm.DB, rawToken string, userID uint, expiresAt, createdAt time.Time) error {
	res := db.Exec(
		`INSERT INTO user_sessions (id, user_id, expires_at, created_at)
		 SELECT ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM users WHERE id = ? AND status = ?)`,
		hashSessionToken(rawToken), userID, expiresAt, createdAt,
		userID, model.UserStatusEnabled,
	)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errcode.ErrAccountDisabled
	}
	return nil
}

// FindUserByValidSession takes the raw token (as read from the cookie),
// hashes it, and returns the owning user of the matching unexpired
// session. Returns gorm.ErrRecordNotFound both when no row matches AND
// when it exists but has already expired — callers (i.e.
// RequireSession) must not distinguish the two, so an expired session
// behaves identically to one that was never issued.
//
// The session→user resolution is one JOIN rather than two lookups so
// every authenticated request costs a single query, and so there is no
// window where a just-deleted user's session resolves to a dangling id.
// The user's status is returned as-is, NOT filtered here: the middleware
// decides how a disabled account answers (403 with an explicit
// account-disabled code), which a WHERE clause would collapse into the
// generic invalid-session 401.
func FindUserByValidSession(db *gorm.DB, rawToken string, now time.Time) (*model.User, error) {
	var user model.User
	err := db.Model(&model.User{}).
		Joins("JOIN user_sessions ON user_sessions.user_id = users.id").
		Where("user_sessions.id = ? AND user_sessions.expires_at > ?", hashSessionToken(rawToken), now).
		First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// DeleteSession removes a single session row (logout), identified by its
// raw token. Deleting an already-gone id is not an error — logout must be
// idempotent.
func DeleteSession(db *gorm.DB, rawToken string) error {
	return db.Where("id = ?", hashSessionToken(rawToken)).Delete(&model.UserSession{}).Error
}

// DeleteAllSessionsForUser removes every session belonging to a user —
// used by ChangePassword to force every existing login (including the
// caller's own) to re-authenticate.
func DeleteAllSessionsForUser(db *gorm.DB, userID uint) error {
	return db.Where("user_id = ?", userID).Delete(&model.UserSession{}).Error
}

// DeleteExpiredSessions removes every user_sessions row whose expiry has
// already passed. No path ever deletes a session once its TTL elapses on
// its own (only an explicit Logout or ChangePassword deletes a row) — a
// user who simply lets the 24h TTL lapse without logging out (the common
// case) would otherwise leave that row behind forever, one dead row per
// login with no ceiling. This mirrors an established cleanup pattern:
// called from inside Login's existing transaction (auth.Login) so the
// cleanup cost is amortized across normal logins rather than needing a
// separate cleanup worker/cron.
func DeleteExpiredSessions(db *gorm.DB, now time.Time) error {
	return db.Where("expires_at <= ?", now).Delete(&model.UserSession{}).Error
}
