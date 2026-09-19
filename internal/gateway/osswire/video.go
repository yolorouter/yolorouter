package osswire

import (
	"context"
	"errors"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/gateway/rows"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/videotask"
)

// VideoTasks adapts the video job domain to the kernel's collaborator
// port, converting task rows both ways at the boundary.
type VideoTasks struct {
	svc *videotask.Service
}

// NewVideoTasks wraps the domain service as the kernel port.
func NewVideoTasks(svc *videotask.Service) *VideoTasks {
	return &VideoTasks{svc: svc}
}

func (v *VideoTasks) Create(ctx context.Context, task *rows.VideoTask, now time.Time) error {
	if err := v.svc.Create(ctx, task, now); err != nil {
		return translateVideoErr(err)
	}
	// The domain mutates the task row in place (id minting, expiry
	// horizon, price estimate); the caller's rows.VideoTask is the same
	// object, so no writeback is needed.
	return nil
}

func (v *VideoTasks) PrecheckBudget(ctx context.Context, apiKeyID uint, modelName, size string, seconds int) error {
	return translateVideoErr(v.svc.PrecheckBudget(ctx, apiKeyID, modelName, size, seconds))
}

func (v *VideoTasks) Get(ctx context.Context, apiKeyID uint, id string, now time.Time) (*rows.VideoTask, error) {
	return v.svc.Get(ctx, apiKeyID, id, now)
}

// QuerierAdapter turns the kernel's poller into the domain service's
// Querier, converting the task row in and the query result out.
type QuerierAdapter struct {
	Poller *gateway.UpstreamVideoTaskPoller
}

// QueryTask implements videotask.Querier.
func (a QuerierAdapter) QueryTask(ctx context.Context, task rows.VideoTask) (videotask.QueryResult, error) {
	res, err := a.Poller.PollTask(ctx, task)
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
