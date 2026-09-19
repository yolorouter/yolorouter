// Package repository — observed rate limits: pure data access for the
// learned 429 evidence rows. The only-down rule for limit_value lives in
// the upsert itself (a SQL CASE inside ON CONFLICT), not in callers, so
// concurrent 429s landing out of order cannot end up with the larger of
// two ceilings — the database resolves the race, whoever commits second.
package repository

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/yolorouter/yolorouter/internal/model"
)

// ObservedRateLimitView is the admin list row: the learned fact plus the
// provider/key naming a join supplies. Names are snapshotted at read time
// rather than stored — renaming a provider or key label must not leave
// the evidence rows pointing at stale strings.
type ObservedRateLimitView struct {
	ID            uint       `json:"id"`
	ProviderID    uint       `json:"provider_id"`
	ProviderName  string     `json:"provider_name"`
	ProviderKeyID uint       `json:"provider_key_id"`
	KeyLabel      string     `json:"key_label"`
	Meter         string     `json:"meter"`
	LimitValue    *int64     `json:"limit_value"`
	WindowSeconds *int64     `json:"window_seconds"`
	LastRemaining *int64     `json:"last_remaining"`
	LastResetAt   *time.Time `json:"last_reset_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// UpsertObservedRateLimit merges one meter's fresh 429 evidence into its
// row. Only limit_value is guarded (only-down); the gauge fields
// (remaining, reset) and window_seconds are freshest-wins — a later 429
// that names a window is newer evidence about which quota tripped, and
// stale NULLs must not survive an answer. observedAt stamps when the
// evidence was seen (used for both observed_at and updated_at: every
// write here carries fresh evidence).
func UpsertObservedRateLimit(db *gorm.DB, keyID uint, meter string, limitValue, windowSeconds, remaining *int64, resetAt *time.Time, observedAt time.Time) error {
	row := model.ObservedRateLimit{
		ProviderKeyID: keyID,
		Meter:         meter,
		LimitValue:    limitValue,
		WindowSeconds: windowSeconds,
		LastRemaining: remaining,
		LastResetAt:   resetAt,
		ObservedAt:    observedAt,
		UpdatedAt:     observedAt,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "provider_key_id"}, {Name: "meter"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			// NULL means "never learned"; any real number replaces it,
			// and afterwards only a strictly smaller number does.
			"limit_value": gorm.Expr(
				`CASE WHEN excluded.limit_value IS NOT NULL AND (` +
					`observed_rate_limits.limit_value IS NULL OR ` +
					`excluded.limit_value < observed_rate_limits.limit_value) ` +
					`THEN excluded.limit_value ELSE observed_rate_limits.limit_value END`),
			"window_seconds": gorm.Expr(`COALESCE(excluded.window_seconds, observed_rate_limits.window_seconds)`),
			"last_remaining": gorm.Expr(`COALESCE(excluded.last_remaining, observed_rate_limits.last_remaining)`),
			"last_reset_at":  gorm.Expr(`COALESCE(excluded.last_reset_at, observed_rate_limits.last_reset_at)`),
			"observed_at":    gorm.Expr(`excluded.observed_at`),
			"updated_at":     gorm.Expr(`excluded.updated_at`),
		}),
	}).Create(&row).Error
}

// ListObservedRateLimits returns every learned row with provider and key
// names attached, newest update first. Deliberately unpaginated: the row
// count is bounded by (keys × 2 meters), which is tens of rows for any
// realistic deployment. The slice is non-nil even when empty so the admin
// envelope carries [] rather than null — same contract as the provider
// list.
func ListObservedRateLimits(db *gorm.DB) ([]ObservedRateLimitView, error) {
	rows := []ObservedRateLimitView{}
	err := db.Table("observed_rate_limits o").
		Select(`o.id, k.provider_id, p.name AS provider_name, o.provider_key_id, ` +
			`k.label AS key_label, o.meter, o.limit_value, o.window_seconds, ` +
			`o.last_remaining, o.last_reset_at, o.updated_at`).
		Joins("LEFT JOIN provider_keys k ON k.id = o.provider_key_id").
		Joins("LEFT JOIN providers p ON p.id = k.provider_id").
		Order("o.updated_at DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// DeleteObservedRateLimit removes one learned row — the admin-facing
// reset. Idempotent: a row already gone is the requested state.
func DeleteObservedRateLimit(db *gorm.DB, id uint) error {
	return db.Where("id = ?", id).Delete(&model.ObservedRateLimit{}).Error
}
