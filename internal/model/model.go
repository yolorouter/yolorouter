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
	// SchedulingMode decides how the candidate chain picks its first
	// candidate per request: failover always starts at the head of
	// sort_order, balanced spreads caller API keys across providers with
	// per-key sticky bindings. Set explicitly at every row-creation site —
	// GORM inserts all mapped columns, so an unset field would store the
	// empty string rather than let the column default apply.
	SchedulingMode SchedulingMode `gorm:"column:scheduling_mode" json:"scheduling_mode"`
	// SupportsImageInput is the admin's declaration of whether this model can
	// read images. Tri-state: nil = undeclared, and the gateway must not
	// touch images at all (neither describe nor strip) — a missing
	// declaration must never degrade a vision-capable model.
	SupportsImageInput *bool     `gorm:"column:supports_image_input" json:"supports_image_input"`
	CreatedAt          time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt          time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (Model) TableName() string { return "models" }

// SchedulingMode is how the gateway orders a model's candidate chain. It is
// its own string type so the two-value vocabulary travels with the value
// instead of every consumer re-validating a bare string.
type SchedulingMode string

// Failover is the historical behaviour and the default for every model that
// never chose one, so an empty value reads as failover wherever the column
// is interpreted.
const (
	ModelSchedulingModeFailover SchedulingMode = "failover"
	ModelSchedulingModeBalanced SchedulingMode = "balanced"
)

// Normalized maps a stored or submitted scheduling mode onto the valid
// vocabulary: empty becomes failover (pre-migration rows, callers that never
// sent the field). Anything else passes through unchanged and must be
// validated separately via Valid.
func (m SchedulingMode) Normalized() SchedulingMode {
	if m == "" {
		return ModelSchedulingModeFailover
	}
	return m
}

// Valid reports whether the mode is one of the two schedulers.
func (m SchedulingMode) Valid() bool {
	return m == ModelSchedulingModeFailover || m == ModelSchedulingModeBalanced
}

// IsBalanced reports whether the model uses the balanced scheduler; the
// empty pre-migration value normalizes to failover.
func (m Model) IsBalanced() bool {
	return m.SchedulingMode.Normalized() == ModelSchedulingModeBalanced
}

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
	// LastTestError is the diagnostic of the most recent probe that ran and
	// failed; a passing probe clears it. Nil when no probe has failed (or none
	// has run). It exists so asynchronous (post-import) probe failures keep an
	// actionable reason after the request that queued them has returned.
	LastTestError *string `gorm:"column:last_test_error" json:"last_test_error"`
	// LastProbeRunID identifies the probe run that last wrote this row's probe
	// outcome ("" = never probed). It is the ownership token concurrent probes
	// compare-and-set on: unlike a timestamp, ids never collide or order
	// ambiguously, and a run can recognize its own already-applied write after
	// a lost acknowledgment by reading the id back. Storage detail, not API.
	LastProbeRunID string `gorm:"column:last_probe_run_id" json:"-"`
	// AutoEnableOnPass is the import flow's standing promise: armed by bulk
	// import (and re-armed by a requeue), it lets the background probe queue
	// enable the row on a pass — checked inside the commit statement, so an
	// admin's explicit disable (which clears it) wins whether the probe is
	// still queued or mid-flight. A pass consumes it either way: fulfilled
	// when the row was still aligned (see ArmedAt), revoked when it was not.
	// Storage detail, not API.
	AutoEnableOnPass bool `gorm:"column:auto_enable_on_pass" json:"-"`
	// ArmedAt pins when the promise was armed: arming writes it equal to the
	// same statement's updated_at, and the enable requires the equality to
	// still hold at commit time. Every writer of this table bumps updated_at
	// — including binaries too old to know the armed columns — so any write
	// after arming breaks the alignment and blocks the auto-enable. The row
	// itself carries the revocation signal an old binary's disable could not
	// otherwise leave. Storage detail, not API.
	ArmedAt   *time.Time `gorm:"column:armed_at" json:"-"`
	CreatedAt time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at" json:"updated_at"`
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
