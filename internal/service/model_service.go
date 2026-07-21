// Package service additions: model configuration business logic,
// running-status computation, and candidate test orchestration.
package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
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
	db        *gorm.DB
	masterKey []byte
	client    ProviderClient
}

func NewModelService(db *gorm.DB, masterKey []byte, client ProviderClient) *ModelService {
	return &ModelService{db: db, masterKey: masterKey, client: client}
}

type CandidateView struct {
	ID                      uint       `json:"id"`
	ProviderID              uint       `json:"provider_id"`
	ProviderName            string     `json:"provider_name"`
	ProviderModelName       string     `json:"provider_model_name"`
	InputPrice              float64    `json:"input_price"`
	OutputPrice             float64    `json:"output_price"`
	CacheWritePrice         *float64   `json:"cache_write_price"`
	CacheReadPrice          *float64   `json:"cache_read_price"`
	MaxOutput               int        `json:"max_output"`
	SupportsStreaming       bool       `json:"supports_streaming"`
	SupportsFunctionCalling bool       `json:"supports_function_calling"`
	ManagementStatus        int        `json:"management_status"`
	SortOrder               int        `json:"sort_order"`
	VerificationStatus      int        `json:"verification_status"`
	Routable                bool       `json:"routable"`
	LastTestResult          *int       `json:"last_test_result"`
	LastTestDurationMs      *int64     `json:"last_test_duration_ms"`
	LastTestedAt            *time.Time `json:"last_tested_at"`
}

type ModelView struct {
	ID               uint            `json:"id"`
	Name             string          `json:"name"`
	ManagementStatus int             `json:"management_status"`
	RunningStatus    string          `json:"running_status"`
	Candidates       []CandidateView `json:"candidates"`
	CreatedAt        time.Time       `json:"created_at"`
}

// isCandidateRoutable implements the exhaustive routable-candidate list —
// this deliberately does NOT check anything resembling the
// authorized_destination_version/destination_version staleness gate; a
// candidate's mapping validity and a provider key's credential validity are
// different dimensions.
func isCandidateRoutable(c model.ModelCandidate, providerEnabled, providerHasAvailableKey bool) bool {
	if c.ManagementStatus != model.ModelCandidateStatusEnabled {
		return false
	}
	if !providerEnabled || !providerHasAvailableKey {
		return false
	}
	if c.ProviderModelName == "" {
		return false
	}
	return c.VerificationStatus == model.ModelVerificationStatusPassed
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

// providerHasAvailableKey applies the same "available key" rule
// computeRunningStatus uses: enabled + verified + authorized for the
// provider's current destination_version.
func providerHasAvailableKey(keys []model.ProviderKey, destinationVersion int) bool {
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
func buildCandidateView(c model.ModelCandidate, providerName string, routable bool) CandidateView {
	return CandidateView{
		ID: c.ID, ProviderID: c.ProviderID, ProviderName: providerName, ProviderModelName: c.ProviderModelName,
		InputPrice: c.InputPrice, OutputPrice: c.OutputPrice, CacheWritePrice: c.CacheWritePrice, CacheReadPrice: c.CacheReadPrice,
		MaxOutput: c.MaxOutput, SupportsStreaming: c.SupportsStreaming, SupportsFunctionCalling: c.SupportsFunctionCalling,
		ManagementStatus: c.ManagementStatus, SortOrder: c.SortOrder, VerificationStatus: c.VerificationStatus,
		Routable:           routable,
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
			hasAvailableKey = providerHasAvailableKey(keysByProvider[c.ProviderID], c.Provider.DestinationVersion)
		}
		routable := isCandidateRoutable(c, providerEnabled, hasAvailableKey)
		views = append(views, buildCandidateView(c, providerName, routable))
	}
	return ModelView{
		ID: m.ID, Name: m.Name, ManagementStatus: m.ManagementStatus,
		RunningStatus: computeModelRunningStatus(views), Candidates: views, CreatedAt: m.CreatedAt,
	}
}

// keysByProviderForCandidates batches the provider-keys lookup for every
// distinct ProviderID referenced across candidates into a single query
// (repository.ListProviderKeysByProviderIDs), avoiding the N+1 pattern of
// looking up one provider's keys per candidate.
func keysByProviderForCandidates(db *gorm.DB, candidates []model.ModelCandidate) (map[uint][]model.ProviderKey, error) {
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
	keysByProvider, err := keysByProviderForCandidates(s.db, allCandidates)
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
	keysByProvider, err := keysByProviderForCandidates(s.db, candidates)
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
		if isUniqueViolation(err) {
			return nil, errcode.ErrModelNameTaken
		}
		return nil, err
	}
	return s.GetModelDetail(m.ID)
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

// TestCandidateMappingPreview is the stateless, unpersisted preview
// (POST .../candidates/test-mapping) — never writes to the database,
// used by the "add candidate" drawer before the candidate is saved.
// providerModelName is optional — an admin leaving it blank means "use the
// model's own external name upstream unchanged", so it's resolved against
// modelID here before running the test.
func (s *ModelService) TestCandidateMappingPreview(ctx context.Context, modelID, providerID uint, providerModelName, testType string) (TestResult, error) {
	m, err := repository.FindModelByID(s.db, modelID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return TestResult{}, errcode.ErrModelNotFound
		}
		return TestResult{}, err
	}
	if providerModelName == "" {
		providerModelName = m.Name
	}
	provider, err := repository.FindProviderByID(s.db, providerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return TestResult{}, errcode.ErrProviderNotFound
		}
		return TestResult{}, err
	}
	keys, err := repository.ListProviderKeysByProvider(s.db, providerID)
	if err != nil {
		return TestResult{}, err
	}
	plaintext, err := s.decryptHighestPriorityAvailableKey(keys)
	if err != nil {
		return TestResult{}, err
	}
	return s.runCapabilityTest(ctx, testType, provider.BaseURL, plaintext, providerModelName)
}

