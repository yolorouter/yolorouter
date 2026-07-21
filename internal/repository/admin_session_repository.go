package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// hashSessionToken delegates to crypto.HashToken — the single SHA-256 hex
// recipe shared with the gateway's bearer-key lookup (middleware) and the
// API-key hash (service). admin_sessions.id stores only this digest, never
// the raw token: the raw token is what's sent to the browser as the cookie
// value (design doc §4/§6), so a leaked database file or backup (see
// pkg/database's db:backup) would otherwise hand out directly-replayable,
// still-valid admin sessions for up to SessionTTL. Reversing a SHA-256
// digest back to the original token is infeasible, so a leaked row alone
// isn't enough to impersonate an admin — the same reasoning bcrypt-hashing
// the password already applies to credentials applies here to the session
// token too.
func hashSessionToken(token string) string {
	return crypto.HashToken(token)
}

// CreateSession inserts a new session row for rawToken — the
// caller-generated opaque token (internal/service never generates IDs
// itself here) that becomes the cookie value. This is the only function
// that ever constructs a model.AdminSession: callers never see or set
// TokenHash directly, so there's exactly one place the raw-token-to-hash
// transform happens, and no struct field that means different things
// depending on which direction the data is flowing.
func CreateSession(db *gorm.DB, rawToken string, adminID uint, expiresAt, createdAt time.Time) error {
	session := &model.AdminSession{
		TokenHash: hashSessionToken(rawToken),
		AdminID:   adminID,
		ExpiresAt: expiresAt,
		CreatedAt: createdAt,
	}
	return db.Create(session).Error
}

// FindValidSessionByID takes the raw token (as read from the cookie),
// hashes it, and looks up the matching row. Returns gorm.ErrRecordNotFound
// both when no row matches AND when it exists but has already expired —
// callers (i.e. RequireAdminSession) must not distinguish the two, so an
// expired session behaves identically to one that was never issued.
func FindValidSessionByID(db *gorm.DB, id string, now time.Time) (*model.AdminSession, error) {
	var session model.AdminSession
	if err := db.Where("id = ? AND expires_at > ?", hashSessionToken(id), now).First(&session).Error; err != nil {
		return nil, err
	}
	return &session, nil
}

// DeleteSession removes a single session row (logout), identified by its
// raw token. Deleting an already-gone id is not an error — logout must be
// idempotent.
func DeleteSession(db *gorm.DB, id string) error {
	return db.Where("id = ?", hashSessionToken(id)).Delete(&model.AdminSession{}).Error
}

// DeleteAllSessionsForAdmin removes every session belonging to an admin —
// used by ChangePassword to force every existing login (including the
// caller's own) to re-authenticate, per PRD AUTH-07.
func DeleteAllSessionsForAdmin(db *gorm.DB, adminID uint) error {
	return db.Where("admin_id = ?", adminID).Delete(&model.AdminSession{}).Error
}

// DeleteExpiredSessions removes every admin_sessions row whose expiry has
// already passed. No path ever deletes a session once its TTL elapses on
// its own (only an explicit Logout or ChangePassword deletes a row) — an
// admin who simply lets the 24h TTL lapse without logging out (the common
// case) would otherwise leave that row behind forever, one dead row per
// login with no ceiling. This mirrors the identical fix already applied
// to .claude/reference-projects/yolorouter-deprecated's `sessions`
// table (DeleteExpiredOrRevokedSessionsInTx, closing its own codex
// adversarial review round 7) — called from inside Login's existing
// transaction (service.Login) so the cleanup cost is amortized across
// normal logins rather than needing a separate cleanup worker/cron.
func DeleteExpiredSessions(db *gorm.DB, now time.Time) error {
	return db.Where("expires_at <= ?", now).Delete(&model.AdminSession{}).Error
}
