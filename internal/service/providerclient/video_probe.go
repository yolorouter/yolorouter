package providerclient

// The video generation probe. A video mapping is a task dialect, so the
// probe proves both halves of the conversation a routed request actually
// runs: a submit the upstream accepts (task id back), and one query the
// upstream answers (any documented status, RUNNING very much included —
// a video takes longer than any probe should wait, and the mapping's
// health is the protocol working, not the render finishing). Asking the
// OpenAI images path instead would measure a 404 no routed request would
// ever hit, the same trap the DashScope image probe avoids on its side.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/videos"
)

// videoProbePrompt is the minimal render the video probe asks for: a
// cheap, short clip at the smallest documented duration.
const videoProbePrompt = "a red balloon drifting across a plain sky"

// TestVideoGeneration probes a video mapping the way a video request
// reaches the provider: submit through the DashScope task dialect, then
// one query. A non-DashScope base has no video upstream this build wires,
// and the probe says so rather than measuring an endpoint that cannot
// serve the mapping.
func (c *HTTPProviderClient) TestVideoGeneration(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	if !isDashScopeBase(baseURL) {
		return TestResult{Outcome: TestUpstreamError, Detail: "video probing is wired for dashscope-compatible bases only in this build"}, nil
	}

	submitBody, err := videos.EncodeDashScopeSubmit(videos.DashScopeSubmitRequest{
		Model: model, Prompt: videoProbePrompt, Resolution: "720P", Ratio: "16:9", Duration: 4,
	})
	if err != nil {
		return TestResult{}, fmt.Errorf("encode dashscope video probe: %w", err)
	}
	submitURL := videos.DashScopeOrigin(baseURL) + videos.DashScopeSubmitPath

	return c.runRawTestRequestAt(ctx, submitURL, protocols.ProtocolOpenAI, apiKey, "application/json", submitBody,
		map[string]string{videos.DashScopeAsyncHeader: "enable"},
		func(resp *http.Response, duration int64) (TestResult, error) {
			body, ok := readBoundedBody(resp)
			if !ok {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
			}
			if resp.StatusCode != http.StatusOK {
				return classifyResponse(protocols.ProtocolOpenAI, resp, body, model, duration), nil
			}
			taskID, biz, perr := videos.ParseDashScopeSubmitResponse(body)
			if perr != nil {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration, Detail: "submit body: " + perr.Error()}, nil
			}
			if biz != nil {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration,
					Detail: fmt.Sprintf("HTTP 200: dashscope error %s: %s", biz.Code, biz.Message)}, nil
			}
			// The submit half passed; the query half is one read of the
			// task it created. Any documented status answers, including
			// one still running — that is the protocol working.
			return c.queryDashScopeVideoProbe(ctx, videos.DashScopeOrigin(baseURL)+videos.DashScopeTaskPathPrefix+taskID, apiKey, duration)
		})
}

// queryDashScopeVideoProbe performs the one task query whose 200 answer
// completes the probe.
func (c *HTTPProviderClient) queryDashScopeVideoProbe(ctx context.Context, url, apiKey string, submitDuration int64) (TestResult, error) {
	ctx, cancel := context.WithTimeout(ctx, providerClientTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return TestResult{}, fmt.Errorf("build task query: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return TestResult{Outcome: TestUpstreamError, DurationMs: submitDuration, Detail: "task query: " + err.Error()}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body := videos.ReadDashScopeBounded(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return TestResult{Outcome: TestUpstreamError, DurationMs: submitDuration,
			Detail: fmt.Sprintf("task query status %d", resp.StatusCode)}, nil
	}
	if _, _, perr := videos.ParseDashScopeTaskResponse(body); perr != nil {
		return TestResult{Outcome: TestUpstreamError, DurationMs: submitDuration, Detail: "task query body: " + perr.Error()}, nil
	}
	return TestResult{Outcome: TestSuccess, DurationMs: submitDuration}, nil
}
