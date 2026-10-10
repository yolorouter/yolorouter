// Package model defines Model / ModelCandidate.
// Schema lives in migrations/{sqlite,postgres}/00005_create_models.sql —
// goose owns DDL, GORM here is query-only (no AutoMigrate).
package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// Model is one externally-exposed model name. Deleting one is a hard
// cascade: its provider candidates and key-allowlist references go in the
// same transaction as the row itself, while request history stays behind
// under the name (logs key on model_name, not the id). management_status
// only toggles routing off — it removes nothing.
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
	//
	// Deprecated: InputModalities carries the same fact as part of a full
	// input-modality array. This column is kept in mirror for the transition
	// period — every write of input_modalities derives it (contains image ⇒
	// true, a non-empty list without image ⇒ false, undeclared ⇒ nil) in the
	// same statement, and the migration backfilled input_modalities from it —
	// because the gateway's vision-fallback hot path still reads this column.
	// It will be removed once the gateway reads input_modalities instead.
	SupportsImageInput *bool `gorm:"column:supports_image_input" json:"supports_image_input"`
	// ContextLength is the admin's declared context window in tokens. Nil
	// means undeclared. Purely informational at this stage — no routing or
	// admission decision reads it yet.
	ContextLength *int `gorm:"column:context_length" json:"context_length"`
	// InputModalities is the JSON array of modality ids this model accepts as
	// input ("text", "image", ...), stored as a string column. Supersedes the
	// deprecated SupportsImageInput boolean: the array form can express
	// audio/video input the boolean never could. Read leniently: an empty,
	// NULL or malformed value reads as undeclared (a nil list), which is what
	// every row predating the column was — and an undeclared input must never
	// be read as "text only".
	InputModalities string `gorm:"column:input_modalities" json:"input_modalities"`
	// SupportsTools is the admin's tri-state declaration that the model can
	// be called with tools (nil = undeclared, never "no"). Model-level and
	// admin-declared, so it is deliberately distinct from the candidate-level
	// SupportsFunctionCalling, which only records what a probe confirmed on
	// one mapping. Informational at this stage.
	SupportsTools *bool `gorm:"column:supports_tools" json:"supports_tools"`
	// SupportsReasoning is the admin's tri-state declaration that the model
	// produces reasoning output (nil = undeclared, never "no"). Informational
	// at this stage.
	SupportsReasoning *bool `gorm:"column:supports_reasoning" json:"supports_reasoning"`
	// Temperature is the admin's declared recommended default temperature for
	// calls to this model. Nil means undeclared. It is model metadata — the
	// name collides with the request-passthrough temperature the gateway
	// forwards upstream, and this field does not participate in that path at
	// all. Informational at this stage.
	Temperature *float64 `gorm:"column:temperature" json:"temperature"`
	// OutputModalities is the JSON array of modality ids this model produces
	// ("text", "image"), stored as a string column. The gateway routes a
	// request only when the endpoint's modality is declared here, so models
	// stay in the pools they belong to. Read leniently: an empty or malformed
	// value reads as text-only, which is what every row predating the column
	// was.
	OutputModalities string    `gorm:"column:output_modalities" json:"output_modalities"`
	CreatedAt        time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (Model) TableName() string { return "models" }

// The modality ids a model may declare in output_modalities. The gateway's
// modality registry uses the same spellings for its own ids, so the two
// vocabularies meet as plain strings without either side importing the other.
const (
	OutputModalityText  = "text"
	OutputModalityImage = "image"
	OutputModalityVideo = "video"
	OutputModalityAudio = "audio"
)

// ValidOutputModalities is the write-path vocabulary: every id an
// output_modalities value may contain. The read path (ServesOutputModality)
// is deliberately laxer — an unknown id in a stored row reads as "not this
// modality" rather than failing the request.
var ValidOutputModalities = []string{OutputModalityText, OutputModalityImage, OutputModalityVideo, OutputModalityAudio}

// DefaultOutputModalitiesJSON is what a row created without a declaration
// stores: text only, which is what every model predating the column was.
const DefaultOutputModalitiesJSON = `["text"]`

// OutputModalityList parses the stored declaration. Empty or malformed
// values read as text-only — a read must always have a usable answer, and
// every row that predates the column was a text model.
func (m Model) OutputModalityList() []string {
	raw := strings.TrimSpace(m.OutputModalities)
	if raw == "" {
		return []string{OutputModalityText}
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list) == 0 {
		return []string{OutputModalityText}
	}
	return list
}

// ServesOutputModality reports whether the model declares it produces
// modality. The id spelled here is the gateway modality's own id, so the
// caller's question and the row's answer share one vocabulary.
func (m Model) ServesOutputModality(modality string) bool {
	for _, id := range m.OutputModalityList() {
		if id == modality {
			return true
		}
	}
	return false
}

// OutputVideoExclusive reports whether the model declares video output
// and nothing else: the shape whose mappings bill per second and whose
// probes speak a task dialect rather than a completion. A model that also
// serves text keeps chat semantics, because the same mapping carries its
// chat traffic too.
func (m Model) OutputVideoExclusive() bool {
	return m.ServesOutputModality(OutputModalityVideo) && len(m.OutputModalityList()) == 1
}

// OutputImageExclusive reports whether the model declares image output and
// nothing else. That is the shape whose mappings bill per image; a model that
// also serves text keeps token settlement, because the same mapping carries
// its chat traffic too.
func (m Model) OutputImageExclusive() bool {
	return !m.ServesOutputModality(OutputModalityText) && m.ServesOutputModality(OutputModalityImage)
}

