// Package model defines Model / ModelCandidate.
// Schema lives in migrations/{sqlite,postgres}/00005_create_models.sql —
// goose owns DDL, GORM here is query-only (no AutoMigrate).
package model

import "time"

const (
	ModelStatusEnabled  = 1
	ModelStatusDisabled = 2
)

const (
	ModelCandidateStatusEnabled  = 1
	ModelCandidateStatusDisabled = 2
)

// ModelVerificationStatus* answers "has this candidate's basic-text mapping
// ever been confirmed" — streaming/function-calling are separate bool flags
// on ModelCandidate, not folded into this (a candidate can
// pass basic-text and never have run a streaming test at all, which is not
// the same thing as having failed one).
const (
	ModelVerificationStatusUntested = 0
	ModelVerificationStatusPassed   = 1
	ModelVerificationStatusFailed   = 2
)

// model_candidates.last_test_result stores service.TestOutcome's int values
// verbatim (SMALLINT NULL — nil means "never tested") — see provider.go's
// LastTestResult* constants, which already cover this exact value set and
// are reused here rather than duplicated.

// Model is one externally-exposed model name. No delete —
// only management_status toggles it off.
type Model struct {
	ID               uint      `gorm:"column:id;primaryKey" json:"id"`
	Name             string    `gorm:"column:name" json:"name"`
	ManagementStatus int       `gorm:"column:management_status" json:"management_status"`
	CreatedAt        time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (Model) TableName() string { return "models" }

// ModelCandidate is one provider's offering of a Model — the external name
// resolves to this candidate's ProviderModelName when routed.
type ModelCandidate struct {
	ID                      uint       `gorm:"column:id;primaryKey" json:"id"`
	ModelID                 uint       `gorm:"column:model_id" json:"model_id"`
	ProviderID              uint       `gorm:"column:provider_id" json:"provider_id"`
	ProviderModelName       string     `gorm:"column:provider_model_name" json:"provider_model_name"`
	InputPrice              float64    `gorm:"column:input_price" json:"input_price"`
	OutputPrice             float64    `gorm:"column:output_price" json:"output_price"`
	CacheWritePrice         *float64   `gorm:"column:cache_write_price" json:"cache_write_price"`
	CacheReadPrice          *float64   `gorm:"column:cache_read_price" json:"cache_read_price"`
	MaxOutput               int        `gorm:"column:max_output" json:"max_output"`
	SupportsStreaming       bool       `gorm:"column:supports_streaming" json:"supports_streaming"`
	SupportsFunctionCalling bool       `gorm:"column:supports_function_calling" json:"supports_function_calling"`
	ManagementStatus        int        `gorm:"column:management_status" json:"management_status"`
	SortOrder               int        `gorm:"column:sort_order" json:"sort_order"`
	VerificationStatus      int        `gorm:"column:verification_status" json:"verification_status"`
	LastTestResult          *int       `gorm:"column:last_test_result" json:"last_test_result"`
	LastTestDurationMs      *int64     `gorm:"column:last_test_duration_ms" json:"last_test_duration_ms"`
	LastTestedAt            *time.Time `gorm:"column:last_tested_at" json:"last_tested_at"`
	CreatedAt               time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt               time.Time  `gorm:"column:updated_at" json:"updated_at"`

	// Provider is populated via an explicit preload in repository queries
	// that need it (e.g. listing candidates with provider name/status) —
	// never relied upon to be populated by default (same convention as
	// ProviderKey.Provider, which doesn't exist there because the provider
	// layer never needed the reverse direction).
	Provider *Provider `gorm:"foreignKey:ProviderID" json:"provider,omitempty"`
}

func (ModelCandidate) TableName() string { return "model_candidates" }
