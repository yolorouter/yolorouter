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
// reaches the provider: a submit through the task dialect the base
// speaks, then one query. A base neither dialect serves has no video
// upstream this build wires, and the probe says so rather than measuring
// an endpoint that cannot serve the mapping.
func (c *HTTPProviderClient) TestVideoGeneration(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	var submitBody []byte
	var submitPath string
	var headers map[string]string
	var err error
	switch {
	case isArkBase(baseURL):
		submitBody, err = videos.EncodeArkSubmit(videos.ArkSubmitRequest{
			Model: model, Prompt: videoProbePrompt, Resolution: "720p", Ratio: "16:9", Duration: 4,
		})
		if err != nil {
			return TestResult{}, fmt.Errorf("encode ark video probe: %w", err)
		}
		submitPath = videos.ArkSubmitPath
	case isDashScopeBase(baseURL):
		submitBody, err = videos.EncodeDashScopeSubmit(videos.DashScopeSubmitRequest{
			Model: model, Prompt: videoProbePrompt, Resolution: "720P", Ratio: "16:9", Duration: 4,
		})
		if err != nil {
			return TestResult{}, fmt.Errorf("encode dashscope video probe: %w", err)
		}
		submitPath = videos.DashScopeSubmitPath
		headers = map[string]string{videos.DashScopeAsyncHeader: "enable"}
	default:
		return TestResult{Outcome: TestUpstreamError, Detail: "video probing is wired for dashscope and ark bases in this build"}, nil
	}

	return c.runRawTestRequestAt(ctx, videos.Origin(baseURL)+submitPath, protocols.ProtocolOpenAI, apiKey, "application/json", submitBody, headers,
		func(resp *http.Response, duration int64) (TestResult, error) {
			body, ok := readBoundedBody(resp)
			if !ok {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
			}
			if resp.StatusCode != http.StatusOK {
				return classifyResponse(protocols.ProtocolOpenAI, resp, body, model, duration), nil
			}
			taskID, refuse, perr := parseVideoProbeSubmit(baseURL, body)
			if perr != nil {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration, Detail: "submit body: " + perr.Error()}, nil
			}
			if refuse != nil {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration,
					Detail: fmt.Sprintf("HTTP 200: %s error %s: %s", dialectName(baseURL), refuse.Code, refuse.Message)}, nil
			}
			// The submit half passed; the query half is one read of the
			// task it created. Any documented status answers, including
			// one still running — that is the protocol working.
			return c.queryVideoProbeTask(ctx, taskQueryURL(baseURL, taskID), apiKey, duration, baseURL)
		})
}

// parseVideoProbeSubmit reads a submit answer in the base's dialect and
// hands back either vendor's refusal behind one face.
func parseVideoProbeSubmit(baseURL string, body []byte) (string, *videos.Refusal, error) {
	if isArkBase(baseURL) {
		id, biz, err := videos.ParseArkSubmitResponse(body)
		if biz != nil {
			return id, &videos.Refusal{Code: biz.Code, Message: biz.Message}, nil
		}
		return id, nil, err
	}
	id, biz, err := videos.ParseDashScopeSubmitResponse(body)
	if biz != nil {
		return id, &videos.Refusal{Code: biz.Code, Message: biz.Message}, nil
	}
	return id, nil, err
}

// dialectName names the base's dialect for error detail strings.
func dialectName(baseURL string) string {
	if isArkBase(baseURL) {
		return "ark"
	}
	return "dashscope"
}

// taskQueryURL builds the one task-read URL in the base's dialect.
func taskQueryURL(baseURL, taskID string) string {
	if isArkBase(baseURL) {
		return videos.Origin(baseURL) + videos.ArkTaskPathPrefix + taskID
	}
	return videos.Origin(baseURL) + videos.DashScopeTaskPathPrefix + taskID
}

// queryVideoProbeTask performs the one task query whose 200 answer
// completes the probe, validating the body in the base's dialect.
func (c *HTTPProviderClient) queryVideoProbeTask(ctx context.Context, url, apiKey string, submitDuration int64, baseURL string) (TestResult, error) {
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
	body := videos.ReadTaskBounded(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return TestResult{Outcome: TestUpstreamError, DurationMs: submitDuration,
			Detail: fmt.Sprintf("task query status %d", resp.StatusCode)}, nil
	}
	var perr error
	if isArkBase(baseURL) {
		_, perr = videos.ParseArkTaskResponse(body)
	} else {
		_, _, perr = videos.ParseDashScopeTaskResponse(body)
	}
	if perr != nil {
		return TestResult{Outcome: TestUpstreamError, DurationMs: submitDuration, Detail: "task query body: " + perr.Error()}, nil
	}
	return TestResult{Outcome: TestSuccess, DurationMs: submitDuration}, nil
}
