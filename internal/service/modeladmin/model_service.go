// Package modeladmin owns model configuration business logic,
// running-status computation, and candidate test orchestration.
package modeladmin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/pricecatalog"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/providerproto"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

const (
	ModelRunningStatusNotConfigured = "not_configured"
	ModelRunningStatusPending       = "pending_test"
	ModelRunningStatusAvailable     = "available"
	ModelRunningStatusDegraded      = "degraded"
	ModelRunningStatusUnavailable   = "unavailable"
)

var modelNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

type ModelService struct {
	db      *gorm.DB
	secrets crypto.SecretBox
	client  providerclient.ProviderClient
}

func NewModelService(db *gorm.DB, secrets crypto.SecretBox, client providerclient.ProviderClient) *ModelService {
	return &ModelService{db: db, secrets: secrets, client: client}
}

type CandidateView struct {
	ID                uint     `json:"id"`
	ProviderID        uint     `json:"provider_id"`
	ProviderName      string   `json:"provider_name"`
	ProviderModelName string   `json:"provider_model_name"`
	InputPrice        float64  `json:"input_price"`
	OutputPrice       float64  `json:"output_price"`
	CacheWritePrice   *float64 `json:"cache_write_price"`
	CacheReadPrice    *float64 `json:"cache_read_price"`
	MaxOutput         int      `json:"max_output"`
	// Mirrors model.ModelCandidate: true when the last probe confirmed the
	// capability, null when it did not. Informational — routing ignores these.
	SupportsStreaming       *bool `json:"supports_streaming"`
	SupportsFunctionCalling *bool `json:"supports_function_calling"`
	ManagementStatus        int   `json:"management_status"`
	SortOrder               int   `json:"sort_order"`
	VerificationStatus      int   `json:"verification_status"`
	Routable                bool  `json:"routable"`
	// BlockedBy names what stops this candidate being routed to, empty when
	// nothing does. Routable is kept alongside it rather than derived at every
	// call site: a list that only wants to grey out a row should not have to
	// know the vocabulary of reasons to do it.
	BlockedBy          string     `json:"blocked_by"`
	LastTestResult     *int       `json:"last_test_result"`
	LastTestDurationMs *int64     `json:"last_test_duration_ms"`
	LastTestedAt       *time.Time `json:"last_tested_at"`
}

type ModelView struct {
	ID               uint   `json:"id"`
	Name             string `json:"name"`
	ManagementStatus int    `json:"management_status"`
	RunningStatus    string `json:"running_status"`
	// SupportsImageInput mirrors the admin's tri-state declaration (nil =
	// undeclared): whether this model can read images, driving the vision
	// fallback and strip behaviors in the gateway.
	SupportsImageInput *bool           `json:"supports_image_input"`
	Candidates         []CandidateView `json:"candidates"`
	CreatedAt          time.Time       `json:"created_at"`
}

// Why a candidate cannot be routed to. Each value names a different repair, and
// which one applies is the whole question an operator has when a model will not
// serve: turning a provider back on, adding a key, filling in a name, and
// running a probe are four unrelated pieces of work.
//
// Empty means routable.
const (
	CandidateBlockedByOwnStatus   = "candidate_disabled"
	CandidateBlockedByProvider    = "provider_disabled"
	CandidateBlockedByNoUsableKey = "no_usable_key"
	CandidateBlockedByMissingName = "no_provider_model_name"
	CandidateBlockedByUnverified  = "not_verified"
)

// CandidateBlockedBy implements the exhaustive routable-candidate list,
// answering not just whether a candidate can be routed to but what stops it —
// this deliberately does NOT check anything resembling the
// authorized_destination_version/destination_version staleness gate; a
// candidate's mapping validity and a provider key's credential validity are
// different dimensions.
//
// The order is the order an operator fixes things in, and it is why only one
// reason is reported rather than all of them: each step is a precondition of
// the next. A probe needs an enabled provider with a usable key and a name to
// probe under, and SetCandidateStatus refuses to enable a candidate that has
// not passed a probe — so the candidate's own switch comes last, because until
// everything before it is cleared, flipping it cannot succeed.
func CandidateBlockedBy(c model.ModelCandidate, providerEnabled, providerHasAvailableKey bool) string {
	switch {
	case !providerEnabled:
		return CandidateBlockedByProvider
	case !providerHasAvailableKey:
		return CandidateBlockedByNoUsableKey
	case c.ProviderModelName == "":
		return CandidateBlockedByMissingName
	case c.VerificationStatus != model.ModelVerificationStatusPassed:
		return CandidateBlockedByUnverified
	case c.ManagementStatus != model.ModelCandidateStatusEnabled:
		return CandidateBlockedByOwnStatus
	default:
		return ""
	}
}

func computeModelRunningStatus(candidates []CandidateView) string {
	if len(candidates) == 0 {
		return ModelRunningStatusNotConfigured
	}
	anyVerified := false
	for _, c := range candidates {
		if c.VerificationStatus == model.ModelVerificationStatusPassed {
			anyVerified = true
			break
		}
	}
	if !anyVerified {
		return ModelRunningStatusPending
	}
	if candidates[0].Routable {
		return ModelRunningStatusAvailable
	}
	for _, c := range candidates[1:] {
		if c.Routable {
			return ModelRunningStatusDegraded
		}
	}
	return ModelRunningStatusUnavailable
}

// ProviderHasAvailableKey applies the same "available key" rule
// the provider running-status computation uses: enabled + verified +
// authorized for the provider's current destination_version.
func ProviderHasAvailableKey(keys []model.ProviderKey, destinationVersion int) bool {
	for _, k := range keys {
		if k.ManagementStatus == model.ProviderKeyStatusEnabled && k.VerificationStatus == model.VerificationStatusPassed &&
			k.AuthorizedDestinationVersion == destinationVersion {
			return true
		}
	}
	return false
}

// buildCandidateView maps a ModelCandidate plus its already-resolved
// provider name/routability into the API-facing CandidateView shape — the
// one piece of construction shared by toModelView (batched, list-wide) and
// toCandidateView (single candidate, always fetches its own provider/keys).
func buildCandidateView(c model.ModelCandidate, providerName string, blockedBy string) CandidateView {
	return CandidateView{
		ID: c.ID, ProviderID: c.ProviderID, ProviderName: providerName, ProviderModelName: c.ProviderModelName,
		InputPrice: c.InputPrice, OutputPrice: c.OutputPrice, CacheWritePrice: c.CacheWritePrice, CacheReadPrice: c.CacheReadPrice,
		MaxOutput: c.MaxOutput, SupportsStreaming: c.SupportsStreaming, SupportsFunctionCalling: c.SupportsFunctionCalling,
		ManagementStatus: c.ManagementStatus, SortOrder: c.SortOrder, VerificationStatus: c.VerificationStatus,
		Routable:           blockedBy == "",
		BlockedBy:          blockedBy,
		LastTestResult:     c.LastTestResult,
		LastTestDurationMs: c.LastTestDurationMs,
		LastTestedAt:       c.LastTestedAt,
	}
}

