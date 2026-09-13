package osswire

import (
	"context"
	"errors"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway/rows"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"

	"gorm.io/gorm"
)

// VideoStore implements the videotask domain's Store over this
// deployment's repository, converting rows into the kernel vocabulary.
type VideoStore struct{ db *gorm.DB }

// NewVideoStore wires the domain's data port to the repository.
func NewVideoStore(db *gorm.DB) *VideoStore { return &VideoStore{db: db} }

func (s *VideoStore) notFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return rows.ErrNotFound
	}
	return err
}

func (s *VideoStore) FindModelByName(ctx context.Context, name string) (*rows.Model, error) {
	m, err := repository.FindModelByName(s.db.WithContext(ctx), name)
	if err != nil {
		return nil, s.notFound(err)
	}
	out := Model(m)
	return &out, nil
}

func (s *VideoStore) ListModelCandidatesByModelID(ctx context.Context, modelID uint) ([]rows.ModelCandidate, error) {
	cs, err := repository.ListModelCandidatesByModelID(s.db.WithContext(ctx), modelID)
	if err != nil {
		return nil, err
	}
	return Candidates(cs), nil
}

func (s *VideoStore) FindModelCandidateByID(ctx context.Context, id uint) (*rows.ModelCandidate, error) {
	c, err := repository.FindModelCandidateByID(s.db.WithContext(ctx), id)
	if err != nil {
		return nil, s.notFound(err)
	}
	out := Candidate(c)
	return &out, nil
}

func (s *VideoStore) FindAPIKeyByID(ctx context.Context, id uint) (*rows.APIKey, error) {
	k, err := repository.FindAPIKeyByID(s.db.WithContext(ctx), id)
	if err != nil {
		return nil, s.notFound(err)
	}
	return APIKey(k), nil
}

func (s *VideoStore) CreateVideoTask(ctx context.Context, task *rows.VideoTask) error {
	return repository.CreateVideoTask(s.db.WithContext(ctx), rowsVideoToModel(task))
}

func (s *VideoStore) FindVideoTaskForOwner(ctx context.Context, apiKeyID uint, id string) (*rows.VideoTask, error) {
	t, err := repository.FindVideoTaskForOwner(s.db.WithContext(ctx), apiKeyID, id)
	if err != nil {
		return nil, s.notFound(err)
	}
	return rowsVideoFromModel(t), nil
}

func (s *VideoStore) SaveVideoTaskPollResult(ctx context.Context, id string, result map[string]any, now time.Time) (bool, error) {
	return repository.SaveVideoTaskPollResult(s.db.WithContext(ctx), id, result, now)
}

func (s *VideoStore) ClaimVideoTaskPoll(ctx context.Context, apiKeyID uint, id string, prev, next time.Time) (bool, error) {
	return repository.ClaimVideoTaskPoll(s.db.WithContext(ctx), apiKeyID, id, prev, next)
}

func (s *VideoStore) ChargeVideoTask(ctx context.Context, id string, micros int64, now time.Time) (bool, error) {
	return repository.ChargeVideoTask(s.db.WithContext(ctx), id, micros, now)
}

func (s *VideoStore) UpdateRequestLogVideoSettlement(ctx context.Context, requestID string, micros int64, seconds int) error {
	return repository.UpdateRequestLogVideoSettlement(s.db.WithContext(ctx), requestID, micros, seconds)
}

func (s *VideoStore) ExpireStaleVideoTasks(ctx context.Context, now time.Time) (int64, error) {
	return repository.ExpireStaleVideoTasks(s.db.WithContext(ctx), now)
}

func (s *VideoStore) ExpireProviderInFlightVideoTasks(ctx context.Context, providerID uint, newDestinationVersion int, now time.Time) (int64, error) {
	return repository.ExpireProviderInFlightVideoTasks(s.db.WithContext(ctx), providerID, newDestinationVersion, now)
}

func (s *VideoStore) SumInFlightVideoEstimated(ctx context.Context, apiKeyID uint) (int64, error) {
	return repository.SumInFlightVideoEstimated(s.db.WithContext(ctx), apiKeyID)
}

func (s *VideoStore) ListUnbilledCompletedVideoTasks(ctx context.Context) ([]rows.VideoTask, error) {
	tasks, err := repository.ListUnbilledCompletedVideoTasks(s.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]rows.VideoTask, 0, len(tasks))
	for i := range tasks {
		out = append(out, *rowsVideoFromModel(&tasks[i]))
	}
	return out, nil
}

func rowsVideoToModel(t *rows.VideoTask) *model.VideoTask {
	return &model.VideoTask{
		ID: t.ID, APIKeyID: t.APIKeyID, ModelID: t.ModelID, ModelName: t.ModelName,
		CandidateID: t.CandidateID, ProviderID: t.ProviderID, ProviderModelName: t.ProviderModelName,
		ProviderTaskID: t.ProviderTaskID, DestinationVersion: t.DestinationVersion,
		RequestID: t.RequestID, Status: t.Status, ErrorCode: t.ErrorCode, ErrorMessage: t.ErrorMessage,
		RequestSnapshot: t.RequestSnapshot, Size: t.Size, Seconds: t.Seconds,
		ResultURL: t.ResultURL, CoverURL: t.CoverURL, UsageSeconds: t.UsageSeconds,
		EstimatedMicros: t.EstimatedMicros, Billed: t.Billed, BilledMicros: t.BilledMicros,
		ExpiresAt: t.ExpiresAt, LastPolledAt: t.LastPolledAt,
		UpstreamSubmittedAt: t.UpstreamSubmittedAt, UpstreamCompletedAt: t.UpstreamCompletedAt,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func rowsVideoFromModel(t *model.VideoTask) *rows.VideoTask {
	return &rows.VideoTask{
		ID: t.ID, APIKeyID: t.APIKeyID, ModelID: t.ModelID, ModelName: t.ModelName,
		CandidateID: t.CandidateID, ProviderID: t.ProviderID, ProviderModelName: t.ProviderModelName,
		ProviderTaskID: t.ProviderTaskID, DestinationVersion: t.DestinationVersion,
		RequestID: t.RequestID, Status: t.Status, ErrorCode: t.ErrorCode, ErrorMessage: t.ErrorMessage,
		RequestSnapshot: t.RequestSnapshot, Size: t.Size, Seconds: t.Seconds,
		ResultURL: t.ResultURL, CoverURL: t.CoverURL, UsageSeconds: t.UsageSeconds,
		EstimatedMicros: t.EstimatedMicros, Billed: t.Billed, BilledMicros: t.BilledMicros,
		ExpiresAt: t.ExpiresAt, LastPolledAt: t.LastPolledAt,
		UpstreamSubmittedAt: t.UpstreamSubmittedAt, UpstreamCompletedAt: t.UpstreamCompletedAt,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}
