package gateway

// The DashScope poll side: an implementation of the videotask Querier
// that asks about one task. It resolves everything a poll needs from the
// task's own snapshots — which provider, which destination, which task id
// — plus the live provider row and a usable key, and refuses quietly when
// the world moved under the task: a provider whose destination version
// advanced past the task's, or a provider with no key left that could ask.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols/videos"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/videotask"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// videoDoer is the HTTP surface the poller needs; *http.Client and the
// gateway's own client both satisfy it through one adapter.
type videoDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// videoTaskQuerier polls whichever vendor the task's provider speaks,
// one task at a time: the shared half — resolve the provider, check the
// destination version, pick a key — then the dialect's own route and
// parser.
type videoTaskQuerier struct {
	db      *gorm.DB
	secrets crypto.SecretBox
	client  videoDoer
}

// videoPollTimeout bounds one task query. A poll is a cheap status read;
// a provider that cannot answer it in this window is not worth holding
// the caller's GET open for.
const videoPollTimeout = 15 * time.Second

// errNoUsableVideoKey is a poll that had no key to ask with. It is not a
// task failure — the task keeps its state and the zombie horizon or the
// provider-change hook retires it.
var errNoUsableVideoKey = errors.New("no provider key authorized for this destination")

// QueryTask implements videotask.Querier.
func (q *videoTaskQuerier) QueryTask(ctx context.Context, task model.VideoTask) (videotask.QueryResult, error) {
	var provider model.Provider
	if err := q.db.WithContext(ctx).First(&provider, "id = ?", task.ProviderID).Error; err != nil {
		return videotask.QueryResult{}, fmt.Errorf("load provider %d: %w", task.ProviderID, err)
	}
	// A task issued by an older destination cannot be known at the new
	// one; the provider-change hook normally retires these first, and
	// this check is the backstop that never asks a foreign destination.
	if int(provider.DestinationVersion) != task.DestinationVersion {
		return videotask.QueryResult{
			Status: model.VideoTaskExpired, ErrorCode: "provider_destination_changed",
			ErrorMessage: "the provider address changed after this task was submitted",
		}, nil
	}

	plaintext, err := q.keyFor(ctx, provider)
	if err != nil {
		return videotask.QueryResult{}, err
	}

	if isArkBase(provider.BaseURL) {
		return q.pollArk(ctx, provider, task, plaintext)
	}
	return q.pollDashScope(ctx, provider, task, plaintext)
}

// pollDashScope asks the dashscope task route and normalizes its answer.
func (q *videoTaskQuerier) pollDashScope(ctx context.Context, provider model.Provider, task model.VideoTask, plaintext string) (videotask.QueryResult, error) {
	obs, biz, err := q.getTask(ctx, provider, plaintext, task.ProviderTaskID, videos.DashScopeTaskPathPrefix, "dashscope",
		func(body []byte) (taskObservation, *videos.Refusal, error) {
			parsed, biz, perr := videos.ParseDashScopeTaskResponse(body)
			return taskObservation{Status: parsed.Status, VideoURL: parsed.VideoURL, UsageSecs: parsed.UsageSecs, ErrorCode: parsed.ErrorCode, ErrorMessage: parsed.ErrorMessage}, refusal(biz), perr
		})
	if err != nil {
		return videotask.QueryResult{}, err
	}
	if biz != nil {
		// A business refusal inside a 200: the task itself is refused,
		// which is a terminal observation for the caller, not a poll error.
		return videotask.QueryResult{
			Status: model.VideoTaskFailed, ErrorCode: biz.Code, ErrorMessage: biz.Message,
		}, nil
	}
	return obs.result(), nil
}

// pollArk asks the Ark task route and normalizes its answer.
func (q *videoTaskQuerier) pollArk(ctx context.Context, provider model.Provider, task model.VideoTask, plaintext string) (videotask.QueryResult, error) {
	obs, _, err := q.getTask(ctx, provider, plaintext, task.ProviderTaskID, videos.ArkTaskPathPrefix, "ark",
		func(body []byte) (taskObservation, *videos.Refusal, error) {
			parsed, perr := videos.ParseArkTaskResponse(body)
			return taskObservation{Status: parsed.Status, VideoURL: parsed.VideoURL, UsageSecs: parsed.UsageSecs, ErrorCode: parsed.ErrorCode, ErrorMessage: parsed.ErrorMessage}, nil, perr
		})
	if err != nil {
		return videotask.QueryResult{}, err
	}
	// Ark reports no seconds field of its own — the billable duration is
	// the task's echo of what was asked. A completion that arrives
	// without the echo still bills what this dialect stated in the
	// submit: asking for a duration and forgetting it upstream-side is
	// not a reason to bill zero for a delivered video.
	if obs.UsageSecs == 0 {
		obs.UsageSecs = task.Seconds
	}
	return obs.result(), nil
}

// taskObservation is one poll's normalized answer before it becomes the
// querier's result — the shape both vendors' parsers reduce to, so the
// GET skeleton below stays dialect-free.
type taskObservation struct {
	Status       string
	VideoURL     string
	UsageSecs    int
	ErrorCode    string
	ErrorMessage string
}

func (o taskObservation) result() videotask.QueryResult {
	return videotask.QueryResult{
		Status: o.Status, ResultURL: o.VideoURL, UsageSeconds: o.UsageSecs,
		ErrorCode: o.ErrorCode, ErrorMessage: o.ErrorMessage,
	}
}

// refusal adapts the dashscope parse's own refusal type onto the one
// shared face; a nil stays nil so "no refusal" keeps its meaning.
func refusal(biz *videos.DashScopeBizError) *videos.Refusal {
	if biz == nil {
		return nil
	}
	return &videos.Refusal{Code: biz.Code, Message: biz.Message}
}

// getTask is the GET-and-read skeleton every task dialect shares: one
// bounded, time-boxed, bearer-authenticated status read against the
// provider's origin, handed to the dialect's parser. The two vendors'
// polls differed only in route and parser; a third would have copied the
// skeleton again.
func (q *videoTaskQuerier) getTask(ctx context.Context, provider model.Provider, plaintext, taskID, pathPrefix, vendor string,
	parse func(body []byte) (taskObservation, *videos.Refusal, error),
) (taskObservation, *videos.Refusal, error) {
	pollCtx, cancel := context.WithTimeout(ctx, videoPollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pollCtx, http.MethodGet,
		videos.Origin(provider.BaseURL)+pathPrefix+taskID, nil)
	if err != nil {
		return taskObservation{}, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+plaintext)
	resp, err := q.client.Do(req)
	if err != nil {
		return taskObservation{}, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body := videos.ReadTaskBounded(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return taskObservation{}, nil, fmt.Errorf("%s task query status %d: %.200s", vendor, resp.StatusCode, body)
	}
	return parse(body)
}

// keyFor picks a key authorized for the provider's current destination.
// The first authorized key wins: a poll is a read, any working key asks
// it equally well, and spreading polls across a pool buys nothing.
func (q *videoTaskQuerier) keyFor(ctx context.Context, provider model.Provider) (string, error) {
	keys, err := repository.ListProviderKeysByProvider(q.db.WithContext(ctx), provider.ID)
	if err != nil {
		return "", err
	}
	for i := range keys {
		k := &keys[i]
		if k.ManagementStatus != model.ProviderKeyStatusEnabled {
			continue
		}
		if k.AuthorizedDestinationVersion != provider.DestinationVersion {
			continue
		}
		plaintext, derr := q.secrets.Decrypt(k.EncryptedKey)
		if derr != nil {
			continue
		}
		return plaintext, nil
	}
	return "", errNoUsableVideoKey
}
