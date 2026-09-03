package providerclient

// The video generation probe. A video mapping is a task dialect, so the
// probe proves both halves of the conversation a routed request actually
// runs: a submit the upstream accepts (task id back), and one query the
// upstream answers (any documented status, RUNNING very much included —
// a video takes longer than any probe should wait, and the mapping's
// health is the protocol working, not the render finishing). Asking the
// OpenAI images path instead would measure a 404 no routed request would
// ever hit, the same trap the DashScope image probe avoids on its side.
//
// Everything the probe branches on per base lives in one table below —
// the third dialect arrived and the per-helper if-chains were six copies
// of the same dispatch, so the dispatch is written once and each row
// carries its dialect's own spellings. The gateway-side dispatch stays
// per-modality (Supports gates and submit builders differ in shape there)
// and is the remaining half of the descriptor consolidation.

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

// videoProbeDialect is one row of the probe's dispatch table: the submit
// body and route, the submit-answer and task-answer parsers, and the task
// route — the whole per-dialect surface the probe needs, in one place.
type videoProbeDialect struct {
	name        string
	matches     func(string) bool
	build       func(model string) (body []byte, path string, headers map[string]string, err error)
	parseSubmit func(body []byte) (taskID string, refuse *videos.Refusal, err error)
	taskURL     func(baseURL, taskID string) string
	parseTask   func(body []byte) error
}

// videoProbeDialects is the dispatch table, first match wins.
var videoProbeDialects = []videoProbeDialect{{
	name: "ark",
	// A closure over the package var, not the var itself: tests override
	// these detectors per fake upstream, and a table row that captured
	// the function value at init would never see the override.
	matches: func(b string) bool { return isArkBase(b) },
	build: func(model string) ([]byte, string, map[string]string, error) {
		body, err := videos.EncodeArkSubmit(videos.ArkSubmitRequest{
			Model: model, Prompt: videoProbePrompt, Resolution: "720p", Ratio: "16:9", Duration: 4,
		})
		if err != nil {
			return nil, "", nil, fmt.Errorf("encode ark video probe: %w", err)
		}
		return body, videos.ArkSubmitPath, nil, nil
	},
	parseSubmit: func(body []byte) (string, *videos.Refusal, error) {
		id, biz, err := videos.ParseArkSubmitResponse(body)
		if biz != nil {
			return id, &videos.Refusal{Code: biz.Code, Message: biz.Message}, nil
		}
		return id, nil, err
	},
	taskURL: func(baseURL, taskID string) string {
		return videos.Origin(baseURL) + videos.ArkTaskPathPrefix + taskID
	},
	parseTask: func(body []byte) error {
		_, err := videos.ParseArkTaskResponse(body)
		return err
	},
}, {
	name:    "dashscope",
	matches: func(b string) bool { return isDashScopeBase(b) },
	build: func(model string) ([]byte, string, map[string]string, error) {
		body, err := videos.EncodeDashScopeSubmit(videos.DashScopeSubmitRequest{
			Model: model, Prompt: videoProbePrompt, Resolution: "720P", Ratio: "16:9", Duration: 4,
		})
		if err != nil {
			return nil, "", nil, fmt.Errorf("encode dashscope video probe: %w", err)
		}
		return body, videos.DashScopeSubmitPath, map[string]string{videos.DashScopeAsyncHeader: "enable"}, nil
	},
	parseSubmit: func(body []byte) (string, *videos.Refusal, error) {
		id, biz, err := videos.ParseDashScopeSubmitResponse(body)
		if biz != nil {
			return id, &videos.Refusal{Code: biz.Code, Message: biz.Message}, nil
		}
		return id, nil, err
	},
	taskURL: func(baseURL, taskID string) string {
		return videos.Origin(baseURL) + videos.DashScopeTaskPathPrefix + taskID
	},
	parseTask: func(body []byte) error {
		_, _, err := videos.ParseDashScopeTaskResponse(body)
		return err
	},
}, {
	name:    "kling",
	matches: func(b string) bool { return isKlingBase(b) },
	build: func(model string) ([]byte, string, map[string]string, error) {
		// Kling's endpoints accept any integer 3..15; the probe asks for
		// the floor of that range because a probe task renders and bills
		// for real — three seconds is the cheapest clip the dialect can
		// ask for, and it is an internal ask, not the caller-facing
		// vocabulary.
		body, err := videos.EncodeKlingSubmit(videos.KlingSubmitRequest{
			Model: model, Prompt: videoProbePrompt, Resolution: "720p", Ratio: "16:9", Duration: 3,
		})
		if err != nil {
			return nil, "", nil, fmt.Errorf("encode kling video probe: %w", err)
		}
		return body, videos.KlingSubmitPath(model, false), nil, nil
	},
	parseSubmit: func(body []byte) (string, *videos.Refusal, error) {
		id, biz, err := videos.ParseKlingSubmitResponse(body)
		if biz != nil {
			return id, &videos.Refusal{Code: biz.Code, Message: biz.Message}, nil
		}
		return id, nil, err
	},
	taskURL: func(baseURL, taskID string) string {
		return videos.Origin(baseURL) + videos.KlingTaskRoute(taskID)
	},
	parseTask: func(body []byte) error {
		_, _, err := videos.ParseKlingTaskResponse(body)
		return err
	},
}}

