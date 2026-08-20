// Package model defines Model / ModelCandidate.
// Schema lives in migrations/{sqlite,postgres}/00005_create_models.sql —
// goose owns DDL, GORM here is query-only (no AutoMigrate).
package model

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	ModelStatusEnabled  = 1
	ModelStatusDisabled = 2
)

const (
	ModelCandidateStatusEnabled  = 1
	ModelCandidateStatusDisabled = 2
)

// ModelVerificationStatus* answers "has this candidate's basic-text mapping ever
// been confirmed". Unlike the capability flags on ModelCandidate, which are
// informational, this one gates routing: the gateway will not send traffic to a
// candidate that is not Passed. Streaming / function-calling stay separate
// because passing basic text says nothing about whether either was ever probed.
const (
	ModelVerificationStatusUntested = 0
	ModelVerificationStatusPassed   = 1
	ModelVerificationStatusFailed   = 2
)

// model_candidates.last_test_result stores providerclient.TestOutcome's int values
// verbatim (SMALLINT NULL — nil means "never tested") — see provider.go's
// LastTestResult* constants, which already cover this exact value set and
// are reused here rather than duplicated.

// Model is one externally-exposed model name. No delete —
// only management_status toggles it off.
type Model struct {
	ID               uint   `gorm:"column:id;primaryKey" json:"id"`
	Name             string `gorm:"column:name" json:"name"`
	ManagementStatus int    `gorm:"column:management_status" json:"management_status"`
	// SupportsImageInput is the admin's declaration of whether this model can
	// read images. Tri-state: nil = undeclared, and the gateway must not
	// touch images at all (neither describe nor strip) — a missing
	// declaration must never degrade a vision-capable model.
	SupportsImageInput *bool     `gorm:"column:supports_image_input" json:"supports_image_input"`
	CreatedAt          time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt          time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (Model) TableName() string { return "models" }

// ModelCandidate is one provider's offering of a Model — the external name
// resolves to this candidate's ProviderModelName when routed.
type ModelCandidate struct {
	ID                uint     `gorm:"column:id;primaryKey" json:"id"`
	ModelID           uint     `gorm:"column:model_id" json:"model_id"`
	ProviderID        uint     `gorm:"column:provider_id" json:"provider_id"`
	ProviderModelName string   `gorm:"column:provider_model_name" json:"provider_model_name"`
	InputPrice        float64  `gorm:"column:input_price" json:"input_price"`
	OutputPrice       float64  `gorm:"column:output_price" json:"output_price"`
	CacheWritePrice   *float64 `gorm:"column:cache_write_price" json:"cache_write_price"`
	CacheReadPrice    *float64 `gorm:"column:cache_read_price" json:"cache_read_price"`
	MaxOutput         int      `gorm:"column:max_output" json:"max_output"`
	// SupportsStreaming / SupportsFunctionCalling record whether the last probe
	// CONFIRMED the capability: true when it did, nil when it did not. They are
	// informational — the admin UI shows them and routing ignores them entirely
	// (see filterCandidates), so an unconfirmed capability costs a missing tick
	// and nothing else.
	//
	// Nullable rather than a plain bool so "never probed / could not confirm" is
	// distinguishable from a false. Nothing writes false today; the column can
	// still hold one written by an older build.
	SupportsStreaming       *bool      `gorm:"column:supports_streaming" json:"supports_streaming"`
	SupportsFunctionCalling *bool      `gorm:"column:supports_function_calling" json:"supports_function_calling"`
	ManagementStatus        int        `gorm:"column:management_status" json:"management_status"`
	SortOrder               int        `gorm:"column:sort_order" json:"sort_order"`
	VerificationStatus      int        `gorm:"column:verification_status" json:"verification_status"`
	LastTestResult          *int       `gorm:"column:last_test_result" json:"last_test_result"`
	LastTestDurationMs      *int64     `gorm:"column:last_test_duration_ms" json:"last_test_duration_ms"`
	LastTestedAt            *time.Time `gorm:"column:last_tested_at" json:"last_tested_at"`
	CreatedAt               time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt               time.Time  `gorm:"column:updated_at" json:"updated_at"`
	// PriceUpdatedAt is when one of the four price columns was last written.
	// UpdatedAt cannot answer that: enabling, disabling, retesting and probing
	// all bump it without touching a price, so ordering price history by it
	// would let an untouched stale rate overtake a newer one.
	PriceUpdatedAt time.Time `gorm:"column:price_updated_at" json:"price_updated_at"`
	// ProviderModelNameFolded is ProviderModelName lowercased, kept in the row so
	// the price look-up can match names case-insensitively through a plain
	// indexed equality. It exists because SQL LOWER() is not the same function
	// on SQLite and Postgres, so folding in the predicate would make the same
	// data match on one backend and miss on the other. Written by BeforeCreate
	// and by repository.UpdateModelCandidate, which are the only two places
	// ProviderModelName itself is written; nothing else may set one without the
	// other. Not serialized — it is a storage detail, not part of the API.
	ProviderModelNameFolded string `gorm:"column:provider_model_name_folded" json:"-"`

	// BeforeCreate guarantees PriceUpdatedAt is never stored as the zero time.
	// GORM enumerates every mapped column on insert, so a caller that builds a
	// ModelCandidate without setting it does not omit the column — it writes
	// 0001-01-01, which both backends accept and which loses every price-recency
	// comparison forever, silently. Falling back to CreatedAt (or now) puts the
	// guarantee in the one place every insert passes through, rather than
	// depending on each construction site remembering.

	// Provider is populated via an explicit preload in repository queries
	// that need it (e.g. listing candidates with provider name/status) —
	// never relied upon to be populated by default (same convention as
	// ProviderKey.Provider, which doesn't exist there because the provider
	// layer never needed the reverse direction).
	Provider *Provider `gorm:"foreignKey:ProviderID" json:"provider,omitempty"`
}

func (ModelCandidate) TableName() string { return "model_candidates" }

// BeforeCreate stamps the price clock when the caller left it unset. See the
// note on ModelCandidate.PriceUpdatedAt: an unset value is written as year 0001
// rather than being omitted, so it has to be filled in before the insert.
func (c *ModelCandidate) BeforeCreate(*gorm.DB) error {
	if c.PriceUpdatedAt.IsZero() {
		if !c.CreatedAt.IsZero() {
			c.PriceUpdatedAt = c.CreatedAt
		} else {
			c.PriceUpdatedAt = time.Now().UTC()
		}
	}
	// Derived, never supplied by callers: keeping it here means no construction
	// site can insert a row the price look-up cannot see.
	c.ProviderModelNameFolded = FoldModelName(c.ProviderModelName)
	return nil
}

// FoldModelName is the one definition of "the same upstream model name ignoring
// case", shared by what is stored and what is queried. Both sides must call it
// or a row becomes unfindable.
func FoldModelName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
