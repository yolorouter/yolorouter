// ProviderClient abstracts the outbound "test a provider connection" call
// so provider_service.go can be unit-tested with a fake
// implementation, never triggering a real network request.
package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/service/safehttp"
)

// TestOutcome is one of the 8 test-result categories. Its
// numeric values are stored verbatim in provider_keys.last_test_result
// (see model.LastTestResult* constants, which must stay numerically
// identical to this list).
type TestOutcome int

const (
	TestSuccess TestOutcome = iota
	TestAuthFailed
	TestPermissionDenied
	TestModelNotFound
	TestQuotaUnavailable
	TestRateLimited
	TestUnreachable
	TestUpstreamError
)

// TestResult is what ProviderClient returns for one test attempt.
type TestResult struct {
	Outcome    TestOutcome
	DurationMs int64
	// IsModelScoped is only meaningful when Outcome == TestPermissionDenied:
	// true if the response body structurally names the tested model as the
	// reason (error.param == "model", or the message references it). The
	// service layer's verification_status write rules
	// depend on this bit and never re-parse the raw HTTP body themselves.
	IsModelScoped bool
}

// ProviderClient is implemented by HTTPProviderClient in production and by
// a fake in provider_service_test.go.
type ProviderClient interface {
	TestChatCompletion(ctx context.Context, baseURL, apiKey, model string) (TestResult, error)
	// TestStreamingCompletion validates that baseURL+model can serve a
	// streaming response:
	// success requires at least one structurally valid `delta` chunk
	// followed by a normal `data: [DONE]` termination.
	TestStreamingCompletion(ctx context.Context, baseURL, apiKey, model string) (TestResult, error)
	// TestFunctionCalling validates that baseURL+model can return a
	// structurally valid tool_calls response to a minimal tool definition.
	TestFunctionCalling(ctx context.Context, baseURL, apiKey, model string) (TestResult, error)
}

const (
	providerClientTimeout      = 15 * time.Second
	providerClientMaxBodyBytes = 64 * 1024
	// providerClientConcurrency caps simultaneous in-flight real provider
	// test calls across the whole process — chosen
	// generously enough for a single admin clicking several test buttons in
	// quick succession or one batch test, without letting an unbounded
	// number of outbound sockets accumulate.
	providerClientConcurrency = 8
)

// HTTPProviderClient is the production ProviderClient: a real minimal
// /chat/completions call through safehttp's SSRF-safe transport, with a
// hard per-call timeout and a shared concurrency cap.
type HTTPProviderClient struct {
	httpClient *http.Client
	limiter    *middleware.Semaphore
}

// NewHTTPProviderClient builds the connection-test client. allowPrivate is
// forwarded to safehttp.NewTransport so a self-hosted operator testing a
// LAN/localhost model server can opt out of the SSRF IP-range denial (see
// config.SecurityConfig.AllowPrivateUpstreams).
func NewHTTPProviderClient(allowPrivate bool) *HTTPProviderClient {
	return &HTTPProviderClient{
		httpClient: &http.Client{
			Transport: safehttp.NewTransport(allowPrivate),
			// Never follow redirects. Without this,
			// Go's default http.Client follows up to 10 redirect hops and
			// may carry the Authorization header (the decrypted upstream
			// key) to a host the admin never confirmed.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		limiter: middleware.NewSemaphore(providerClientConcurrency),
	}
}

type chatCompletionErrorBody struct {
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Param   string `json:"param"`
	} `json:"error"`
}