// videoProbeDialectFor resolves a base to its row, nil when no wired
// dialect speaks it.
func videoProbeDialectFor(baseURL string) *videoProbeDialect {
	for i := range videoProbeDialects {
		if videoProbeDialects[i].matches(baseURL) {
			return &videoProbeDialects[i]
		}
	}
	return nil
}

// MediaDialectBase reports whether a base URL is one the media dialects
// speak — the same dispatch TestVideoGeneration routes on, so a caller
// gating on it can never drift from the probe's own routing. Key
// verification uses it to gate BOTH media fallbacks (video, then image):
// these are the vendors whose one base serves chat, image, and video
// dialects side by side, which is what makes a chat-shaped refusal here
// ambiguous — a wrong model name on an ordinary OpenAI-compatible host is
// just a wrong model name, and probing it with a billable render would
// measure nothing a routed request ever hits.
func MediaDialectBase(baseURL string) bool {
	return videoProbeDialectFor(baseURL) != nil
}

// TestVideoGeneration probes a video mapping the way a video request
// reaches the provider: a submit through the task dialect the base
// speaks, then one query. A base no wired dialect serves has no video
// upstream this build routes to, and the probe says so rather than
// measuring an endpoint that cannot serve the mapping.
func (c *HTTPProviderClient) TestVideoGeneration(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	dialect := videoProbeDialectFor(baseURL)
	if dialect == nil {
		return TestResult{Outcome: TestUpstreamError, Detail: "video probing is wired for dashscope, ark, and kling bases in this build"}, nil
	}
	submitBody, submitPath, headers, err := dialect.build(model)
	if err != nil {
		return TestResult{}, err
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
			taskID, refuse, perr := dialect.parseSubmit(body)
			if perr != nil {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration, Detail: "submit body: " + perr.Error()}, nil
			}
			if refuse != nil {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration,
					Detail: fmt.Sprintf("HTTP 200: %s error %s: %s", dialect.name, refuse.Code, refuse.Message)}, nil
			}
			// The submit half passed; the query half is one read of the
			// task it created. Any documented status answers, including
			// one still running — that is the protocol working.
			return c.queryVideoProbeTask(ctx, dialect.taskURL(baseURL, taskID), apiKey, duration, dialect.parseTask)
		})
}

// queryVideoProbeTask performs the one task query whose 200 answer
// completes the probe, validating the body through the dialect's own
// parser.
func (c *HTTPProviderClient) queryVideoProbeTask(ctx context.Context, url, apiKey string, submitDuration int64, parseTask func([]byte) error) (TestResult, error) {
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
	if perr := parseTask(body); perr != nil {
		return TestResult{Outcome: TestUpstreamError, DurationMs: submitDuration, Detail: "task query body: " + perr.Error()}, nil
	}
	return TestResult{Outcome: TestSuccess, DurationMs: submitDuration}, nil
}