// toModelView never queries the database itself — keysByProvider must
// already hold every provider's keys referenced by candidates (batched by
// the caller via repository.ListProviderKeysByProviderIDs) so that listing
// many models doesn't turn into one key query per candidate.
func (s *ModelService) toModelView(m model.Model, candidates []model.ModelCandidate, keysByProvider map[uint][]model.ProviderKey) ModelView {
	views := make([]CandidateView, 0, len(candidates))
	for _, c := range candidates {
		providerEnabled := false
		hasAvailableKey := false
		providerName := ""
		if c.Provider != nil {
			providerEnabled = c.Provider.ManagementStatus == model.ProviderStatusEnabled
			providerName = c.Provider.Name
			hasAvailableKey = ProviderHasAvailableKey(keysByProvider[c.ProviderID], c.Provider.DestinationVersion)
		}
		blockedBy := CandidateBlockedBy(c, providerEnabled, hasAvailableKey)
		views = append(views, buildCandidateView(c, providerName, blockedBy))
	}
	return ModelView{
		ID: m.ID, Name: m.Name, ManagementStatus: m.ManagementStatus, SupportsImageInput: m.SupportsImageInput,
		RunningStatus: computeModelRunningStatus(views), Candidates: views, CreatedAt: m.CreatedAt,
	}
}

// KeysByProviderForCandidates batches the provider-keys lookup for every
// distinct ProviderID referenced across candidates into a single query
// (repository.ListProviderKeysByProviderIDs), avoiding the N+1 pattern of
// looking up one provider's keys per candidate.
func KeysByProviderForCandidates(db *gorm.DB, candidates []model.ModelCandidate) (map[uint][]model.ProviderKey, error) {
	providerIDSet := make(map[uint]struct{}, len(candidates))
	for _, c := range candidates {
		providerIDSet[c.ProviderID] = struct{}{}
	}
	providerIDs := make([]uint, 0, len(providerIDSet))
	for id := range providerIDSet {
		providerIDs = append(providerIDs, id)
	}
	keys, err := repository.ListProviderKeysByProviderIDs(db, providerIDs)
	if err != nil {
		return nil, err
	}
	keysByProvider := make(map[uint][]model.ProviderKey, len(providerIDs))
	for _, k := range keys {
		keysByProvider[k.ProviderID] = append(keysByProvider[k.ProviderID], k)
	}
	return keysByProvider, nil
}

func (s *ModelService) ListModels() ([]ModelView, error) {
	models, err := repository.ListModels(s.db)
	if err != nil {
		return nil, err
	}
	ids := make([]uint, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	allCandidates, err := repository.ListModelCandidatesByModelIDs(s.db, ids)
	if err != nil {
		return nil, err
	}
	candidatesByModel := make(map[uint][]model.ModelCandidate, len(models))
	for _, c := range allCandidates {
		candidatesByModel[c.ModelID] = append(candidatesByModel[c.ModelID], c)
	}
	keysByProvider, err := KeysByProviderForCandidates(s.db, allCandidates)
	if err != nil {
		return nil, err
	}
	views := make([]ModelView, 0, len(models))
	for _, m := range models {
		views = append(views, s.toModelView(m, candidatesByModel[m.ID], keysByProvider))
	}
	return views, nil
}

func (s *ModelService) GetModelDetail(id uint) (*ModelView, error) {
	m, err := repository.FindModelByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrModelNotFound
		}
		return nil, err
	}
	candidates, err := repository.ListModelCandidatesByModelID(s.db, id)
	if err != nil {
		return nil, err
	}
	keysByProvider, err := KeysByProviderForCandidates(s.db, candidates)
	if err != nil {
		return nil, err
	}
	view := s.toModelView(*m, candidates, keysByProvider)
	return &view, nil
}

type CreateModelInput struct {
	Name string
}

func isValidModelName(name string) bool {
	return len(name) > 0 && len(name) <= 100 && modelNamePattern.MatchString(name)
}

