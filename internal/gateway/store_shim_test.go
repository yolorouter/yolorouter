package gateway

// Test-store shim: the kernel's own tests drive it through the same
// Store port production goes through, implemented here straight over
// the repository and the storage-facing model types — imports a test
// file may carry even though the kernel's production files may not.

import (
	"context"
	"errors"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway/rows"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"

	"gorm.io/gorm"
)

type testStore struct{ db *gorm.DB }

func testStoreFrom(db *gorm.DB) *testStore { return &testStore{db: db} }

func (s *testStore) notFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return rows.ErrNotFound
	}
	return err
}

func (s *testStore) FindModelByName(ctx context.Context, name string) (*Model, error) {
	m, err := repository.FindModelByName(s.db.WithContext(ctx), name)
	if err != nil {
		return nil, s.notFound(err)
	}
	return &Model{
		ID: m.ID, Name: m.Name, ManagementStatus: m.ManagementStatus,
		SchedulingMode: SchedulingMode(m.SchedulingMode), SupportsImageInput: m.SupportsImageInput,
		OutputModalities: m.OutputModalities, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}, nil
}

func (s *testStore) ListModels(ctx context.Context) ([]Model, error) {
	all, err := repository.ListModels(s.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(all))
	for i := range all {
		m := &all[i]
		out = append(out, Model{
			ID: m.ID, Name: m.Name, ManagementStatus: m.ManagementStatus,
			SchedulingMode: SchedulingMode(m.SchedulingMode), SupportsImageInput: m.SupportsImageInput,
			OutputModalities: m.OutputModalities, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		})
	}
	return out, nil
}

func (s *testStore) FindAPIKeyByID(ctx context.Context, id uint) (*APIKey, error) {
	k, err := repository.FindAPIKeyByID(s.db.WithContext(ctx), id)
	if err != nil {
		return nil, s.notFound(err)
	}
	return &APIKey{
		ID: k.ID, UserID: k.UserID, Status: k.Status, ExpiresAt: k.ExpiresAt,
		RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit, ConcurrencyLimit: k.ConcurrencyLimit,
		AllowAllModels: k.AllowAllModels, BudgetLimitMicros: k.BudgetLimitMicros, BudgetSpentMicros: k.BudgetSpentMicros,
		CustomSystemPromptEnabledOverride: k.CustomSystemPromptEnabledOverride,
		CustomSystemPromptEnabled:         k.CustomSystemPromptEnabled,
		CustomSystemPrompt:                k.CustomSystemPrompt,
		CompressEnabledOverride:           k.CompressEnabledOverride,
		CompressEnabled:                   k.CompressEnabled,
	}, nil
}

func (s *testStore) FindAPIKeyModelIDs(ctx context.Context, apiKeyID uint) ([]uint, error) {
	return repository.FindAPIKeyModelIDs(s.db.WithContext(ctx), apiKeyID)
}

func (s *testStore) HasAPIKeyModelAccess(ctx context.Context, apiKeyID, modelID uint) (bool, error) {
	return repository.HasAPIKeyModelAccess(s.db.WithContext(ctx), apiKeyID, modelID)
}

func (s *testStore) ListModelCandidatesByModelID(ctx context.Context, modelID uint) ([]ModelCandidate, error) {
	cs, err := repository.ListModelCandidatesByModelID(s.db.WithContext(ctx), modelID)
	if err != nil {
		return nil, err
	}
	out := make([]ModelCandidate, 0, len(cs))
	for i := range cs {
		c := &cs[i]
		var prov *Provider
		if c.Provider != nil {
			prov = &Provider{
				ID: c.Provider.ID, Name: c.Provider.Name, ProviderType: c.Provider.ProviderType,
				BaseURL: c.Provider.BaseURL, ProtocolEndpoints: c.Provider.ProtocolEndpoints,
				ManagementStatus: c.Provider.ManagementStatus, DestinationVersion: c.Provider.DestinationVersion,
				CreatedAt: c.Provider.CreatedAt, UpdatedAt: c.Provider.UpdatedAt,
			}
		}
		out = append(out, ModelCandidate{
			ID: c.ID, ModelID: c.ModelID, ProviderID: c.ProviderID, ProviderModelName: c.ProviderModelName,
			Provider: prov, InputPrice: c.InputPrice, OutputPrice: c.OutputPrice,
			CacheWritePrice: c.CacheWritePrice, CacheReadPrice: c.CacheReadPrice, MaxOutput: c.MaxOutput,
			BillingMode: c.BillingMode, ImagePricingTiers: c.ImagePricingTiers, VideoPricingTiers: c.VideoPricingTiers,
			AudioUnitPrice: c.AudioUnitPrice, SupportsStreaming: c.SupportsStreaming,
			SupportsFunctionCalling: c.SupportsFunctionCalling, ManagementStatus: c.ManagementStatus,
			VerificationStatus: c.VerificationStatus, SortOrder: c.SortOrder,
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		})
	}
	return out, nil
}

