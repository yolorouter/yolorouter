package gateway

// Test-side wiring of the real video job domain: the kernel's own tests
// exercise the actual Create/budget/settle machinery, so newSvc installs
// this the way the router assembly installs its adapter. A test file
// may import the storage-facing model package; production files may not.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway/rows"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/videotask"
	"github.com/yolorouter/yolorouter/pkg/crypto"

	"gorm.io/gorm"
)

func videoTaskToModelForTest(t *VideoTask) *model.VideoTask {
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

func videoTaskToRowsForTest(t *model.VideoTask) *VideoTask {
	return &VideoTask{
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

type testVideoTasks struct{ svc *videotask.Service }

func (v testVideoTasks) Create(ctx context.Context, task *VideoTask, now time.Time) error {
	m := videoTaskToModelForTest(task)
	if err := v.svc.Create(ctx, m, now); err != nil {
		return translateVideoErr(err)
	}
	// The domain mints the job id, the expiry horizon, and the price
	// estimate inside Create — write them back so the caller's task is
	// the one the caller can poll.
	task.ID = m.ID
	task.Status = m.Status
	task.ExpiresAt = m.ExpiresAt
	task.EstimatedMicros = m.EstimatedMicros
	task.UpstreamSubmittedAt = m.UpstreamSubmittedAt
	task.CreatedAt = m.CreatedAt
	task.UpdatedAt = m.UpdatedAt
	return nil
}

func (v testVideoTasks) PrecheckBudget(ctx context.Context, apiKeyID uint, modelName, size string, seconds int) error {
	return translateVideoErr(v.svc.PrecheckBudget(ctx, apiKeyID, modelName, size, seconds))
}

func (v testVideoTasks) Get(ctx context.Context, apiKeyID uint, id string, now time.Time) (*VideoTask, error) {
	t, err := v.svc.Get(ctx, apiKeyID, id, now)
	if err != nil {
		return nil, translateVideoErr(err)
	}
	return videoTaskToRowsForTest(t), nil
}

// testVideoQuerierAdapter adapts the kernel poller to the domain's
// Querier, converting the task row in and the result out.
type testVideoQuerierAdapter struct{ poller *UpstreamVideoTaskPoller }

func (a testVideoQuerierAdapter) QueryTask(ctx context.Context, task model.VideoTask) (videotask.QueryResult, error) {
	res, err := a.poller.PollTask(ctx, *videoTaskToRowsForTest(&task))
	if err != nil {
		return videotask.QueryResult{}, err
	}
	return videotask.QueryResult{
		Status:       res.Status,
		ResultURL:    res.ResultURL,
		CoverURL:     res.CoverURL,
		UsageSeconds: res.UsageSeconds,
		ErrorCode:    res.ErrorCode,
		ErrorMessage: res.ErrorMessage,
	}, nil
}

// newTestVideoTasks builds the real domain over the test database; the
// poller rides the service's own client so polls use the same
// loopback-enabled transport the tests swap in.
func newTestVideoTasks(t *testing.T, db *gorm.DB, secrets crypto.SecretBox, store Store, client *UpstreamClient) testVideoTasks {
	t.Helper()
	poller := NewVideoTaskPoller(store, secrets, client)
	return testVideoTasks{svc: videotask.NewService(db, testVideoQuerierAdapter{poller: poller})}
}

// translateVideoErr maps the domain's own error types onto the kernel
// vocabulary's, so the kernel's errors.As/Is checks see what they expect
// through the port.
func translateVideoErr(err error) error {
	var budget *videotask.BudgetExceededError
	if errors.As(err, &budget) {
		return &rows.BudgetExceededError{Limit: budget.Limit, Spent: budget.Spent, InFlight: budget.InFlight, Ask: budget.Ask}
	}
	if errors.Is(err, repository.ErrVideoTaskNotFound) {
		return rows.ErrVideoTaskNotFound
	}
	return err
}