// decryptHighestPriorityAvailableKey picks the sort_order-first available
// key (enabled+verified+authorized for the provider's current
// destination_version) and decrypts it — candidate tests never touch a key
// that a real request wouldn't itself be allowed to use. ListProviderKeysByProvider
// already orders by sort_order, so the first match here is the
// highest-priority one.
func (s *ModelService) decryptHighestPriorityAvailableKey(keys []model.ProviderKey) (string, error) {
	for _, k := range keys {
		if k.ManagementStatus != model.ProviderKeyStatusEnabled || k.VerificationStatus != model.VerificationStatusPassed {
			continue
		}
		return crypto.Decrypt(s.masterKey, k.EncryptedKey)
	}
	return "", errcode.ErrProviderNoTestableModel
}

func (s *ModelService) runCapabilityTest(ctx context.Context, testType, baseURL, apiKey, providerModelName string) (TestResult, error) {
	switch testType {
	case "basic":
		return s.client.TestChatCompletion(ctx, baseURL, apiKey, providerModelName)
	case "streaming":
		return s.client.TestStreamingCompletion(ctx, baseURL, apiKey, providerModelName)
	case "function_calling":
		return s.client.TestFunctionCalling(ctx, baseURL, apiKey, providerModelName)
	default:
		return TestResult{}, fmt.Errorf("unknown test_type %q", testType)
	}
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
	sortOrder, err := repository.NextCandidateSortOrder(s.db, modelID)
	if err != nil {
		return nil, err
	}
	candidate := &model.ModelCandidate{
		ModelID: modelID, ProviderID: input.ProviderID, ProviderModelName: providerModelName,
		InputPrice: input.InputPrice, OutputPrice: input.OutputPrice,
		CacheWritePrice: input.CacheWritePrice, CacheReadPrice: input.CacheReadPrice, MaxOutput: input.MaxOutput,
		SortOrder: sortOrder, ManagementStatus: model.ModelCandidateStatusDisabled,
		VerificationStatus: model.ModelVerificationStatusUntested, CreatedAt: now, UpdatedAt: now,
	}
	if err := repository.CreateModelCandidate(s.db, candidate); err != nil {
		if isUniqueViolation(err) {
			return nil, errcode.ErrModelCandidateProviderTaken
		}
		return nil, err
	}

	// Server-side re-verification, not trusting the client's preview test
	// result: only requested when
	// the caller asked for the candidate to be enabled; "save as disabled"
	// never triggers this.
	if input.ManagementStatus == model.ModelCandidateStatusEnabled {
		s.reverifyAndCommitNewCandidate(ctx, candidate.ID, input.ProviderID, providerModelName, now)
	}

	reloaded, err := repository.FindModelCandidateByID(s.db, candidate.ID)
	if err != nil {
		return nil, err
	}
	return s.toCandidateView(*reloaded)
}