func (s *testStore) ListProviderKeysByProvider(ctx context.Context, providerID uint) ([]ProviderKey, error) {
	ks, err := repository.ListProviderKeysByProvider(s.db.WithContext(ctx), providerID)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderKey, 0, len(ks))
	for i := range ks {
		k := &ks[i]
		out = append(out, ProviderKey{
			ID: k.ID, ProviderID: k.ProviderID, Label: k.Label, EncryptedKey: k.EncryptedKey,
			SortOrder: k.SortOrder, ManagementStatus: k.ManagementStatus, VerificationStatus: k.VerificationStatus,
			AuthorizedDestinationVersion: k.AuthorizedDestinationVersion, ConfigVersion: k.ConfigVersion,
			TestGeneration: k.TestGeneration,
		})
	}
	return out, nil
}

func (s *testStore) FindProviderByID(ctx context.Context, id uint) (*Provider, error) {
	var p model.Provider
	if err := s.db.WithContext(ctx).First(&p, "id = ?", id).Error; err != nil {
		return nil, s.notFound(err)
	}
	return &Provider{
		ID: p.ID, Name: p.Name, ProviderType: p.ProviderType, BaseURL: p.BaseURL,
		ProtocolEndpoints: p.ProtocolEndpoints, ManagementStatus: p.ManagementStatus,
		DestinationVersion: p.DestinationVersion, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}, nil
}

func (s *testStore) FindProviderKeyByID(ctx context.Context, id uint) (*ProviderKey, error) {
	var k model.ProviderKey
	if err := s.db.WithContext(ctx).First(&k, "id = ?", id).Error; err != nil {
		return nil, s.notFound(err)
	}
	return &ProviderKey{
		ID: k.ID, ProviderID: k.ProviderID, Label: k.Label, EncryptedKey: k.EncryptedKey,
		SortOrder: k.SortOrder, ManagementStatus: k.ManagementStatus, VerificationStatus: k.VerificationStatus,
		AuthorizedDestinationVersion: k.AuthorizedDestinationVersion, ConfigVersion: k.ConfigVersion,
		TestGeneration: k.TestGeneration,
	}, nil
}

func (s *testStore) MarkProviderKeyVerificationFailedIfCurrent(ctx context.Context, keyID uint, providerDestinationVersion, configVersion, testGeneration int, now time.Time) (bool, error) {
	return repository.MarkProviderKeyVerificationFailedIfCurrent(s.db.WithContext(ctx), keyID, providerDestinationVersion, configVersion, testGeneration, now)
}

func (s *testStore) UpsertObservedRateLimit(ctx context.Context, keyID uint, meter string, limit *int64, windowSecs *int64, remaining *int64, resetAt *time.Time, now time.Time) error {
	return repository.UpsertObservedRateLimit(s.db.WithContext(ctx), keyID, meter, limit, windowSecs, remaining, resetAt, now)
}

func (s *testStore) IncrementAPIKeyBudgetSpent(ctx context.Context, apiKeyID uint, micros int64) error {
	return repository.IncrementAPIKeyBudgetSpent(s.db, apiKeyID, micros)
}

// stubVideoTasks answers nothing: tests that need the video domain
// install their own into the package seam.
type stubVideoTasks struct{}

func (stubVideoTasks) Create(context.Context, *VideoTask, time.Time) error { return nil }
func (stubVideoTasks) PrecheckBudget(context.Context, uint, string, string, int) error {
	return nil
}
func (stubVideoTasks) Get(context.Context, uint, string, time.Time) (*VideoTask, error) {
	return nil, ErrVideoTaskNotFound
}

// Video-domain methods the test video store delegates to.

func (s *testStore) FindModelCandidateByID(ctx context.Context, id uint) (*ModelCandidate, error) {
	c, err := repository.FindModelCandidateByID(s.db.WithContext(ctx), id)
	if err != nil {
		return nil, s.notFound(err)
	}
	out := ModelCandidate{ID: c.ID, ModelID: c.ModelID, ProviderID: c.ProviderID,
		ProviderModelName: c.ProviderModelName, InputPrice: c.InputPrice, OutputPrice: c.OutputPrice,
		MaxOutput: c.MaxOutput, BillingMode: c.BillingMode, VideoPricingTiers: c.VideoPricingTiers,
		ManagementStatus: c.ManagementStatus, VerificationStatus: c.VerificationStatus, SortOrder: c.SortOrder}
	return &out, nil
}

func (s *testStore) CreateVideoTask(ctx context.Context, task *rows.VideoTask) error {
	return repository.CreateVideoTask(s.db.WithContext(ctx), tsVideoToModel(task))
}

