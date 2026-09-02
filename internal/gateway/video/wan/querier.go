package wan

// The poll side: an implementation of the videotask Querier that asks
// DashScope about one task. It resolves everything a poll needs from the
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
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/videotask"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// Doer is the HTTP surface the querier needs; *http.Client and the
// gateway's own client both satisfy it.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Querier polls DashScope for one task at a time.
type Querier struct {
	DB      *gorm.DB
	Secrets crypto.SecretBox
	Client  Doer
}

// pollTimeout bounds one task query. A poll is a cheap status read; a
// provider that cannot answer it in this window is not worth holding the
// caller's GET open for.
const pollTimeout = 15 * time.Second

// ErrNoUsableKey is a poll that had no key to ask with. It is not a task
// failure — the task keeps its state and the zombie horizon or the
// provider-change hook retires it.
var ErrNoUsableKey = errors.New("no provider key authorized for this destination")

// QueryTask implements videotask.Querier.
func (q *Querier) QueryTask(ctx context.Context, task model.VideoTask) (videotask.QueryResult, error) {
	var provider model.Provider
	if err := q.DB.WithContext(ctx).First(&provider, "id = ?", task.ProviderID).Error; err != nil {
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

	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pollCtx, http.MethodGet,
		OriginOf(provider.BaseURL)+"/api/v1/tasks/"+task.ProviderTaskID, nil)
	if err != nil {
		return videotask.QueryResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+plaintext)
	resp, err := q.Client.Do(req)
	if err != nil {
		return videotask.QueryResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body := readBounded(resp)
	if resp.StatusCode != http.StatusOK {
		return videotask.QueryResult{}, fmt.Errorf("wan task query status %d: %.200s", resp.StatusCode, body)
	}
	obs, biz, perr := ParseTaskResponse(body)
	if perr != nil {
		return videotask.QueryResult{}, perr
	}
	if biz != nil {
		// A business refusal inside a 200: the task itself is refused,
		// which is a terminal observation for the caller, not a poll error.
		return videotask.QueryResult{
			Status: model.VideoTaskFailed, ErrorCode: biz.Code, ErrorMessage: biz.Message,
		}, nil
	}
	return videotask.QueryResult{
		Status:       obs.Status,
		ResultURL:    obs.VideoURL,
		UsageSeconds: obs.UsageSecs,
		ErrorCode:    obs.ErrorCode,
		ErrorMessage: obs.ErrorMessage,
	}, nil
}

// keyFor picks a key authorized for the provider's current destination.
// The first authorized key wins: a poll is a read, any working key asks
// it equally well, and spreading polls across a pool buys nothing.
func (q *Querier) keyFor(ctx context.Context, provider model.Provider) (string, error) {
	keys, err := repository.ListProviderKeysByProvider(q.DB.WithContext(ctx), provider.ID)
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
		plaintext, derr := q.Secrets.Decrypt(k.EncryptedKey)
		if derr != nil {
			continue
		}
		return plaintext, nil
	}
	return "", ErrNoUsableKey
}
