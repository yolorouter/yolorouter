package videotask

// Test-store shim: the domain's tests drive it through the same Store
// port production goes through, implemented here straight over the
// repository — imports a test file may carry even though the domain's
// production files may not.

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

func (s *testStore) FindModelByName(ctx context.Context, name string) (*rows.Model, error) {
	m, err := repository.FindModelByName(s.db.WithContext(ctx), name)
	if err != nil {
		return nil, s.notFound(err)
	}
	return &rows.Model{ID: m.ID, Name: m.Name, ManagementStatus: m.ManagementStatus,
		SchedulingMode: rows.SchedulingMode(m.SchedulingMode), OutputModalities: m.OutputModalities}, nil
}

func (s *testStore) ListModelCandidatesByModelID(ctx context.Context, modelID uint) ([]rows.ModelCandidate, error) {
	cs, err := repository.ListModelCandidatesByModelID(s.db.WithContext(ctx), modelID)
	if err != nil {
		return nil, err
	}
	out := make([]rows.ModelCandidate, 0, len(cs))
	for i := range cs {
		c := &cs[i]
		out = append(out, rows.ModelCandidate{ID: c.ID, ModelID: c.ModelID, ProviderID: c.ProviderID,
			ProviderModelName: c.ProviderModelName, InputPrice: c.InputPrice, OutputPrice: c.OutputPrice,
			MaxOutput: c.MaxOutput, BillingMode: c.BillingMode, VideoPricingTiers: c.VideoPricingTiers,
			ManagementStatus: c.ManagementStatus, VerificationStatus: c.VerificationStatus, SortOrder: c.SortOrder})
	}
	return out, nil
}

func (s *testStore) FindModelCandidateByID(ctx context.Context, id uint) (*rows.ModelCandidate, error) {
	c, err := repository.FindModelCandidateByID(s.db.WithContext(ctx), id)
	if err != nil {
		return nil, s.notFound(err)
	}
	return &rows.ModelCandidate{ID: c.ID, ModelID: c.ModelID, ProviderID: c.ProviderID,
		ProviderModelName: c.ProviderModelName, InputPrice: c.InputPrice, OutputPrice: c.OutputPrice,
		MaxOutput: c.MaxOutput, BillingMode: c.BillingMode, VideoPricingTiers: c.VideoPricingTiers,
		ManagementStatus: c.ManagementStatus, VerificationStatus: c.VerificationStatus, SortOrder: c.SortOrder}, nil
}

func (s *testStore) FindAPIKeyByID(ctx context.Context, id uint) (*rows.APIKey, error) {
	k, err := repository.FindAPIKeyByID(s.db.WithContext(ctx), id)
	if err != nil {
		return nil, s.notFound(err)
	}
	return &rows.APIKey{ID: k.ID, UserID: k.UserID, Status: k.Status,
		BudgetLimitMicros: k.BudgetLimitMicros, BudgetSpentMicros: k.BudgetSpentMicros}, nil
}

func (s *testStore) CreateVideoTask(ctx context.Context, task *rows.VideoTask) error {
	return repository.CreateVideoTask(s.db.WithContext(ctx), videoToModel(task))
}

func (s *testStore) FindVideoTaskForOwner(ctx context.Context, apiKeyID uint, id string) (*rows.VideoTask, error) {
	t, err := repository.FindVideoTaskForOwner(s.db.WithContext(ctx), apiKeyID, id)
	if err != nil {
		return nil, s.notFound(err)
	}
	return videoFromModel(t), nil
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
		out = append(out, *videoFromModel(&tasks[i]))
	}
	return out, nil
}

func videoToModel(t *rows.VideoTask) *model.VideoTask {
	return &model.VideoTask{ID: t.ID, APIKeyID: t.APIKeyID, ModelID: t.ModelID, ModelName: t.ModelName,
		CandidateID: t.CandidateID, ProviderID: t.ProviderID, ProviderModelName: t.ProviderModelName,
		ProviderTaskID: t.ProviderTaskID, DestinationVersion: t.DestinationVersion, RequestID: t.RequestID,
		Status: t.Status, ErrorCode: t.ErrorCode, ErrorMessage: t.ErrorMessage, RequestSnapshot: t.RequestSnapshot,
		Size: t.Size, Seconds: t.Seconds, ResultURL: t.ResultURL, CoverURL: t.CoverURL, UsageSeconds: t.UsageSeconds,
		EstimatedMicros: t.EstimatedMicros, Billed: t.Billed, BilledMicros: t.BilledMicros, ExpiresAt: t.ExpiresAt,
		LastPolledAt: t.LastPolledAt, UpstreamSubmittedAt: t.UpstreamSubmittedAt, UpstreamCompletedAt: t.UpstreamCompletedAt,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func videoFromModel(t *model.VideoTask) *rows.VideoTask {
	return &rows.VideoTask{ID: t.ID, APIKeyID: t.APIKeyID, ModelID: t.ModelID, ModelName: t.ModelName,
		CandidateID: t.CandidateID, ProviderID: t.ProviderID, ProviderModelName: t.ProviderModelName,
		ProviderTaskID: t.ProviderTaskID, DestinationVersion: t.DestinationVersion, RequestID: t.RequestID,
		Status: t.Status, ErrorCode: t.ErrorCode, ErrorMessage: t.ErrorMessage, RequestSnapshot: t.RequestSnapshot,
		Size: t.Size, Seconds: t.Seconds, ResultURL: t.ResultURL, CoverURL: t.CoverURL, UsageSeconds: t.UsageSeconds,
		EstimatedMicros: t.EstimatedMicros, Billed: t.Billed, BilledMicros: t.BilledMicros, ExpiresAt: t.ExpiresAt,
		LastPolledAt: t.LastPolledAt, UpstreamSubmittedAt: t.UpstreamSubmittedAt, UpstreamCompletedAt: t.UpstreamCompletedAt,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}