func (s *ModelService) CreateModel(input CreateModelInput, now time.Time) (*ModelView, error) {
	if !isValidModelName(input.Name) {
		return nil, fmt.Errorf("%w: model name must contain only letters, digits, dots, hyphens, and underscores", errcode.ErrModelNameTaken)
	}
	if _, err := repository.FindModelByName(s.db, input.Name); err == nil {
		return nil, errcode.ErrModelNameTaken
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	m := &model.Model{Name: input.Name, ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateModel(s.db, m); err != nil {
		if repository.IsUniqueViolation(err) {
			return nil, errcode.ErrModelNameTaken
		}
		return nil, err
	}
	return s.GetModelDetail(m.ID)
}

// Reasons a name is skipped by CreateModelsBatch, surfaced verbatim in the
// per-item summary so the client can group them ("already exists" vs
// "invalid name").
const (
	BatchSkipReasonExists  = "exists"
	BatchSkipReasonInvalid = "invalid"
)

type BatchSkippedModel struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type BatchCreateModelsResult struct {
	Created []ModelView         `json:"created"`
	Skipped []BatchSkippedModel `json:"skipped"`
}

// CreateModelsBatch creates each requested name best-effort: invalid names and
// names that already exist are skipped and reported, the rest are created. A
// name repeated within the batch is created once; later occurrences skip as
// "exists" (the first insert is visible to the later lookup inside the same
// transaction).
//
// All inserts run in ONE transaction: a genuine storage failure rolls the
// whole batch back and returns an error, so the caller never ends up with some
// models silently committed while it sees a total failure (which would make a
// retry report those committed names as already-existing). Invalid/duplicate
// names are skips, not errors, so they never abort the transaction.
func (s *ModelService) CreateModelsBatch(names []string, now time.Time) (*BatchCreateModelsResult, error) {
	result := &BatchCreateModelsResult{Created: []ModelView{}, Skipped: []BatchSkippedModel{}}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for _, name := range names {
			if !isValidModelName(name) {
				result.Skipped = append(result.Skipped, BatchSkippedModel{Name: name, Reason: BatchSkipReasonInvalid})
				continue
			}
			if _, err := repository.FindModelByName(tx, name); err == nil {
				result.Skipped = append(result.Skipped, BatchSkippedModel{Name: name, Reason: BatchSkipReasonExists})
				continue
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			m := &model.Model{Name: name, ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
			if err := repository.CreateModel(tx, m); err != nil {
				// A unique violation here means a concurrent request claimed
				// the name between the lookup above and this insert; roll the
				// batch back and let the caller retry (which then skips it as
				// existing) rather than poison the transaction with a caught
				// constraint error.
				return err
			}
			// A brand-new model has no candidates, so its view is built
			// directly from the inserted row (running status not_configured)
			// instead of re-reading it — the row isn't visible outside this
			// still-open transaction anyway.
			result.Created = append(result.Created, s.toModelView(*m, nil, nil))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type CreateCandidateInput struct {
	ProviderID        uint
	ProviderModelName string
	InputPrice        float64
	OutputPrice       float64
	CacheWritePrice   *float64
	CacheReadPrice    *float64
	MaxOutput         int
	ManagementStatus  int // requested target status; only ==Enabled triggers the server-side retest
}

// SuggestedPrice is one auto-fill result for a provider+model pair. The four
// price slots mirror model.ModelCandidate; Source records where the value came
// from so the UI can tell the admin ("from history" vs "from official catalog")
// and is empty ("") when nothing was found — the form then stays at its default.
type SuggestedPrice struct {
	InputPrice      float64  `json:"input_price"`
	OutputPrice     float64  `json:"output_price"`
	CacheWritePrice *float64 `json:"cache_write_price"`
	CacheReadPrice  *float64 `json:"cache_read_price"`
	Source          string   `json:"source"` // "history" | "seed" | ""
	// CatalogUpdatedAt is the freshness date of the built-in price catalog, in
	// YYYY-MM-DD form, set only for Source "seed". The catalog is refreshed
	// daily by an automated cron (see internal/pricecatalog/live.go), so this
	// date tracks how current the suggested price is — a stale date means the
	// auto-refresh has been failing and the figure may bill an outdated rate;
	// the UI shows it next to the suggestion. Empty for the other sources,
	// where the price came from this deployment's own records.
	CatalogUpdatedAt string `json:"catalog_updated_at"`
}

// SuggestCandidatePrice looks up a price to pre-fill when adding a candidate,
// checking two sources in order: this provider's own previously-saved price
// for the same model (history), then the built-in seed catalog keyed by the
// provider's base_url host. Prices follow the provider, so history takes
// precedence — it reflects what this provider actually charges (possibly a
// negotiated rate) rather than the catalog's generic figure. An empty Source
// means neither source matched and the caller leaves the fields at default.
//
// providerModelName is the name that will actually be sent upstream. Leaving
// the field blank means "use the model's own name", and the caller resolves
// that substitution before asking — a blank name here matches nothing and
// yields an empty suggestion rather than pricing the wrong model.
func (s *ModelService) SuggestCandidatePrice(providerID uint, providerModelName string) (SuggestedPrice, error) {
	providerModelName = strings.TrimSpace(providerModelName)
	if providerModelName == "" {
		return SuggestedPrice{}, nil
	}

	// 1. History: the most recent candidate for this provider + model name.
	if hist, err := repository.FindLatestCandidatePrice(s.db, providerID, providerModelName); err == nil {
		return SuggestedPrice{
			InputPrice:      hist.InputPrice,
			OutputPrice:     hist.OutputPrice,
			CacheWritePrice: hist.CacheWritePrice,
			CacheReadPrice:  hist.CacheReadPrice,
			Source:          "history",
		}, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return SuggestedPrice{}, err
	}

	// 2. Seed catalog: needs the provider's base_url to resolve the host key.
	provider, err := repository.FindProviderByID(s.db, providerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return SuggestedPrice{}, errcode.ErrProviderNotFound
		}
		return SuggestedPrice{}, err
	}
	if p, ok := pricecatalog.Lookup(provider.BaseURL, providerModelName); ok {
		return SuggestedPrice{
			InputPrice:       p.Input,
			OutputPrice:      p.Output,
			CacheWritePrice:  p.CacheWrite,
			CacheReadPrice:   p.CacheRead,
			Source:           "seed",
			CatalogUpdatedAt: pricecatalog.UpdatedAt(),
		}, nil
	}

	return SuggestedPrice{}, nil
}

// decryptHighestPriorityAvailableKey picks the sort_order-first available
// key (enabled+verified+authorized for the provider's current
// destination_version) and decrypts it — candidate tests never touch a key
// that a real request wouldn't itself be allowed to use. ListProviderKeysByProvider
// already orders by sort_order, so the first match here is the
// highest-priority one.
func (s *ModelService) decryptHighestPriorityAvailableKey(keys []model.ProviderKey, destinationVersion int) (string, error) {
	for _, k := range keys {
		if k.ManagementStatus != model.ProviderKeyStatusEnabled || k.VerificationStatus != model.VerificationStatusPassed {
			continue
		}
		// The destination-version check is what makes the promise above true.
		// Without it a probe would send a key to a base_url or protocol the key
		// was never authorized against: changing either bumps
		// destination_version precisely so existing keys stop being used until
		// they are re-entered, and the gateway honours that. A probe that ignored
		// it would hand the credential to a newly configured host — and since
		// candidates now probe on every save, it would do so routinely.
		if k.AuthorizedDestinationVersion != destinationVersion {
			continue
		}
		return s.secrets.Decrypt(k.EncryptedKey)
	}
	return "", errcode.ErrProviderNoTestableModel
}

// classifyBasicResult reduces a basic mapping probe to how verification_status
// should change. overwrite=false means "leave the stored status alone".
//
// The distinction matters because verification_status gates routing: recording an
// inconclusive probe as Failed would take a healthy candidate out of service for
// every request until someone landed a successful retest by hand.
//
// Only two outcomes are decisive about a MAPPING, since the mapping under test is
// "does this provider serve this model name": success proves it does, and the
// model not existing (or being denied specifically for this model) proves it does
// not. Everything else describes the provider's health or the credential rather
// than the mapping — and a candidate's mapping validity is deliberately a
// separate dimension from its key's credential validity (see
// CandidateBlockedBy), so a bad or rate-limited key must not brand the mapping
// broken. An unusable key already makes the candidate unroutable through
// ProviderHasAvailableKey.
func classifyBasicResult(result providerclient.TestResult) (status int, overwrite bool) {
	switch result.Outcome {
	case providerclient.TestSuccess:
		return model.ModelVerificationStatusPassed, true
	case providerclient.TestModelNotFound:
		return model.ModelVerificationStatusFailed, true
	case providerclient.TestPermissionDenied:
		if result.IsModelScoped {
			return model.ModelVerificationStatusFailed, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// classifyCapabilityResult reduces a capability probe to the value to persist:
// true when the capability demonstrably worked, nil otherwise.
//
// It never records a decisive "not supported", because nothing here can tell one
// apart reliably. A model that ignores tools answers 200 with no tool call, which
// is indistinguishable from a mid-stream reset; a refusal arrives as a 4xx, which
// is shared with rate limits, billing and timeouts. Since these flags are shown
// to the admin rather than used for routing, guessing wrong costs a misleading
// tick, and guessing conservatively costs nothing — so the flag means exactly
// "last probe confirmed this", and its absence means "not confirmed".
func classifyCapabilityResult(result providerclient.TestResult) *bool {
	if result.Outcome == providerclient.TestSuccess {
		supported := true
		return &supported
	}
	return nil
}

// runCapabilityTest dispatches one of the two capability probes. The basic
// mapping probe is not routed through here — runCandidateProbes calls
// TestChatCompletion directly, because it gates the other two rather than being
// one of them.
func (s *ModelService) runCapabilityTest(ctx context.Context, proto protocols.ProtocolID, testType, baseURL, apiKey, providerModelName string) (providerclient.TestResult, error) {
	switch testType {
	case "streaming":
		return s.client.TestStreamingCompletion(ctx, proto, baseURL, apiKey, providerModelName)
	case "function_calling":
		return s.client.TestFunctionCalling(ctx, proto, baseURL, apiKey, providerModelName)
	default:
		return providerclient.TestResult{}, fmt.Errorf("unknown capability test_type %q", testType)
	}
}

// ProbeReport is one probe's admin-facing result.
type ProbeReport struct {
	// Ran is false when the probe was deliberately skipped. The UI must show
	// that as "not tested" rather than as a verdict, so an operator never reads
	// a skipped capability probe as proof the capability is missing.
	Ran bool `json:"ran"`
	// Supported is the verdict. For the basic probe it is never nil while Ran is
	// true: anything short of success means the mapping cannot be enabled. For a
	// capability probe it is tri-state — nil means the probe was inconclusive.
	Supported  *bool `json:"supported"`
	Outcome    *int  `json:"outcome"`
	DurationMs int64 `json:"duration_ms"`
	// verificationStatus carries the basic probe's decisive verdict for the
	// verification_status column, or nil when the probe was inconclusive and the
	// stored status must be left alone. Unexported because it is an internal
	// write decision rather than something the admin UI displays; the JSON
	// encoder simply skips it.
	verificationStatus *int
}

// Passed reports whether this probe produced an affirmative verdict.
func (p ProbeReport) Passed() bool { return p.Ran && p.Supported != nil && *p.Supported }

// CandidateTestReport is the full result of probing one mapping.
type CandidateTestReport struct {
	Basic           ProbeReport `json:"basic"`
	Streaming       ProbeReport `json:"streaming"`
	FunctionCalling ProbeReport `json:"function_calling"`
}

// TestAndCreateResult is what the test-then-save endpoint returns. Created is
// false when enablement was requested but the basic probe failed: the caller
// asked for a mapping that works now, so nothing is written and the admin can
// fix the configuration and retry without a dead row being left behind.
type TestAndCreateResult struct {
	Report    CandidateTestReport `json:"report"`
	Created   bool                `json:"created"`
	Candidate *CandidateView      `json:"candidate"`
}

// runCandidateProbes runs the basic mapping probe first and, only when it
// passes, the two capability probes concurrently.
//
// The basic probe gates the others because it is the one that proves the
// fundamentals — credential, address, model name. When it fails the capability
// probes cannot produce a meaningful verdict, so running them would spend two
// more upstream requests to learn nothing, and would risk recording a
// misleading "not supported" for a mapping that is simply misconfigured. Once
// the fundamentals hold the remaining two are independent, so they run together
// to keep the admin's wait to two round trips rather than three.
func (s *ModelService) runCandidateProbes(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, providerModelName string) (CandidateTestReport, error) {
	var report CandidateTestReport

	basic, err := s.client.TestChatCompletion(ctx, proto, baseURL, apiKey, providerModelName)
	if err != nil {
		return report, err
	}
	basicOutcome := int(basic.Outcome)
	basicPassed := basic.Outcome == providerclient.TestSuccess
	report.Basic = ProbeReport{Ran: true, Supported: &basicPassed, Outcome: &basicOutcome, DurationMs: basic.DurationMs}
	if status, overwrite := classifyBasicResult(basic); overwrite {
		report.Basic.verificationStatus = &status
	}
	if !basicPassed {
		return report, nil
	}

	// Errors from the capability probes are deliberately not propagated: an
	// unrunnable capability probe leaves an unknown flag, which routes
	// optimistically, whereas failing the whole call would discard a basic
	// probe that already passed and block a mapping that is known to work.
	var streaming, functionCalling ProbeReport
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		streaming = s.runOneCapabilityProbe(ctx, "streaming", proto, baseURL, apiKey, providerModelName)
	}()
	go func() {
		defer wg.Done()
		functionCalling = s.runOneCapabilityProbe(ctx, "function_calling", proto, baseURL, apiKey, providerModelName)
	}()
	wg.Wait()
	report.Streaming = streaming
	report.FunctionCalling = functionCalling

	return report, nil
}

func (s *ModelService) runOneCapabilityProbe(ctx context.Context, testType string, proto protocols.ProtocolID, baseURL, apiKey, providerModelName string) ProbeReport {
	result, err := s.runCapabilityTest(ctx, proto, testType, baseURL, apiKey, providerModelName)
	if err != nil {
		// Ran stays false: an error here means the probe never reached the
		// upstream — this client's test semaphore was saturated (each
		// providerclient.ProviderClient owns its own pool of providerClientConcurrency slots, so
		// candidate probes contend only with other candidate probes), or the
		// admin's request was cancelled. Reporting it as having run would make
		// toProbeCommit treat the run as authoritative and overwrite stored
		// verdicts with "unknown", destroying an earned verdict because of a
		// local concurrency limit. It would also tell the operator the provider
		// gave no clear answer when the provider was never asked.
		return ProbeReport{Ran: false, Supported: nil, DurationMs: result.DurationMs}
	}
	outcome := int(result.Outcome)
	return ProbeReport{Ran: true, Supported: classifyCapabilityResult(result), Outcome: &outcome, DurationMs: result.DurationMs}
}

// toProbeCommit reduces a report to the row update it implies. The run's
// duration is the basic probe's: it is the only probe guaranteed to have run,
// and the two capability probes overlap so summing them would overstate the
// elapsed time.
func toProbeCommit(report CandidateTestReport) repository.CandidateProbeCommit {
	// A basic probe that never ran leaves the mapping untested, not failed:
	// "failed" is a verdict the upstream returned, and claiming one that was
	// never obtained would misreport a mapping nobody has been able to check yet.
	return repository.CandidateProbeCommit{
		VerificationStatus:      report.Basic.verificationStatus,
		SupportsStreaming:       report.Streaming.Supported,
		SupportsFunctionCalling: report.FunctionCalling.Supported,
		LastTestResult:          report.Basic.Outcome,
		DurationMs:              report.Basic.DurationMs,
	}
}

// probeCandidateMapping resolves the provider, its highest-priority usable key
// and the effective upstream model name, then probes the mapping.
func (s *ModelService) probeCandidateMapping(ctx context.Context, providerID uint, providerModelName string) (CandidateTestReport, error) {
	provider, err := repository.FindProviderByID(s.db, providerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return CandidateTestReport{}, errcode.ErrProviderNotFound
		}
		return CandidateTestReport{}, err
	}
	keys, err := repository.ListProviderKeysByProvider(s.db, providerID)
	if err != nil {
		return CandidateTestReport{}, err
	}
	plaintext, err := s.decryptHighestPriorityAvailableKey(keys, provider.DestinationVersion)
	if err != nil {
		return CandidateTestReport{}, err
	}
	return s.runCandidateProbes(ctx, providerproto.TypeOf(provider.ProviderType), provider.BaseURL, plaintext, providerModelName)
}

// TestAndCreateCandidate probes a mapping and only then decides whether to
// store it, so an admin asking for a working mapping never ends up with a
// broken one saved behind their back.
//
// The probe results are produced here rather than accepted from the caller. A
// client that could assert its own verification_status would be able to create
// a candidate that reads as verified and then flip it to enabled through the
// status endpoint, which checks only that flag — bypassing the entire
// verification gate.
func (s *ModelService) TestAndCreateCandidate(ctx context.Context, modelID uint, input CreateCandidateInput, now time.Time) (*TestAndCreateResult, error) {
	m, err := repository.FindModelByID(s.db, modelID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrModelNotFound
		}
		return nil, err
	}
	providerModelName := input.ProviderModelName
	if providerModelName == "" {
		providerModelName = m.Name
	}

	requestedEnable := input.ManagementStatus == model.ModelCandidateStatusEnabled

	report, err := s.probeCandidateMapping(ctx, input.ProviderID, providerModelName)
	if err != nil {
		// The probe could not run at all — no verified key on the provider yet, or
		// the provider vanished. Asking to enable cannot be honoured, so the
		// reason is surfaced. Saving as disabled still goes through with unknown
		// verdicts: a mapping that cannot route yet is exactly what an admin
		// configures while the provider's credential is still being sorted out,
		// and refusing it would leave them with nowhere to put the configuration.
		if requestedEnable {
			return nil, err
		}
		view, createErr := s.createCandidateWithProbeResults(modelID, input, providerModelName, CandidateTestReport{}, now)
		if createErr != nil {
			return nil, createErr
		}
		return &TestAndCreateResult{Report: CandidateTestReport{}, Created: true, Candidate: view}, nil
	}

	if requestedEnable && !report.Basic.Passed() {
		return &TestAndCreateResult{Report: report, Created: false}, nil
	}

	view, err := s.createCandidateWithProbeResults(modelID, input, providerModelName, report, now)
	if err != nil {
		return nil, err
	}
	return &TestAndCreateResult{Report: report, Created: true, Candidate: view}, nil
}

// createCandidateWithProbeResults inserts the row already carrying the verdicts
// from a probe run that happened before it existed.
func (s *ModelService) createCandidateWithProbeResults(
	modelID uint, input CreateCandidateInput, providerModelName string, report CandidateTestReport, now time.Time,
) (*CandidateView, error) {
	managementStatus := model.ModelCandidateStatusDisabled
	if input.ManagementStatus == model.ModelCandidateStatusEnabled && report.Basic.Passed() {
		managementStatus = model.ModelCandidateStatusEnabled
	}
	commit := toProbeCommit(report)
	// A brand-new row has no stored status to preserve, so an inconclusive probe
	// (nil) starts it at Untested rather than leaving the column unset.
	verificationStatus := model.ModelVerificationStatusUntested
	if commit.VerificationStatus != nil {
		verificationStatus = *commit.VerificationStatus
	}
	candidate := &model.ModelCandidate{
		ModelID: modelID, ProviderID: input.ProviderID, ProviderModelName: providerModelName,
		InputPrice: input.InputPrice, OutputPrice: input.OutputPrice,
		CacheWritePrice: input.CacheWritePrice, CacheReadPrice: input.CacheReadPrice, MaxOutput: input.MaxOutput,
		ManagementStatus:        managementStatus,
		VerificationStatus:      verificationStatus,
		SupportsStreaming:       commit.SupportsStreaming,
		SupportsFunctionCalling: commit.SupportsFunctionCalling,
		LastTestResult:          commit.LastTestResult,
		CreatedAt:               now, UpdatedAt: now, PriceUpdatedAt: now,
	}
	// The test timestamp and duration are only written when a probe actually
	// ran. Stamping them unconditionally would leave a row that simultaneously
	// reports "never tested" and "tested just now, in 0 ms", which any later
	// staleness check or report reading last_tested_at would take at face value.
	if report.Basic.Ran {
		candidate.LastTestDurationMs = &commit.DurationMs
		candidate.LastTestedAt = &now
	}
	if err := s.insertCandidateWithSortOrder(candidate); err != nil {
		return nil, err
	}
	reloaded, err := repository.FindModelCandidateByID(s.db, candidate.ID)
	if err != nil {
		return nil, err
	}
	return s.toCandidateView(*reloaded)
}

// candidateSortOrderInsertAttempts bounds the retry below. A collision needs
// another concurrent insert on the same model to have won the race, so a couple
// of retries covers realistic contention while still terminating.
const candidateSortOrderInsertAttempts = 3

// insertCandidateWithSortOrder appends the candidate at the end of the model's
// route chain, retrying if a concurrent insert claimed the same position.
//
// The retry matters more here than it looks: probing happens before this runs
// and can take tens of seconds, so two admins adding candidates to one model
// read the same "next" value with a very wide window between the read and the
// insert. Without the retry the loser hit UNIQUE(model_id, sort_order) and —
// because IsUniqueViolation is a blanket match — was told its provider was
// already used by this model, which is both untrue and not something they could
// act on. The two constraints on the table are told apart so that message is
// only produced for a genuine duplicate provider.
func (s *ModelService) insertCandidateWithSortOrder(candidate *model.ModelCandidate) error {
	var lastErr error
	for attempt := 0; attempt < candidateSortOrderInsertAttempts; attempt++ {
		sortOrder, err := repository.NextCandidateSortOrder(s.db, candidate.ModelID)
		if err != nil {
			return err
		}
		candidate.SortOrder = sortOrder
		err = repository.CreateModelCandidate(s.db, candidate)
		if err == nil {
			return nil
		}
		if repository.IsSortOrderUniqueViolation(err) {
			// Someone else took this position; re-read and append after them.
			lastErr = err
			continue
		}
		if repository.IsUniqueViolation(err) {
			return errcode.ErrModelCandidateProviderTaken
		}
		return err
	}
	return lastErr
}

func (s *ModelService) CreateModelCandidate(ctx context.Context, modelID uint, input CreateCandidateInput, now time.Time) (*CandidateView, error) {
	m, err := repository.FindModelByID(s.db, modelID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrModelNotFound
		}
		return nil, err
	}
	// providerModelName is optional — a blank value means "use the model's
	// own external name upstream unchanged".
	providerModelName := input.ProviderModelName
	if providerModelName == "" {
		providerModelName = m.Name
	}
	candidate := &model.ModelCandidate{
		ModelID: modelID, ProviderID: input.ProviderID, ProviderModelName: providerModelName,
		InputPrice: input.InputPrice, OutputPrice: input.OutputPrice,
		CacheWritePrice: input.CacheWritePrice, CacheReadPrice: input.CacheReadPrice, MaxOutput: input.MaxOutput,
		ManagementStatus:   model.ModelCandidateStatusDisabled,
		VerificationStatus: model.ModelVerificationStatusUntested,
		CreatedAt:          now, UpdatedAt: now, PriceUpdatedAt: now,
	}
	if err := s.insertCandidateWithSortOrder(candidate); err != nil {
		return nil, err
	}

	// Probing happens only when enablement was asked for. Saving as disabled is
	// the deliberate "store this without touching the upstream" path: the mapping
	// cannot route while disabled, so there is nothing to verify yet, and it is
	// also the admin UI's escape hatch after a probe run has already failed —
	// re-probing there would spend three more upstream requests to re-learn what
	// the operator was just shown.
	//
	// That makes this path store a deliberately barer row than
	// TestAndCreateCandidate's disabled branch: Untested with no verdicts, rather
	// than the verdicts a probe produced. The asymmetry is the accepted cost of
	// not re-probing, and closing it would mean either burning those requests or
	// letting the client supply its own verification_status — which would let it
	// create a row that reads as verified and then enable it through the status
	// endpoint, since that endpoint checks only the flag.
	//
	// Best-effort: the row is already committed, so a probe that cannot run or
	// cannot be persisted leaves it at Disabled/Untested rather than failing a
	// create that otherwise succeeded. The view returned below is re-read from the
	// database, so the caller always sees the state that was actually stored.
	if input.ManagementStatus == model.ModelCandidateStatusEnabled {
		_, _, _ = s.probeAndCommitExistingCandidate(ctx, candidate.ID, input.ProviderID, providerModelName,
			model.ModelCandidateStatusDisabled, true, now)
	}

	reloaded, err := repository.FindModelCandidateByID(s.db, candidate.ID)
	if err != nil {
		return nil, err
	}
	return s.toCandidateView(*reloaded)
}

// probeAndCommitExistingCandidate probes an already-stored candidate and
// commits the verdicts, optionally enabling it when the basic probe passes.
//
// ran reports whether a verdict was actually obtained. It is false when the probe
// could not be started at all — an unresolvable provider or key, a saturated test
// semaphore, a cancelled request — and callers must not read that as a failing
// mapping: demoting a live candidate because of a local concurrency limit would
// take it out of service for a reason that has nothing to do with the upstream.
//
// Probing runs outside any transaction so a slow upstream never holds one open.
func (s *ModelService) probeAndCommitExistingCandidate(
	ctx context.Context, candidateID, providerID uint, providerModelName string,
	expectedManagementStatus int, enableIfPassed bool, now time.Time,
) (report CandidateTestReport, ran bool, err error) {
	report, err = s.probeCandidateMapping(ctx, providerID, providerModelName)
	if err != nil {
		return report, false, err
	}
	// Surfaced rather than swallowed: if the verdict does not reach the database,
	// silently continuing would let the enable below be skipped while the caller
	// is told the save succeeded, leaving a row whose stored state contradicts
	// what the operator was shown.
	applied, err := repository.CommitModelCandidateProbeResults(s.db, candidateID, providerModelName, toProbeCommit(report), now)
	if err != nil {
		return report, true, err
	}
	if !applied {
		// A concurrent edit retargeted the candidate while this probe was in
		// flight, so the verdict describes a mapping the row no longer has. It is
		// dropped, and enablement is not attempted — the edit that moved the row
		// runs its own probe and owns the outcome.
		return report, true, nil
	}
	if enableIfPassed && report.Basic.Passed() {
		if _, err := repository.EnableModelCandidateAfterProbe(
			s.db, candidateID, providerModelName, expectedManagementStatus, model.ModelCandidateStatusEnabled, now,
		); err != nil {
			return report, true, err
		}
	}
	return report, true, nil
}

func (s *ModelService) toCandidateView(c model.ModelCandidate) (*CandidateView, error) {
	provider, err := repository.FindProviderByID(s.db, c.ProviderID)
	if err != nil {
		return nil, err
	}
	keys, err := repository.ListProviderKeysByProvider(s.db, c.ProviderID)
	if err != nil {
		return nil, err
	}
	hasAvailableKey := ProviderHasAvailableKey(keys, provider.DestinationVersion)
	blockedBy := CandidateBlockedBy(c, provider.ManagementStatus == model.ProviderStatusEnabled, hasAvailableKey)
	view := buildCandidateView(c, provider.Name, blockedBy)
	return &view, nil
}

type UpdateCandidateInput struct {
	ProviderModelName string
	InputPrice        float64
	OutputPrice       float64
	CacheWritePrice   *float64
	CacheReadPrice    *float64
	MaxOutput         int
	// ManagementStatus is the requested target status, or nil to leave the
	// current one untouched. It is a pointer because an absent field and a
	// request to disable must not be the same value: with a plain int, any
	// caller editing only prices would send the zero value and silently pull the
	// candidate out of routing.
	//
	// Asking to enable a candidate that is currently disabled triggers a
	// re-probe, because enabling asserts the mapping works right now.
	ManagementStatus *int
}

// UpdateCandidateResult carries the updated candidate plus the probe run, if
// one was needed. Report is nil when nothing about the change could have
// invalidated the stored verdicts.
type UpdateCandidateResult struct {
	Candidate *CandidateView       `json:"candidate"`
	Report    *CandidateTestReport `json:"report"`
}

// sameOptionalPrice compares two nullable prices, where nil ("this model has no
// such price") is a distinct value from any number including zero.
func sameOptionalPrice(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// UpdateModelCandidate saves the edited fields and re-probes only when the
// change could have invalidated what was previously verified.
//
// A re-probe is warranted when the upstream target changed, or when the caller
// is asking to enable a mapping that is not currently enabled — the latter
// because otherwise a mapping whose earlier probe failed could never be
// recovered from this screen. Price and max-output edits say nothing about
// whether the mapping works, so they save without touching the upstream.
//
// The field update is committed regardless of how the probe turns out. The edit
// is an explicit instruction about data the probe has no bearing on, so a
// transient upstream failure must not be able to discard it; a failing probe
// only costs the candidate its enabled state.
func (s *ModelService) UpdateModelCandidate(ctx context.Context, id uint, input UpdateCandidateInput, now time.Time) (*UpdateCandidateResult, error) {
	candidate, err := repository.FindModelCandidateByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrModelCandidateNotFound
		}
		return nil, err
	}
	// providerModelName is optional — a blank value means "use the model's
	// own external name upstream unchanged".
	providerModelName := input.ProviderModelName
	if providerModelName == "" {
		m, err := repository.FindModelByID(s.db, candidate.ModelID)
		if err != nil {
			return nil, err
		}
		providerModelName = m.Name
	}
	// Changing the routing target (provider_model_name) invalidates the prior
	// mapping test, so the candidate must be re-verified before it can route
	// or be enabled again (repository resets verification + capability flags).
	targetChanged := providerModelName != candidate.ProviderModelName
	// The form always posts the whole record, so "a price arrived" is not the
	// same as "a price changed". Only a real change may advance the price clock
	// the auto-suggest look-up ranks candidates by.
	//
	// Retargeting counts as one even when every number stays put: the row now
	// states that rate for a DIFFERENT upstream model, which is a fresh claim
	// about a pair it had never priced. Leaving the clock behind would let some
	// other candidate's older rate for that pair keep winning the look-up.
	priceChanged := targetChanged ||
		input.InputPrice != candidate.InputPrice ||
		input.OutputPrice != candidate.OutputPrice ||
		!sameOptionalPrice(input.CacheWritePrice, candidate.CacheWritePrice) ||
		!sameOptionalPrice(input.CacheReadPrice, candidate.CacheReadPrice)
	if err := repository.UpdateModelCandidate(s.db, id, providerModelName, input.InputPrice, input.OutputPrice,
		input.CacheWritePrice, input.CacheReadPrice, input.MaxOutput, targetChanged, priceChanged, now); err != nil {
		return nil, err
	}

	wasEnabled := candidate.ManagementStatus == model.ModelCandidateStatusEnabled
	targetEnabled := wasEnabled
	if input.ManagementStatus != nil {
		targetEnabled = *input.ManagementStatus == model.ModelCandidateStatusEnabled
	}

	// Probing only earns its cost when the outcome can change something: the
	// candidate is meant to end up enabled, and either its target moved or it is
	// not verified yet. Disabling never needs a probe (it only removes the mapping
	// from rotation), and a price edit on an already-verified, already-enabled
	// candidate says nothing about whether the mapping works.
	//
	// Re-probing an enabled-but-unverified candidate is what keeps this screen
	// from stranding one: verification can end up not-Passed while the row stays
	// enabled (a retest records a failure without touching enablement), and such a
	// row serves no traffic while the admin list shows it as on.
	wasVerified := candidate.VerificationStatus == model.ModelVerificationStatusPassed
	var report *CandidateTestReport
	if targetEnabled && (targetChanged || !wasEnabled || !wasVerified) {
		r, ran, err := s.probeAndCommitExistingCandidate(ctx, id, candidate.ProviderID, providerModelName,
			candidate.ManagementStatus, true, now)
		if err != nil && ran {
			// The probe produced a verdict but persisting it failed. Continuing
			// would report success over a row whose stored state no longer matches
			// what the operator is about to be shown. A probe that never started
			// (ran == false) is not an error worth failing the edit over — the
			// enablement reconciliation below handles the resulting state.
			return nil, err
		}
		report = &r
	}

	reloaded, err := repository.FindModelCandidateByID(s.db, id)
	if err != nil {
		return nil, err
	}

	// Enablement is settled from the row as it now stands, against one invariant:
	// an enabled candidate must be verified. Probing can only ever GRANT the
	// enabled state, so every case that has to take it away is handled here, and
	// deriving the decision from stored state covers all of them at once — an
	// outright request to disable, a probe that ran and failed, a probe that never
	// started after a rename already reset verification, and a row that was
	// already enabled-but-unverified before this edit.
	//
	// Reading the stored status rather than the probe report is what closes the
	// last of those: a rename commits the verification reset up front, so if the
	// probe could not even start the row would otherwise be left enabled and
	// unverified — silently serving nothing while the admin list showed it as on
	// and the API reported success.
	//
	// The write is skipped when the row already reads disabled, so an edit that
	// changes nothing about enablement does not issue a no-op UPDATE.
	nowVerified := reloaded.VerificationStatus == model.ModelVerificationStatusPassed
	nowEnabled := reloaded.ManagementStatus == model.ModelCandidateStatusEnabled
	if nowEnabled && (!targetEnabled || !nowVerified) {
		if err := repository.SetModelCandidateManagementStatus(s.db, id, model.ModelCandidateStatusDisabled, now); err != nil {
			return nil, err
		}
		if reloaded, err = repository.FindModelCandidateByID(s.db, id); err != nil {
			return nil, err
		}
	}

	view, err := s.toCandidateView(*reloaded)
	if err != nil {
		return nil, err
	}
	return &UpdateCandidateResult{Candidate: view, Report: report}, nil
}

// verifyCandidateEnableAllowed mirrors the provider-key enable check: enabling a
// candidate requires it to have passed its basic-text mapping test.
func verifyCandidateEnableAllowed(c *model.ModelCandidate) error {
	if c.VerificationStatus != model.ModelVerificationStatusPassed {
		return errcode.ErrModelCandidateNotVerified
	}
	return nil
}

func (s *ModelService) SetCandidateStatus(id uint, enabled bool, now time.Time) error {
	candidate, err := repository.FindModelCandidateByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errcode.ErrModelCandidateNotFound
		}
		return err
	}
	if !enabled {
		return repository.SetModelCandidateManagementStatus(s.db, id, model.ModelCandidateStatusDisabled, now)
	}
	if err := verifyCandidateEnableAllowed(candidate); err != nil {
		return err
	}
	// CAS-guarded on the same verification_status just checked above — a
	// max-effort-review-style fix applied from day one instead of needing
	// its own round to discover (the same class of check-then-act race
	// exists for provider keys).
	applied, err := repository.SetModelCandidateManagementStatusIfVerified(s.db, id, model.ModelCandidateStatusEnabled, now)
	if err != nil {
		return err
	}
	if !applied {
		return errcode.ErrModelCandidateNotVerified
	}
	return nil
}

// RetestModelCandidate re-probes a stored candidate and commits the verdicts.
//
// It always runs the full set rather than a caller-chosen single probe: a
// capability flag left unknown by an inconclusive probe needs a way back to a
// definite answer, and offering one probe at a time only pushes the choice of
// which to run onto the operator.
//
// Retesting never ENABLES anything — a candidate an admin disabled stays disabled
// even if every probe passes. It does demote one whose mapping is now decisively
// broken, because verification_status not being Passed already stops the gateway
// routing to it; leaving management_status enabled would only make the admin list
// claim it is serving traffic when it is not.
func (s *ModelService) RetestModelCandidate(ctx context.Context, id uint, now time.Time) (*CandidateView, error) {
	candidate, err := repository.FindModelCandidateByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrModelCandidateNotFound
		}
		return nil, err
	}
	report, err := s.probeCandidateMapping(ctx, candidate.ProviderID, candidate.ProviderModelName)
	if err != nil {
		return nil, err
	}
	commit := toProbeCommit(report)
	applied, err := repository.CommitModelCandidateProbeResults(s.db, id, candidate.ProviderModelName, commit, now)
	if err != nil {
		return nil, err
	}
	// Only a decisive verdict demotes, and only if the verdict was actually
	// stored: applied being false means a concurrent edit retargeted the row, so
	// this result describes a mapping it no longer has. An inconclusive probe
	// leaves verification_status untouched, so there is nothing to reconcile.
	if applied && commit.VerificationStatus != nil && *commit.VerificationStatus != model.ModelVerificationStatusPassed &&
		candidate.ManagementStatus == model.ModelCandidateStatusEnabled {
		if err := repository.SetModelCandidateManagementStatus(s.db, id, model.ModelCandidateStatusDisabled, now); err != nil {
			return nil, err
		}
	}
	reloaded, err := repository.FindModelCandidateByID(s.db, id)
	if err != nil {
		return nil, err
	}
	return s.toCandidateView(*reloaded)
}

