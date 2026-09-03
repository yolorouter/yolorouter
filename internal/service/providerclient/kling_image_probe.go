package providerclient

// The Kling image probe. Kling's image endpoint is a task dialect, so the
// probe proves the whole conversation a routed request actually runs: a
// submit the upstream accepts, and the task driven to its terminal state
// — one real generation, billed to the account, exactly like the video
// probe's shortest-task stance. The probe asks for the endpoint's own
// defaults (one image, no size) because they are the cheapest ask the
// dialect can state.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/images"
)

// The probe's pacing: images finish in seconds-to-minutes; the probe waits
// a task out (klingImageProbeDeadline) rather than judging it mid-flight.
const (
	klingImageProbeInterval = 2 * time.Second
	klingImageProbeDeadline = 3 * time.Minute
	klingImageProbeTimeout  = 15 * time.Second
)

func (c *HTTPProviderClient) testKlingImageGeneration(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	submitBody, err := images.EncodeKlingImageRequest(imageProbePrompt, model, 1, "", nil)
	if err != nil {
		return TestResult{}, fmt.Errorf("encode kling image probe: %w", err)
	}
	return c.runRawTestRequestAt(ctx, protocols.OriginURL(baseURL, images.KlingImageSubmitPathFor(model)),
		protocols.ProtocolOpenAI, apiKey, "application/json", submitBody, nil,
		func(resp *http.Response, duration int64) (TestResult, error) {
			body, ok := readBoundedBody(resp)
			if !ok {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
			}
			if resp.StatusCode != http.StatusOK {
				return classifyResponse(protocols.ProtocolOpenAI, resp, body, model, duration), nil
			}
			taskID, refuse, perr := images.ParseKlingImageSubmitResponse(body)
			if perr != nil {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration, Detail: "submit body: " + perr.Error()}, nil
			}
			if refuse != nil {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration,
					Detail: fmt.Sprintf("HTTP 200: kling error %s: %s", refuse.Code, refuse.Message)}, nil
			}
			// The submit half passed; the query half drives the task it
			// created to a terminal state — a finished generation is the
			// protocol working end to end.
			return c.pollKlingImageProbeTask(ctx, baseURL, apiKey, model, taskID, duration)
		})
}

// pollKlingImageProbeTask drives one probe task to its terminal state.
func (c *HTTPProviderClient) pollKlingImageProbeTask(ctx context.Context, baseURL, apiKey, model, taskID string, submitDuration int64) (TestResult, error) {
	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(klingImageProbeDeadline))
	defer cancel()
	for {
		done, detail := c.oneKlingImageProbeQuery(ctx, baseURL, apiKey, model, taskID)
		if done {
			if detail == "" {
				return TestResult{Outcome: TestSuccess, DurationMs: submitDuration}, nil
			}
			return TestResult{Outcome: TestUpstreamError, DurationMs: submitDuration, Detail: detail}, nil
		}
		select {
		case <-ctx.Done():
			return TestResult{Outcome: TestUpstreamError, DurationMs: submitDuration,
				Detail: fmt.Sprintf("task did not finish within %s", klingImageProbeDeadline)}, nil
		case <-time.After(klingImageProbeInterval):
		}
	}
}

// oneKlingImageProbeQuery performs one bounded task read. done reports a
// terminal observation; detail names a failure for the caller.
func (c *HTTPProviderClient) oneKlingImageProbeQuery(ctx context.Context, baseURL, apiKey, model, taskID string) (bool, string) {
	queryCtx, cancel := context.WithTimeout(ctx, klingImageProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(queryCtx, http.MethodGet,
		protocols.OriginURL(baseURL, images.KlingImageTaskPathPrefixFor(model)+taskID), nil)
	if err != nil {
		return true, "build task query: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return true, "task query: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, ok := readBoundedBodyN(resp, 1<<20)
	if !ok {
		return true, "task query body: unreadable or oversized"
	}
	if resp.StatusCode != http.StatusOK {
		return true, fmt.Sprintf("task query status %d: %.200s", resp.StatusCode, body)
	}
	task, biz, perr := images.ParseKlingImageTaskResponse(body)
	if perr != nil {
		return true, "task query body: " + perr.Error()
	}
	if biz != nil {
		return true, fmt.Sprintf("HTTP 200: kling error %s: %s", biz.Code, biz.Message)
	}
	if !task.Terminal {
		return false, ""
	}
	if task.Failed {
		return true, "task failed: " + task.StatusMsg
	}
	if len(task.ImageURLs) == 0 {
		return true, "task succeeded without images"
	}
	return true, ""
}