func (s *testStore) FindVideoTaskForOwner(ctx context.Context, apiKeyID uint, id string) (*rows.VideoTask, error) {
	t, err := repository.FindVideoTaskForOwner(s.db.WithContext(ctx), apiKeyID, id)
	if err != nil {
		return nil, s.notFound(err)
	}
	return tsVideoFromModel(t), nil
}

func (s *testStore) SaveVideoTaskPollResult(ctx context.Context, id string, result map[string]any, now time.Time) (bool, error) {
	return repository.SaveVideoTaskPollResult(s.db.WithContext(ctx), id, result, now)
}

func (s *testStore) ClaimVideoTaskPoll(ctx context.Context, apiKeyID uint, id string, prev, next time.Time) (bool, error) {
	return repository.ClaimVideoTaskPoll(s.db.WithContext(ctx), apiKeyID, id, prev, next)
}

func (s *testStore) ChargeVideoTask(ctx context.Context, id string, micros int64, now time.Time) (bool, error) {
	return repository.ChargeVideoTask(s.db.WithContext(ctx), id, micros, now)
}

func (s *testStore) UpdateRequestLogVideoSettlement(ctx context.Context, requestID string, micros int64, seconds int) error {
	return repository.UpdateRequestLogVideoSettlement(s.db.WithContext(ctx), requestID, micros, seconds)
}

func (s *testStore) ExpireStaleVideoTasks(ctx context.Context, now time.Time) (int64, error) {
	return repository.ExpireStaleVideoTasks(s.db.WithContext(ctx), now)
}

func (s *testStore) ExpireProviderInFlightVideoTasks(ctx context.Context, providerID uint, newDestinationVersion int, now time.Time) (int64, error) {
	return repository.ExpireProviderInFlightVideoTasks(s.db.WithContext(ctx), providerID, newDestinationVersion, now)
}

func (s *testStore) SumInFlightVideoEstimated(ctx context.Context, apiKeyID uint) (int64, error) {
	return repository.SumInFlightVideoEstimated(s.db.WithContext(ctx), apiKeyID)
}

func (s *testStore) ListUnbilledCompletedVideoTasks(ctx context.Context) ([]rows.VideoTask, error) {
	tasks, err := repository.ListUnbilledCompletedVideoTasks(s.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]rows.VideoTask, 0, len(tasks))
	for i := range tasks {
		out = append(out, *tsVideoFromModel(&tasks[i]))
	}
	return out, nil
}

func tsVideoToModel(t *rows.VideoTask) *model.VideoTask {
	return &model.VideoTask{ID: t.ID, APIKeyID: t.APIKeyID, ModelID: t.ModelID, ModelName: t.ModelName,
		CandidateID: t.CandidateID, ProviderID: t.ProviderID, ProviderModelName: t.ProviderModelName,
		ProviderTaskID: t.ProviderTaskID, DestinationVersion: t.DestinationVersion, RequestID: t.RequestID,
		Status: t.Status, ErrorCode: t.ErrorCode, ErrorMessage: t.ErrorMessage, RequestSnapshot: t.RequestSnapshot,
		Size: t.Size, Seconds: t.Seconds, ResultURL: t.ResultURL, CoverURL: t.CoverURL, UsageSeconds: t.UsageSeconds,
		EstimatedMicros: t.EstimatedMicros, Billed: t.Billed, BilledMicros: t.BilledMicros, ExpiresAt: t.ExpiresAt,
		LastPolledAt: t.LastPolledAt, UpstreamSubmittedAt: t.UpstreamSubmittedAt, UpstreamCompletedAt: t.UpstreamCompletedAt,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func tsVideoFromModel(t *model.VideoTask) *rows.VideoTask {
	return &rows.VideoTask{ID: t.ID, APIKeyID: t.APIKeyID, ModelID: t.ModelID, ModelName: t.ModelName,
		CandidateID: t.CandidateID, ProviderID: t.ProviderID, ProviderModelName: t.ProviderModelName,
		ProviderTaskID: t.ProviderTaskID, DestinationVersion: t.DestinationVersion, RequestID: t.RequestID,
		Status: t.Status, ErrorCode: t.ErrorCode, ErrorMessage: t.ErrorMessage, RequestSnapshot: t.RequestSnapshot,
		Size: t.Size, Seconds: t.Seconds, ResultURL: t.ResultURL, CoverURL: t.CoverURL, UsageSeconds: t.UsageSeconds,
		EstimatedMicros: t.EstimatedMicros, Billed: t.Billed, BilledMicros: t.BilledMicros, ExpiresAt: t.ExpiresAt,
		LastPolledAt: t.LastPolledAt, UpstreamSubmittedAt: t.UpstreamSubmittedAt, UpstreamCompletedAt: t.UpstreamCompletedAt,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}