type chatCompletionSuccessBody struct {
	Choices []struct {
		// Message is a pointer so a body that omits the field entirely, or
		// sets it explicitly to null, both decode to nil — isValidSuccessBody
		// treats either as "not actually a valid completion", not a
		// zero-value message that would otherwise satisfy len(Choices) > 0.
		Message *struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// runTestRequest builds and sends a POST /chat/completions request, holding
// the shared concurrency slot and per-call timeout for the request's ENTIRE
// duration — including whatever handle does with the response body — not
// just until headers arrive. This is why the semaphore/timeout/transport-
// error handling can't be split into a plain "get me an *http.Response"
// helper that returns before the caller reads the body: streaming's body
// read can itself take a while, and it must still count against the same
// cap as every other in-flight test call.
//
// On a transport-level failure (network/timeout/SSRF-blocked dial), handle
// is never invoked and TestUnreachable is returned directly, matching the
// "don't leak which kind of failure this was" rule.
func (c *HTTPProviderClient) runTestRequest(
	ctx context.Context, baseURL, apiKey string, reqPayload interface{},
	handle func(resp *http.Response, durationMs int64) (TestResult, error),
) (TestResult, error) {
	if !c.limiter.TryAcquire() {
		return TestResult{}, fmt.Errorf("too many concurrent provider test calls in flight")
	}
	defer c.limiter.Release()

	ctx, cancel := context.WithTimeout(ctx, providerClientTimeout)
	defer cancel()

	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		return TestResult{}, fmt.Errorf("marshal request body: %w", err)
	}

	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return TestResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		return TestResult{Outcome: TestUnreachable, DurationMs: duration}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	return handle(resp, duration)
}

// readBoundedBody applies the same "don't trust an unbounded upstream body"
// limit every test method needs before classifying a non-streaming response.
func readBoundedBody(resp *http.Response) ([]byte, bool) {
	limited := io.LimitReader(resp.Body, providerClientMaxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil || len(body) > providerClientMaxBodyBytes {
		return nil, false
	}
	return body, true
}

func (c *HTTPProviderClient) TestChatCompletion(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	payload := map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	}
	return c.runTestRequest(ctx, baseURL, apiKey, payload, func(resp *http.Response, duration int64) (TestResult, error) {
		body, ok := readBoundedBody(resp)
		if !ok {
			return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
		}
		return classifyResponse(resp, body, model, duration), nil
	})
}

// streamChunk mirrors the minimal OpenAI streaming chunk shape this test
// needs to recognize a structurally valid delta — same "don't trust a 200
// status code alone" principle as isValidSuccessBody.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// scanSSEStream reads a bounded number of bytes off r looking for at least
// one structurally valid `data: {...}` chunk followed by a normal
// `data: [DONE]` termination. Non-"data:" lines (blank lines, comments) are
// skipped, not treated as failures.
func scanSSEStream(r io.Reader) (sawValidDelta, sawDone bool) {
	scanner := bufio.NewScanner(io.LimitReader(r, providerClientMaxBodyBytes))
	for scanner.Scan() {
		line := scanner.Text()
		data := strings.TrimPrefix(line, "data: ")
		if data == line {
			continue // not an SSE data line
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			sawValidDelta = true
		}
	}
	return sawValidDelta, sawDone
}

func (c *HTTPProviderClient) TestStreamingCompletion(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	payload := map[string]interface{}{
		"model":    model,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	}
	return c.runTestRequest(ctx, baseURL, apiKey, payload, func(resp *http.Response, duration int64) (TestResult, error) {
		if resp.StatusCode != http.StatusOK {
			body, ok := readBoundedBody(resp)
			if !ok {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
			}
			return classifyResponse(resp, body, model, duration), nil
		}

		sawValidDelta, sawDone := scanSSEStream(resp.Body)
		if sawValidDelta && sawDone {
			return TestResult{Outcome: TestSuccess, DurationMs: duration}, nil
		}
		return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
	})
}

// toolCallResponseBody mirrors the minimal OpenAI tool_calls response shape.
type toolCallResponseBody struct {
	Choices []struct {
		Message struct {
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// isValidToolCallsBody requires at least one choice with at least one
// tool_calls entry naming a real function and carrying parseable JSON
// arguments — a bare non-empty tool_calls array with garbage fields is not
// enough (mirrors isValidSuccessBody's "don't trust the status code alone"
// principle, applied to the tool-call response shape).
func isValidToolCallsBody(body []byte) bool {
	var parsed toolCallResponseBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	if len(parsed.Choices) == 0 || len(parsed.Choices[0].Message.ToolCalls) == 0 {
		return false
	}
	call := parsed.Choices[0].Message.ToolCalls[0]
	if call.Function.Name == "" {
		return false
	}
	var args map[string]interface{}
	return json.Unmarshal([]byte(call.Function.Arguments), &args) == nil
}

func (c *HTTPProviderClient) TestFunctionCalling(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	payload := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "What's the weather in Beijing?"},
		},
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "get_weather",
					"description": "Get the current weather for a location",
					"parameters": map[string]interface{}{
						"type":       "object",
						"properties": map[string]interface{}{"location": map[string]string{"type": "string"}},
						"required":   []string{"location"},
					},
				},
			},
		},
	}
	return c.runTestRequest(ctx, baseURL, apiKey, payload, func(resp *http.Response, duration int64) (TestResult, error) {
		body, ok := readBoundedBody(resp)
		if !ok {
			return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
		}
		if resp.StatusCode != http.StatusOK {
			return classifyResponse(resp, body, model, duration), nil
		}
		if !isValidToolCallsBody(body) {
			return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
		}
		return TestResult{Outcome: TestSuccess, DurationMs: duration}, nil
	})
}