// OutputAudioExclusive reports whether the model declares audio output and
// nothing else — the shape whose mappings bill per character and whose
// probes speak a speech dialect rather than a completion.
func (m Model) OutputAudioExclusive() bool {
	return !m.ServesOutputModality(OutputModalityText) && m.ServesOutputModality(OutputModalityAudio)
}

// CanonicalOutputModalities validates a declaration from the write path and
// returns the JSON form the column stores. Rejects an empty list (a model
// produces something), an unknown id (typos must not silently narrow
// routing), and a duplicate (the list is a set, and a repeated entry would
// be stored to be read back as one).
func CanonicalOutputModalities(list []string) (string, error) {
	if len(list) == 0 {
		return "", errors.New("output modalities must not be empty")
	}
	seen := make(map[string]bool, len(list))
	for _, id := range list {
		if !slices.Contains(ValidOutputModalities, id) {
			return "", fmt.Errorf("unknown output modality %q", id)
		}
		if seen[id] {
			return "", fmt.Errorf("duplicate output modality %q", id)
		}
		seen[id] = true
	}
	out, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// The modality ids a model may declare in input_modalities. They share their
// spellings with the output vocabulary because both sides name the same
// media kinds; they are kept as separate constants because each column's
// write path validates against its own list, and a future modality may
// legitimately be valid on one side and not the other.
const (
	InputModalityText  = "text"
	InputModalityImage = "image"
	InputModalityAudio = "audio"
	InputModalityVideo = "video"
)

// ValidInputModalities is the write-path vocabulary for input_modalities,
// mirroring ValidOutputModalities. The read path (InputModalityList) is
// deliberately laxer — an unknown id in a stored row reads as simply not
// that modality rather than failing anything.
var ValidInputModalities = []string{InputModalityText, InputModalityImage, InputModalityAudio, InputModalityVideo}

// CanonicalInputModalities validates an input declaration from the write
// path and returns the JSON form the column stores. The empty string (also
// returned for an empty list) means "no declaration" and is stored as NULL —
// unlike output modalities, an undeclared input is a legal, meaningful state
// (the tri-state the old boolean column carried). Ids are case-folded and
// trimmed before validation so a sloppy client cannot store a near-miss of a
// valid id; an empty or unknown id after folding is a rejection, as is a
// duplicate.
func CanonicalInputModalities(list []string) (string, error) {
	if len(list) == 0 {
		return "", nil
	}
	folded := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, raw := range list {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			return "", errors.New("input modalities must not contain an empty id")
		}
		if !slices.Contains(ValidInputModalities, id) {
			return "", fmt.Errorf("unknown input modality %q", raw)
		}
		if seen[id] {
			return "", fmt.Errorf("duplicate input modality %q", id)
		}
		seen[id] = true
		folded = append(folded, id)
	}
	out, err := json.Marshal(folded)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// InputModalityList parses the stored declaration. NULL, empty and malformed
// values all read as nil — undeclared — because that is what every row that
// predates the column was, and an undeclared input must never be narrowed
// into a declaration (nil is the state the gateway-side semantics treat as
// "do not act on this").
func (m Model) InputModalityList() []string {
	raw := strings.TrimSpace(m.InputModalities)
	if raw == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list) == 0 {
		return nil
	}
	return list
}

// DeriveImageInputSupport maps a canonical input_modalities value onto the
// deprecated supports_image_input tri-state, preserving the column's
// semantics: a list containing image ⇒ true, a non-empty list without image
// ⇒ false, and no declaration (empty, NULL, or unparseable) ⇒ nil. The
// direction is load-bearing — the vision-fallback hot path still reads the
// old column, so a save that declared ["text","image"] while leaving the old
// column nil would silently disable image pass-through. Every writer of
// input_modalities must derive the old column in the same statement through
// this function.
func DeriveImageInputSupport(canonicalInputModalities string) *bool {
	list := Model{InputModalities: canonicalInputModalities}.InputModalityList()
	if len(list) == 0 {
		return nil
	}
	for _, id := range list {
		if id == InputModalityImage {
			t := true
			return &t
		}
	}
	f := false
	return &f
}

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
	// BillingMode says which quantities settlement prices this candidate
	// in: token (the default every row had before the column), image (per
	// delivered image, through ImagePricingTiers), video (per delivered
	// second, through VideoPricingTiers), or audio (per million counted
	// characters, through AudioUnitPrice).
	BillingMode string `gorm:"column:billing_mode" json:"billing_mode"`
	// ImagePricingTiers is the per-image price table, stored as the JSON
	// form model.ImagePricingTiers defines. Only read when BillingMode is
	// image; parsed leniently on the hot path and validated at save time.
	ImagePricingTiers string `gorm:"column:image_pricing_tiers" json:"image_pricing_tiers"`
	// VideoPricingTiers is the per-second price table, stored as the JSON
	// form model.VideoPricingTiers defines (the shared admin frontend's
	// editor shape). Only read when BillingMode is video; parsed leniently
	// on the hot path and validated at save time.
	VideoPricingTiers string `gorm:"column:video_pricing_tiers" json:"video_pricing_tiers"`
	// AudioUnitPrice is the per-million-characters price, the single price
	// an audio-mode candidate bills by (speech has no input/output or tier
	// axes). Only read when BillingMode is audio. NULL is unpriced — a
	// distinct fact from free, which is 0.
	AudioUnitPrice *float64 `gorm:"column:audio_unit_price" json:"audio_unit_price"`
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
