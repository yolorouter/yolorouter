// Package model additions for M2: Provider / ProviderKey / ProviderKeyFingerprint.
// Schema lives in migrations/{sqlite,postgres}/00004_create_providers.sql —
// goose owns DDL, GORM here is query-only (no AutoMigrate). See design doc
// .claude/docs/2026-07-18-m2-provider-design.md §3.
package model

import "time"

const (
	ProviderStatusEnabled  = 1
	ProviderStatusDisabled = 2
)

const (
	ProviderKeyStatusEnabled  = 1
	ProviderKeyStatusDisabled = 2
)

// VerificationStatus* answers "was this credential ever confirmed valid" —
// not "is the provider healthy right now" (design doc §3/§5's explicit M2/M6
// scope boundary).
const (
	VerificationStatusUntested = 0
	VerificationStatusPassed   = 1
	VerificationStatusFailed   = 2
)

// LastTestResult* mirror service.TestOutcome's int values 1:1 for storage in
// provider_keys.last_test_result (SMALLINT NULL — nil means "never tested").
// Kept in this package (not internal/service) so the model layer doesn't
// import service; internal/service/provider_client.go's TestOutcome
// constants are numerically identical by construction (both start at 0 and
// list the same 8 outcomes in the same order per design doc §5).
const (
	LastTestResultSuccess          = 0
	LastTestResultAuthFailed       = 1
	LastTestResultPermissionDenied = 2
	LastTestResultModelNotFound    = 3
	LastTestResultQuotaUnavailable = 4
	LastTestResultRateLimited      = 5
	LastTestResultUnreachable      = 6
	LastTestResultUpstreamError    = 7
)

// Provider is one upstream connection target (PRD §6.2). Deleting a
// provider is not supported by design (§1) — only management_status
// toggles it off.
type Provider struct {
	ID   uint   `gorm:"column:id;primaryKey" json:"id"`
	Name string `gorm:"column:name" json:"name"`
	// ProviderType is fixed to "openai" in v0.1 (design doc §3) — kept as a
	// column (not a hardcoded constant) so a later version can add more
	// provider types without a schema migration.
	ProviderType     string `gorm:"column:provider_type" json:"provider_type"`
	BaseURL          string `gorm:"column:base_url" json:"base_url"`
	Note             string `gorm:"column:note" json:"note"`
	ManagementStatus int    `gorm:"column:management_status" json:"management_status"`
	// DestinationVersion is json:"-": it's an internal CAS/versioning
	// primitive (design doc §3), never something the frontend reads or
	// writes directly.
	DestinationVersion int       `gorm:"column:destination_version" json:"-"`
	CreatedAt          time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt          time.Time `gorm:"column:updated_at" json:"-"`
}

func (Provider) TableName() string { return "providers" }

// ProviderKey is one upstream API key belonging to a Provider's key pool
// (design doc §3). EncryptedKey is never serialized; KeyPrefix is the only
// display-safe representation of the plaintext.
type ProviderKey struct {
	ID           uint   `gorm:"column:id;primaryKey" json:"id"`
	ProviderID   uint   `gorm:"column:provider_id" json:"provider_id"`
	Label        string `gorm:"column:label" json:"label"`
	EncryptedKey string `gorm:"column:encrypted_key" json:"-"`
	KeyPrefix    string `gorm:"column:key_prefix" json:"key_prefix"`
	SortOrder    int    `gorm:"column:sort_order" json:"sort_order"`
	// TestModel is the model name every test call for this key uses
	// (design doc addendum: M2 has no real model mapping yet, so the admin
	// supplies a temporary test model per key — PRD §6.2.8).
	TestModel string `gorm:"column:test_model" json:"test_model"`

	ManagementStatus   int `gorm:"column:management_status" json:"management_status"`
	VerificationStatus int `gorm:"column:verification_status" json:"verification_status"`
	// AuthorizedDestinationVersion is json:"-": an internal CAS field the
	// frontend never reads directly — "needs re-entry" is derived from it
	// server-side and exposed as a separate boolean in API responses (see
	// handler DTOs in Task 9), not this raw version number.
	AuthorizedDestinationVersion int `gorm:"column:authorized_destination_version" json:"-"`

	LastTestResult     *int       `gorm:"column:last_test_result" json:"last_test_result"`
	LastTestModel      string     `gorm:"column:last_test_model" json:"last_test_model"`
	LastTestDurationMs *int64     `gorm:"column:last_test_duration_ms" json:"last_test_duration_ms"`
	LastTestedAt       *time.Time `gorm:"column:last_tested_at" json:"last_tested_at"`

	ConfigVersion  int `gorm:"column:config_version" json:"-"`
	TestGeneration int `gorm:"column:test_generation" json:"-"`

	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"-"`
}

func (ProviderKey) TableName() string { return "provider_keys" }

// ProviderKeyFingerprint is the single-row table used to detect a
// master-key/database mismatch at startup (design doc §5).
type ProviderKeyFingerprint struct {
	ID             uint      `gorm:"column:id;primaryKey" json:"-"`
	EncryptedProbe string    `gorm:"column:encrypted_probe" json:"-"`
	CreatedAt      time.Time `gorm:"column:created_at" json:"-"`
}

func (ProviderKeyFingerprint) TableName() string { return "provider_key_fingerprint" }
