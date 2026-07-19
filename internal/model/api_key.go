// Package model additions for M4: APIKey / APIKeyModel.
// Schema lives in migrations/{sqlite,postgres}/00006_create_api_keys.sql —
// goose owns DDL, GORM here is query-only (no AutoMigrate). See design doc
// .claude/docs/2026-07-19-m4-apikey-design.md §3.
package model

import "time"

const (
	APIKeyStatusActive  = 1
	APIKeyStatusRevoked = 2
)

// APIKey is one Yolorouter calling credential (PRD §6.4). The plaintext key
// is shown only at create time and never persisted; the row stores just
// KeyHash (SHA-256) + KeyPrefix (first chars, list-distinguishing only — not
// enough to reconstruct the full key). Limits are pointer-typed so NULL means
// "no cap". Budget is integer cents (int64) to avoid float drift on a
// cumulative hard cap. KeyHash and RevokedAt are json:"-": the hash must
// never serialize back out, and revoked state surfaces via display_status
// rather than a raw timestamp.
type APIKey struct {
	ID               uint       `gorm:"column:id;primaryKey" json:"id"`
	KeyHash          string     `gorm:"column:key_hash" json:"-"`
	KeyPrefix        string     `gorm:"column:key_prefix" json:"key_prefix"`
	OwnerLabel       string     `gorm:"column:owner_label" json:"owner_label"`
	Remark           string     `gorm:"column:remark" json:"remark"`
	Status           int        `gorm:"column:status" json:"status"`
	ExpiresAt        *time.Time `gorm:"column:expires_at" json:"expires_at"`
	RPMLimit         *int       `gorm:"column:rpm_limit" json:"rpm_limit"`
	TPMLimit         *int       `gorm:"column:tpm_limit" json:"tpm_limit"`
	ConcurrencyLimit *int       `gorm:"column:concurrency_limit" json:"concurrency_limit"`
	BudgetLimitCents *int64     `gorm:"column:budget_limit_cents" json:"budget_limit_cents"`
	BudgetSpentCents int64      `gorm:"column:budget_spent_cents" json:"budget_spent_cents"`
	CreatedAt        time.Time  `gorm:"column:created_at" json:"created_at"`
	RevokedAt        *time.Time `gorm:"column:revoked_at" json:"-"`
	UpdatedAt        time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

func (APIKey) TableName() string { return "api_keys" }

// APIKeyModel is one row of a key's model allowlist (PRD §6.4.7). Stored by
// model_id rather than model name, so renaming a model doesn't break the
// whitelists of keys that reference it; a stopped model stays in the list and
// is rejected at route time (handled by the gateway module, not M4).
type APIKeyModel struct {
	ID        uint      `gorm:"column:id;primaryKey" json:"id"`
	APIKeyID  uint      `gorm:"column:api_key_id" json:"api_key_id"`
	ModelID   uint      `gorm:"column:model_id" json:"model_id"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

func (APIKeyModel) TableName() string { return "api_key_models" }
