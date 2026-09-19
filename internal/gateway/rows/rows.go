// Package rows is the kernel's own vocabulary: the row shapes it
// actually consumes, with no dependency on any deployment's model or
// repository package. The kernel's Store answers in these shapes; a
// deployment converts from its own storage once, at assembly time.
//
// The field sets are exactly what the kernel reads (plus identity
// fields), nothing more: a field no kernel code touches does not belong
// here, because every field here is a promise every deployment's
// adapter must keep.
package rows

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Provider is the routing-relevant face of a provider row.
type Provider struct {
	ID                 uint
	Name               string
	ProviderType       string
	BaseURL            string
	ProtocolEndpoints  string
	ManagementStatus   int
	DestinationVersion int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ProviderKey is one dispatchable credential as the walk sees it. The
// generation pair (ConfigVersion, TestGeneration) drives the bench's
// era rules; EncryptedKey is the ciphertext the kernel decrypts through
// its SecretBox.
type ProviderKey struct {
	ID           uint
	ProviderID   uint
	Label        string
	EncryptedKey string
	KeyPrefix    string
	TestModel    string
	SortOrder    int
	// ManagementStatus / VerificationStatus are int-coded exactly as the
	// deployment stores them; the kernel only compares against the
	// *StatusEnabled / VerificationStatusPassed constants below.
	ManagementStatus   int
	VerificationStatus int
	// AuthorizedDestinationVersion is the provider destination version
	// this key was last verified against; a mismatch retires the key.
	AuthorizedDestinationVersion int
	ConfigVersion                int
	TestGeneration               int
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

// ModelCandidate is one routable mapping with its pricing snapshot.
// Provider is preloaded by the listing store call — the walk reads
// provider facts off the candidate without a second query.
type ModelCandidate struct {
	ID                uint
	ModelID           uint
	ProviderID        uint
	ProviderModelName string
	Provider          *Provider
	InputPrice        float64
	OutputPrice       float64
	CacheWritePrice   *float64
	CacheReadPrice    *float64
	MaxOutput         int
	BillingMode       string
	// ImagePricingTiers / VideoPricingTiers are the stored JSON tables,
	// parsed leniently through the Parse helpers below when the billing
	// mode names them.
	ImagePricingTiers string
	VideoPricingTiers string
	// AudioUnitPrice is per-million-characters; nil is unpriced, which is
	// deliberately not the same as free.
	AudioUnitPrice          *float64
	SupportsStreaming       *bool
	SupportsFunctionCalling *bool
	ManagementStatus        int
	VerificationStatus      int
	SortOrder               int
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Model is the model row's routing face.
type Model struct {
	ID                 uint
	Name               string
	ManagementStatus   int
	SchedulingMode     SchedulingMode
	SupportsImageInput *bool
	OutputModalities   string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// APIKey is the caller credential's face: quota switches, budget axes,
// and the per-key overrides capabilities read.
type APIKey struct {
	ID                                uint
	UserID                            uint
	Status                            int
	ExpiresAt                         *time.Time
	RPMLimit                          *int
	TPMLimit                          *int
	ConcurrencyLimit                  *int
	AllowAllModels                    bool
	BudgetLimitMicros                 *int64
	BudgetSpentMicros                 int64
	CustomSystemPromptEnabledOverride bool
	CustomSystemPromptEnabled         bool
	CustomSystemPrompt                string
	CompressEnabledOverride           bool
	CompressEnabled                   bool
	KeyHash                           string
	KeyPrefix                         string
	CreatedAt                         time.Time
	UpdatedAt                         time.Time
}

// VideoTask is the asynchronous job resource the video modality creates
// and the poll side renders.
type VideoTask struct {
	ID                  string
	APIKeyID            uint
	ModelID             uint
	ModelName           string
	CandidateID         uint
	ProviderID          uint
	ProviderModelName   string
	ProviderTaskID      string
	DestinationVersion  int
	RequestID           string
	Status              string
	ErrorCode           string
	ErrorMessage        string
	RequestSnapshot     string
	Size                string
	Seconds             int
	ResultURL           string
	CoverURL            string
	UsageSeconds        int
	EstimatedMicros     int64
	Billed              bool
	BilledMicros        int64
	ExpiresAt           *time.Time
	LastPolledAt        *time.Time
	UpstreamSubmittedAt time.Time
	UpstreamCompletedAt *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Status and vocabulary constants — the values are the persisted
// spellings; the kernel compares against these only.
const (
	ModelStatusEnabled            = 1
	ModelCandidateStatusEnabled   = 1
	ModelVerificationStatusPassed = 1
	VerificationStatusPassed      = 1
	ProviderStatusEnabled         = 1
	ProviderKeyStatusEnabled      = 1
	APIKeyStatusRevoked           = 2

	APIKeyStatusActive              = 1
	VerificationStatusFailed        = 2
	ModelVerificationStatusUntested = 0
	ModelVerificationStatusFailed   = 2
	ProviderStatusDisabled          = 2
	ProviderKeyStatusDisabled       = 2
	ModelStatusDisabled             = 2
	ModelCandidateStatusDisabled    = 2

	BillingModeToken = "token"
	BillingModeImage = "image"
	BillingModeVideo = "video"
	BillingModeAudio = "audio"

	MeterRequests = "requests"
	MeterTokens   = "tokens"

	VideoTaskPending    = "pending"
	VideoTaskProcessing = "processing"
	VideoTaskCompleted  = "completed"
	VideoTaskFailed     = "failed"
	VideoTaskExpired    = "expired"
	VideoTaskCancelled  = "cancelled"

	RequestLogSourceVisionFallback = "vision_fallback"

	OutputModalityText  = "text"
	OutputModalityImage = "image"
	OutputModalityVideo = "video"
	OutputModalityAudio = "audio"
)

// SchedulingMode decides how the candidate chain picks its first
// candidate; empty reads as failover.
type SchedulingMode string

const (
	ModelSchedulingModeFailover SchedulingMode = "failover"
	ModelSchedulingModeBalanced SchedulingMode = "balanced"
)

func (m SchedulingMode) Normalized() SchedulingMode {
	if m == "" {
		return ModelSchedulingModeFailover
	}
	return m
}

func (m Model) IsBalanced() bool {
	return m.SchedulingMode.Normalized() == ModelSchedulingModeBalanced
}

func (m Model) ServesOutputModality(modality string) bool {
	for _, id := range m.OutputModalityList() {
		if id == modality {
			return true
		}
	}
	return false
}

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

// ImagePricingTier is one rung of the per-image price table.
type ImagePricingTier struct {
	Quality string  `json:"quality"`
	Size    string  `json:"size"`
	Price   float64 `json:"price"`
}

// ImagePricingTiers is the per-image price table; DefaultPrice prices a
// request that matches no tier (absent is unpriced, not free).
type ImagePricingTiers struct {
	Mode         string             `json:"mode"`
	Tiers        []ImagePricingTier `json:"tiers"`
	DefaultPrice *float64           `json:"default_price,omitempty"`
}

const PerImageMode = "per_image"

// ParseImagePricingTiers reads the stored declaration leniently: empty
// or malformed reads as no table, pricing nothing.
func ParseImagePricingTiers(raw string) *ImagePricingTiers {
	if raw == "" {
		return nil
	}
	var tiers ImagePricingTiers
	if err := json.Unmarshal([]byte(raw), &tiers); err != nil {
		return nil
	}
	if tiers.Mode == "" {
		tiers.Mode = PerImageMode
	}
	return &tiers
}

// VideoPricingTier is one rung of the per-second price table.
type VideoPricingTier struct {
	Resolution    string  `json:"resolution"`
	PurchasePrice float64 `json:"purchase_price"`
	SellPrice     float64 `json:"sell_price"`
}

// VideoPricingTiers is a candidate's per-second video price table.
type VideoPricingTiers struct {
	Tiers []VideoPricingTier `json:"tiers"`
}

// ParseVideoPricingTiers reads the stored declaration leniently.
func ParseVideoPricingTiers(raw string) *VideoPricingTiers {
	if raw == "" {
		return nil
	}
	var tiers VideoPricingTiers
	if err := json.Unmarshal([]byte(raw), &tiers); err != nil {
		return nil
	}
	return &tiers
}

// NormalizeBillingMode maps a stored or submitted mode onto the
// vocabulary: empty and unknown become token, the historical default.
func NormalizeBillingMode(mode string) string {
	switch mode {
	case BillingModeImage, BillingModeVideo, BillingModeAudio:
		return mode
	default:
		return BillingModeToken
	}
}

// ErrVideoTaskNotFound is the sentinel for a job resource lookup that
// matched no row (or no row the caller owns).
var ErrVideoTaskNotFound = errors.New("video task not found")

// ErrNotFound is the store-wide sentinel for a lookup that matched no
// row; deployments translate their own not-found errors into it.
var ErrNotFound = errors.New("not found")

// Price sources a resolved per-image price can be attributed to.
const (
	PriceSourceTier    = "tier"
	PriceSourceDefault = "default"
)

// ResolvePrice matches a request's quality and size against the table:
// empty tier axes are wildcards, an explicit match wins over the
// default, and no match at all is unpriced (ok=false).
func (t *ImagePricingTiers) ResolvePrice(quality, size string) (price float64, source string, ok bool) {
	if t == nil {
		return 0, "", false
	}
	for _, tier := range t.Tiers {
		if tier.Quality != "" && tier.Quality != quality {
			continue
		}
		if tier.Size != "" && normalizeSizeAxis(tier.Size) != normalizeSizeAxis(size) {
			continue
		}
		return tier.Price, PriceSourceTier, true
	}
	if t.DefaultPrice != nil {
		return *t.DefaultPrice, PriceSourceDefault, true
	}
	return 0, "", false
}

func normalizeSizeAxis(size string) string {
	return strings.ReplaceAll(strings.ReplaceAll(size, "x", "*"), "X", "*")
}

// ResolveSellPrice matches a resolution against the per-second table;
// an empty tier resolution is the wildcard.
func (t *VideoPricingTiers) ResolveSellPrice(resolution string) (price float64, ok bool) {
	if t == nil {
		return 0, false
	}
	for _, tier := range t.Tiers {
		if tier.Resolution == "" || tier.Resolution == resolution {
			return tier.SellPrice, true
		}
	}
	return 0, false
}

// QueryResult is one poll of an upstream video task: the raw verdict
// the task domain folds into its state machine.
type QueryResult struct {
	Status       string
	ResultURL    string
	CoverURL     string
	UsageSeconds int
	ErrorCode    string
	ErrorMessage string
}

// BudgetExceededError says a submit would push its key past the budget
// limit, and by how much — the numbers the caller is told, computed from
// the same reads the gate made.
type BudgetExceededError struct {
	Limit    int64
	Spent    int64
	InFlight int64
	Ask      int64
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("video budget exceeded: limit %d, spent %d, in-flight %d, this task %d", e.Limit, e.Spent, e.InFlight, e.Ask)
}

// VideoTaskTerminal reports whether a status is one the state machine
// never moves out of; only terminal rows may settle.
func VideoTaskTerminal(status string) bool {
	switch status {
	case VideoTaskCompleted, VideoTaskFailed, VideoTaskCancelled, VideoTaskExpired:
		return true
	}
	return false
}

// MarshalVideoPricingTiers writes the stored declaration; empty tiers
// marshal to the empty string, which parses back as no table.
func MarshalVideoPricingTiers(t *VideoPricingTiers) (string, error) {
	if t == nil || len(t.Tiers) == 0 {
		return "", nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// MarshalImagePricingTiers writes the stored declaration.
func MarshalImagePricingTiers(t *ImagePricingTiers) (string, error) {
	if t == nil || len(t.Tiers) == 0 && t.DefaultPrice == nil {
		return "", nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