func (s *ModelService) ReorderModelCandidate(modelID, candidateID uint, direction string) error {
	_, err := repository.SwapModelCandidateSortOrder(s.db, modelID, candidateID, direction)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errcode.ErrModelCandidateNotFound
	}
	return err
}

func (s *ModelService) DeleteModelCandidate(id uint) error {
	if _, err := repository.FindModelCandidateByID(s.db, id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errcode.ErrModelCandidateNotFound
		}
		return err
	}
	return repository.DeleteModelCandidate(s.db, id)
}

// UpdateModelNameStatus saves the model's name — and, when imageInputSet is
// true, the tri-state image-input declaration in the same statement, so
// concurrent PATCHes cannot interleave the two fields. A rename also follows
// through to the vision-fallback setting when the renamed model is the
// configured describe model: the setting stores the public name, and leaving
// the old name behind would silently disable the feature at describe time.
// The model row is re-read inside the same transaction that writes it, so a
// concurrent status flip or rename cannot slip between read and write. The
// gateway's settings cache picks the renamed reference up within its 30s
// TTL; in that window a describe lookup misses and the image passes through
// unconverted, which is the feature's normal degrade mode.
func (s *ModelService) UpdateModelNameStatus(id uint, name string, imageInputSet bool, imageInput *bool, now time.Time) (*ModelView, error) {
	if !isValidModelName(name) {
		return nil, fmt.Errorf("%w: model name must contain only letters, digits, dots, hyphens, and underscores", errcode.ErrModelNameTaken)
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		m, err := repository.FindModelByID(tx, id)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errcode.ErrModelNotFound
			}
			return err
		}
		if err := repository.UpdateModelNameStatus(tx, id, name, m.ManagementStatus, imageInputSet, imageInput, now); err != nil {
			return err
		}
		if name != m.Name {
			return repository.RenameVisionFallbackModel(tx, m.Name, name)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errcode.ErrModelNotFound) {
			return nil, err
		}
		if repository.IsUniqueViolation(err) {
			return nil, errcode.ErrModelNameTaken
		}
		return nil, err
	}
	return s.GetModelDetail(id)
}

