package gateway

// The Kling image poller: the sync wrapper's engine. A Kling image submit
// answers with a task id, while the caller asked in the OpenAI images
// shape and expects one synchronous answer — so the delivery side drives
// the task to its terminal state here, inside the request's own budget,
// and hands the terminal observation back for shaping.
//
// Once a task is accepted upstream it renders and bills whether or not
// this poll finishes, so a poll that fails mid-flight is reported as an
// error for the caller, never as a candidate failure: failover would
// submit a second billable task for a request whose first one is already
// rendering.

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/images"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// The poll's own pacing: a status read is cheap and images finish in
// seconds-to-minutes, so a short interval rides the tail quickly while
// the deadline keeps the whole exchange inside the images modality's
// transfer budget with room for the submit that preceded it; the single
// read's own bound is the shared videoPollTimeout.
const (
	klingImagePollInterval = 2 * time.Second
	klingImagePollDeadline = 8 * time.Minute
)

// klingImagePoller is the delivery-side task driver, wired by NewService.
type klingImagePoller struct {
	db      *gorm.DB
	secrets crypto.SecretBox
	client  taskDoer
}

// klingImagePoll is the wired poller; nil in a bare assembly, where a
// Kling delivery answers a gateway error rather than dialing out.
var klingImagePoll *klingImagePoller

// Poll drives one task to its terminal state and returns the raw body of
// the terminal observation beside its parse — that body is the upstream
// response this delivery is actually decided by, and the audit trail
// records it as such. A business refusal inside a 200 comes back behind
// its own face; everything else that goes wrong is an error the caller is
// told about, not a failover signal.
func (p *klingImagePoller) Poll(ctx context.Context, providerID uint, destinationVersion int, taskID, taskPathPrefix string) (images.KlingImageTask, []byte, *images.KlingImageBizError, error) {
	var provider model.Provider
	if err := p.db.WithContext(ctx).First(&provider, "id = ?", providerID).Error; err != nil {
		return images.KlingImageTask{}, nil, nil, fmt.Errorf("load provider %d: %w", providerID, err)
	}
	if int(provider.DestinationVersion) != destinationVersion {
		return images.KlingImageTask{}, nil, nil, fmt.Errorf("the provider address changed under the image task")
	}
	plaintext, err := authorizedTaskKey(ctx, p.db, p.secrets, provider)
	if err != nil {
		return images.KlingImageTask{}, nil, nil, err
	}

	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(klingImagePollDeadline))
	defer cancel()
	for {
		task, body, biz, perr := p.getTask(ctx, provider, plaintext, taskID, taskPathPrefix)
		if perr != nil {
			return images.KlingImageTask{}, nil, nil, perr
		}
		if biz != nil {
			return images.KlingImageTask{}, body, biz, nil
		}
		if task.Terminal {
			return task, body, nil, nil
		}
		select {
		case <-ctx.Done():
			return images.KlingImageTask{}, nil, nil, fmt.Errorf("kling image task did not finish within %s", klingImagePollDeadline)
		case <-time.After(klingImagePollInterval):
		}
	}
}

// getTask performs one bounded status read and parses it in the dialect,
// handing the raw body back beside the parse.
func (p *klingImagePoller) getTask(ctx context.Context, provider model.Provider, plaintext, taskID, taskPathPrefix string) (images.KlingImageTask, []byte, *images.KlingImageBizError, error) {
	body, err := fetchTaskBounded(ctx, p.client, protocols.OriginURL(provider.BaseURL, taskPathPrefix+taskID), plaintext, "kling image")
	if err != nil {
		return images.KlingImageTask{}, nil, nil, err
	}
	task, biz, perr := images.ParseKlingImageTaskResponse(body)
	return task, body, biz, perr
}
