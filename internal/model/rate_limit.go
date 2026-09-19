package model

import "time"

// ObservedRateLimit is one learned fact about what an upstream said its
// own rate limit is, keyed by (provider key, meter). A key equals one
// upstream account, and upstreams enforce limits per account, so the row
// belongs to the key — never the provider, whose other keys may hold
// accounts with entirely different ceilings.
//
// The row is evidence, not configuration: LimitValue only ever moves down
// (one malformed response must not raise the remembered ceiling); the
// gauge columns (remaining, reset) are plain freshest-wins snapshots.
// Deleting the row is the reset — the next 429 teaches it again.
type ObservedRateLimit struct {
	ID            uint       `gorm:"column:id;primaryKey" json:"id"`
	ProviderKeyID uint       `gorm:"column:provider_key_id" json:"provider_key_id"`
	Meter         string     `gorm:"column:meter" json:"meter"`
	LimitValue    *int64     `gorm:"column:limit_value" json:"limit_value"`
	WindowSeconds *int64     `gorm:"column:window_seconds" json:"window_seconds"`
	LastRemaining *int64     `gorm:"column:last_remaining" json:"last_remaining"`
	LastResetAt   *time.Time `gorm:"column:last_reset_at" json:"last_reset_at"`
	// ObservedAt is when the evidence was seen; UpdatedAt is the row's
	// last write. They match today (every write carries fresh evidence)
	// and are kept separate so a future writer that refreshes gauges
	// without re-observing a limit cannot lie about when the limit was
	// actually learned.
	ObservedAt time.Time `gorm:"column:observed_at" json:"observed_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (ObservedRateLimit) TableName() string { return "observed_rate_limits" }

// Observed rate limit meters. Requests counts calls; tokens counts
// prompt+completion tokens. Kept as plain strings (not a typed enum) so
// the untyped SQL upsert and the JSON API surface stay in lockstep
// without a mapping layer.
const (
	MeterRequests = "requests"
	MeterTokens   = "tokens"
)