func (s *ModelService) SetModelStatus(id uint, enabled bool, now time.Time) error {
	m, err := repository.FindModelByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errcode.ErrModelNotFound
		}
		return err
	}
	status := model.ModelStatusDisabled
	if enabled {
		status = model.ModelStatusEnabled
	}
	return repository.UpdateModelNameStatus(s.db, id, m.Name, status, false, nil, now)
}

// modelImpactRecentWindow is how far back the impact preview counts live
// traffic. A week catches weekly batch jobs, the slowest cadence a caller
// realistically runs at, without the count going stale-heavy.
const modelImpactRecentWindow = 7 * 24 * time.Hour

// ModelImpactKeyView is one key an operator would break: enough to recognize
// it in the key list (label plus prefix), nothing that could rebuild it.
type ModelImpactKeyView struct {
	ID        uint   `json:"id"`
	Remark    string `json:"remark"`
	KeyPrefix string `json:"key_prefix"`
}

// ModelImpactView is what disabling or renaming this model touches.
// AllowlistedKeys are callable keys that name the model explicitly;
// AllowAllKeyCount is how many callable keys reach it implicitly. Allowlists
// reference the model by id and survive a rename, so RecentRequestCount
// carries the rename risk instead: callers ask by name, and this is how many
// recent requests would have asked for a name that no longer routes.
type ModelImpactView struct {
	AllowlistedKeys    []ModelImpactKeyView `json:"allowlisted_keys"`
	AllowAllKeyCount   int64                `json:"allow_all_key_count"`
	RecentRequestCount int64                `json:"recent_request_count"`
	RecentWindowDays   int                  `json:"recent_window_days"`
}

// GetModelImpact answers "what breaks if I disable or rename this model" for
// the confirm dialogs and the impact tab.
func (s *ModelService) GetModelImpact(id uint, now time.Time) (*ModelImpactView, error) {
	m, err := repository.FindModelByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrModelNotFound
		}
		return nil, err
	}
	keys, err := repository.ListCallableAPIKeysAllowlisting(s.db, id, now)
	if err != nil {
		return nil, err
	}
	keyViews := make([]ModelImpactKeyView, 0, len(keys))
	for _, k := range keys {
		keyViews = append(keyViews, ModelImpactKeyView{ID: k.ID, Remark: k.Remark, KeyPrefix: k.KeyPrefix})
	}
	allowAll, err := repository.CountCallableAllowAllAPIKeys(s.db, now)
	if err != nil {
		return nil, err
	}
	recent, err := repository.CountRequestLogsForModelSince(s.db, m.Name, now.Add(-modelImpactRecentWindow))
	if err != nil {
		return nil, err
	}
	return &ModelImpactView{
		AllowlistedKeys:    keyViews,
		AllowAllKeyCount:   allowAll,
		RecentRequestCount: recent,
		RecentWindowDays:   int(modelImpactRecentWindow / (24 * time.Hour)),
	}, nil
}
