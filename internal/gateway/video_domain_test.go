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
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/videotask"
	"github.com/yolorouter/yolorouter/pkg/crypto"

	"gorm.io/gorm"
)

type testVideoTasks struct{ svc *videotask.Service }

func (v testVideoTasks) Create(ctx context.Context, task *VideoTask, now time.Time) error {
	if err := v.svc.Create(ctx, task, now); err != nil {
		return translateVideoErr(err)
	}
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
	return t, nil
}

// testVideoQuerierAdapter adapts the kernel poller to the domain's
// Querier, converting the task row in and the result out.
type testVideoQuerierAdapter struct{ poller *UpstreamVideoTaskPoller }

func (a testVideoQuerierAdapter) QueryTask(ctx context.Context, task rows.VideoTask) (videotask.QueryResult, error) {
	res, err := a.poller.PollTask(ctx, task)
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
	return testVideoTasks{svc: videotask.NewService(testVideoStoreFrom(db), testVideoQuerierAdapter{poller: poller})}
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

// testVideoStoreFrom builds a videotask Store over the test database so
// the real domain (Create/budget/settle) runs in the kernel's own tests.
type testVideoStore struct{ inner *testStore }

func testVideoStoreFrom(db *gorm.DB) *testVideoStore {
	return &testVideoStore{inner: testStoreFrom(db)}
}

func (s *testVideoStore) FindModelByName(ctx context.Context, name string) (*rows.Model, error) {
	return s.inner.FindModelByName(ctx, name)
}
func (s *testVideoStore) ListModelCandidatesByModelID(ctx context.Context, modelID uint) ([]rows.ModelCandidate, error) {
	return s.inner.ListModelCandidatesByModelID(ctx, modelID)
}
func (s *testVideoStore) FindModelCandidateByID(ctx context.Context, id uint) (*rows.ModelCandidate, error) {
	return s.inner.FindModelCandidateByID(ctx, id)
}
func (s *testVideoStore) FindAPIKeyByID(ctx context.Context, id uint) (*rows.APIKey, error) {
	return s.inner.FindAPIKeyByID(ctx, id)
}
func (s *testVideoStore) CreateVideoTask(ctx context.Context, task *rows.VideoTask) error {
	return s.inner.CreateVideoTask(ctx, task)
}
func (s *testVideoStore) FindVideoTaskForOwner(ctx context.Context, apiKeyID uint, id string) (*rows.VideoTask, error) {
	return s.inner.FindVideoTaskForOwner(ctx, apiKeyID, id)
}
func (s *testVideoStore) SaveVideoTaskPollResult(ctx context.Context, id string, result map[string]any, now time.Time) (bool, error) {
	return s.inner.SaveVideoTaskPollResult(ctx, id, result, now)
}
func (s *testVideoStore) ClaimVideoTaskPoll(ctx context.Context, apiKeyID uint, id string, prev, next time.Time) (bool, error) {
	return s.inner.ClaimVideoTaskPoll(ctx, apiKeyID, id, prev, next)
}
func (s *testVideoStore) ChargeVideoTask(ctx context.Context, id string, micros int64, now time.Time) (bool, error) {
	return s.inner.ChargeVideoTask(ctx, id, micros, now)
}
func (s *testVideoStore) UpdateRequestLogVideoSettlement(ctx context.Context, requestID string, micros int64, seconds int) error {
	return s.inner.UpdateRequestLogVideoSettlement(ctx, requestID, micros, seconds)
}
func (s *testVideoStore) ExpireStaleVideoTasks(ctx context.Context, now time.Time) (int64, error) {
	return s.inner.ExpireStaleVideoTasks(ctx, now)
}
func (s *testVideoStore) ExpireProviderInFlightVideoTasks(ctx context.Context, providerID uint, newDestinationVersion int, now time.Time) (int64, error) {
	return s.inner.ExpireProviderInFlightVideoTasks(ctx, providerID, newDestinationVersion, now)
}
func (s *testVideoStore) SumInFlightVideoEstimated(ctx context.Context, apiKeyID uint) (int64, error) {
	return s.inner.SumInFlightVideoEstimated(ctx, apiKeyID)
}
func (s *testVideoStore) ListUnbilledCompletedVideoTasks(ctx context.Context) ([]rows.VideoTask, error) {
	return s.inner.ListUnbilledCompletedVideoTasks(ctx)
}