func classifyResponse(resp *http.Response, body []byte, model string, durationMs int64) TestResult {
	switch resp.StatusCode {
	case http.StatusOK:
		if isValidSuccessBody(resp, body) {
			return TestResult{Outcome: TestSuccess, DurationMs: durationMs}
		}
		return TestResult{Outcome: TestUpstreamError, DurationMs: durationMs}
	case http.StatusUnauthorized:
		return TestResult{Outcome: TestAuthFailed, DurationMs: durationMs}
	case http.StatusForbidden:
		modelScoped := isModelScopedError(body, model)
		return TestResult{Outcome: TestPermissionDenied, DurationMs: durationMs, IsModelScoped: modelScoped}
	case http.StatusNotFound:
		return TestResult{Outcome: TestModelNotFound, DurationMs: durationMs}
	case http.StatusTooManyRequests:
		if isQuotaError(body) {
			return TestResult{Outcome: TestQuotaUnavailable, DurationMs: durationMs}
		}
		return TestResult{Outcome: TestRateLimited, DurationMs: durationMs}
	default:
		if resp.StatusCode >= 500 {
			return TestResult{Outcome: TestUpstreamError, DurationMs: durationMs}
		}
		if isModelNotFoundError(body) {
			return TestResult{Outcome: TestModelNotFound, DurationMs: durationMs}
		}
		return TestResult{Outcome: TestUpstreamError, DurationMs: durationMs}
	}
}

// isValidSuccessBody enforces the "success cannot be judged by the status code alone" rule:
// Content-Type must be application/json, the body must parse, and it must
// contain the OpenAI-compatible minimal structure with no top-level error.
func isValidSuccessBody(resp *http.Response, body []byte) bool {
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(contentType), "application/json") {
		return false
	}
	var errBody chatCompletionErrorBody
	if err := json.Unmarshal(body, &errBody); err == nil && errBody.Error != nil {
		return false
	}
	var success chatCompletionSuccessBody
	if err := json.Unmarshal(body, &success); err != nil {
		return false
	}
	return len(success.Choices) > 0 && success.Choices[0].Message != nil
}

func parseErrorBody(body []byte) *chatCompletionErrorBody {
	var parsed chatCompletionErrorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	return &parsed
}

// isModelScopedError reports whether a 403 body structurally names the
// tested model as the reason — either via error.param
// (OpenAI's convention for field-specific errors) or the message text
// mentioning the model name.
func isModelScopedError(body []byte, model string) bool {
	parsed := parseErrorBody(body)
	if parsed == nil || parsed.Error == nil {
		return false
	}
	if parsed.Error.Param == "model" {
		return true
	}
	return model != "" && strings.Contains(strings.ToLower(parsed.Error.Message), strings.ToLower(model))
}

func isQuotaError(body []byte) bool {
	parsed := parseErrorBody(body)
	if parsed == nil || parsed.Error == nil {
		return false
	}
	if parsed.Error.Code == "insufficient_quota" {
		return true
	}
	lower := strings.ToLower(parsed.Error.Message)
	return strings.Contains(lower, "quota") || strings.Contains(lower, "billing")
}

func isModelNotFoundError(body []byte) bool {
	parsed := parseErrorBody(body)
	if parsed == nil || parsed.Error == nil {
		return false
	}
	if parsed.Error.Code == "model_not_found" {
		return true
	}
	lower := strings.ToLower(parsed.Error.Message)
	return strings.Contains(lower, "model") && (strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist"))
}