// reverifyAndCommitNewCandidate runs the real basic-text test outside any
// database transaction (same "test first, then open the transaction" ordering as
// runNewPlaintextTestAndCommit) and commits the result — best-effort: any
// failure to find a provider/key or run the test just leaves the candidate
// at its already-committed Disabled/Untested state, it's not surfaced as
// an error to the caller (the candidate row itself was already saved
// successfully).
func (s *ModelService) reverifyAndCommitNewCandidate(ctx context.Context, candidateID, providerID uint, providerModelName string, now time.Time) {
	provider, err := repository.FindProviderByID(s.db, providerID)
	if err != nil {
		return
	}
	keys, err := repository.ListProviderKeysByProvider(s.db, providerID)
	if err != nil {
		return
	}
	plaintext, err := s.decryptHighestPriorityAvailableKey(keys)
	if err != nil {
		return
	}
	result, err := s.client.TestChatCompletion(ctx, provider.BaseURL, plaintext, providerModelName)
	if err != nil {
		return
	}
	outcomeInt := int(result.Outcome)
	if result.Outcome == TestSuccess {
		if err := repository.CommitModelCandidateBasicTestResult(s.db, candidateID, model.ModelVerificationStatusPassed, &outcomeInt, result.DurationMs, now); err != nil {
			return
		}
		_, _ = repository.SetModelCandidateManagementStatusIfVerified(s.db, candidateID, model.ModelCandidateStatusEnabled, now)
		return
	}
	_ = repository.CommitModelCandidateBasicTestResult(s.db, candidateID, model.ModelVerificationStatusFailed, &outcomeInt, result.DurationMs, now)
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
	hasAvailableKey := providerHasAvailableKey(keys, provider.DestinationVersion)
	routable := isCandidateRoutable(c, provider.ManagementStatus == model.ProviderStatusEnabled, hasAvailableKey)
	view := buildCandidateView(c, provider.Name, routable)
	return &view, nil
}

type UpdateCandidateInput struct {
	ProviderModelName string
	InputPrice        float64
	OutputPrice       float64
	CacheWritePrice   *float64
	CacheReadPrice    *float64
	MaxOutput         int
}

func (s *ModelService) UpdateModelCandidate(id uint, input UpdateCandidateInput, now time.Time) (*CandidateView, error) {
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
	resetVerification := providerModelName != candidate.ProviderModelName
	if err := repository.UpdateModelCandidate(s.db, id, providerModelName, input.InputPrice, input.OutputPrice,
		input.CacheWritePrice, input.CacheReadPrice, input.MaxOutput, resetVerification, now); err != nil {
		return nil, err
	}
	reloaded, err := repository.FindModelCandidateByID(s.db, id)
	if err != nil {
		return nil, err
	}
	return s.toCandidateView(*reloaded)
}

// verifyCandidateEnableAllowed mirrors verifyKeyEnableAllowed: enabling a
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

func (s *ModelService) TestModelCandidate(ctx context.Context, id uint, testType string, now time.Time) (*CandidateView, error) {
	candidate, err := repository.FindModelCandidateByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrModelCandidateNotFound
		}
		return nil, err
	}
	provider, err := repository.FindProviderByID(s.db, candidate.ProviderID)
	if err != nil {
		return nil, err
	}
	keys, err := repository.ListProviderKeysByProvider(s.db, candidate.ProviderID)
	if err != nil {
		return nil, err
	}
	plaintext, err := s.decryptHighestPriorityAvailableKey(keys)
	if err != nil {
		return nil, err
	}
	result, err := s.runCapabilityTest(ctx, testType, provider.BaseURL, plaintext, candidate.ProviderModelName)
	if err != nil {
		return nil, err
	}
	outcomeInt := int(result.Outcome)
	switch testType {
	case "basic":
		verificationStatus := model.ModelVerificationStatusFailed
		if result.Outcome == TestSuccess {
			verificationStatus = model.ModelVerificationStatusPassed
		}
		if err := repository.CommitModelCandidateBasicTestResult(s.db, id, verificationStatus, &outcomeInt, result.DurationMs, now); err != nil {
			return nil, err
		}
	case "streaming", "function_calling":
		if err := repository.CommitModelCandidateCapabilityTestResult(s.db, id, testType, result.Outcome == TestSuccess, &outcomeInt, result.DurationMs, now); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown test_type %q", testType)
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

func (s *ModelService) UpdateModelNameStatus(id uint, name string, now time.Time) (*ModelView, error) {
	if !isValidModelName(name) {
		return nil, fmt.Errorf("%w: model name must contain only letters, digits, dots, hyphens, and underscores", errcode.ErrModelNameTaken)
	}
	m, err := repository.FindModelByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrModelNotFound
		}
		return nil, err
	}
	if err := repository.UpdateModelNameStatus(s.db, id, name, m.ManagementStatus, now); err != nil {
		if isUniqueViolation(err) {
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
	return repository.UpdateModelNameStatus(s.db, id, m.Name, status, now)
}
