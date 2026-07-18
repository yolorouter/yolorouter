package model

import "time"

// AdminSession is an opaque server-side session. TokenHash is the SHA-256
// digest of the raw session token (internal/repository's
// hashSessionToken) — the raw token is only ever the session cookie's
// value (design doc §4/§6); it is never constructed as, or read back out
// of, this struct. Deliberately not named ID/matching the cookie value
// directly: a field that means "raw token" when building a new session
// but "its hash" when read back from the database is exactly the kind of
// footgun that gets a stale value double-hashed into a silent no-op
// delete. TokenHash is explicitly json:"-" as defense in depth even though
// it's already one-way hashed, not the live credential itself.
type AdminSession struct {
	TokenHash string    `gorm:"column:id;primaryKey" json:"-"`
	AdminID   uint      `gorm:"column:admin_id" json:"admin_id"`
	ExpiresAt time.Time `gorm:"column:expires_at" json:"expires_at"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

func (AdminSession) TableName() string { return "admin_sessions" }
