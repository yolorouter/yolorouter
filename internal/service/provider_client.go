// ProviderClient abstracts the outbound "test a provider connection" call
// (design doc §5) so provider_service.go can be unit-tested with a fake
// implementation, never triggering a real network request.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yolorouter/yolorouter-ce/internal/middleware"
	"github.com/yolorouter/yolorouter-ce/internal/service/safehttp"
)

// TestOutcome is one of the 8 PRD §6.2.8 test-result categories. Its
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
	// service layer's verification_status write rules (design doc §5)
	// depend on this bit and never re-parse the raw HTTP body themselves.
	IsModelScoped bool
}

// ProviderClient is implemented by HTTPProviderClient in production and by
// a fake in provider_service_test.go.
type ProviderClient interface {
	TestChatCompletion(ctx context.Context, baseURL, apiKey, model string) (TestResult, error)
}

const (
	providerClientTimeout      = 15 * time.Second
	providerClientMaxBodyBytes = 64 * 1024
	// providerClientConcurrency caps simultaneous in-flight real provider
	// test calls across the whole process (design doc §5) — chosen
	// generously enough for a single admin clicking several test buttons in
	// quick succession or one batch test, without letting an unbounded
	// number of outbound sockets accumulate.
	providerClientConcurrency = 8
)

// HTTPProviderClient is the production ProviderClient: a real minimal
// /chat/completions call through safehttp's SSRF-safe transport, with a
// hard per-call timeout and a shared concurrency cap (design doc §5).
type HTTPProviderClient struct {
	httpClient *http.Client
	limiter    *middleware.Semaphore
}

func NewHTTPProviderClient() *HTTPProviderClient {
	return &HTTPProviderClient{
		httpClient: &http.Client{
			Transport: safehttp.NewTransport(),
			// Design doc §5 item 5: never follow redirects. Without this,
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

func (c *HTTPProviderClient) TestChatCompletion(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	if !c.limiter.TryAcquire() {
		return TestResult{}, fmt.Errorf("too many concurrent provider test calls in flight")
	}
	defer c.limiter.Release()

	ctx, cancel := context.WithTimeout(ctx, providerClientTimeout)
	defer cancel()

	reqBody, err := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
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
		// Network failure, timeout, or SSRF-blocked dial — design doc §5
		// item 7: classify as TestUnreachable without leaking which of
		// these it actually was to the admin.
		return TestResult{Outcome: TestUnreachable, DurationMs: duration}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, providerClientMaxBodyBytes+1)
	body, readErr := io.ReadAll(limited)
	if readErr != nil || len(body) > providerClientMaxBodyBytes {
		return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
	}

	return classifyResponse(resp, body, model, duration), nil
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

// isValidSuccessBody implements design doc §5's "成功判定不能只看状态码":
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
// tested model as the reason (design doc §5) — either via error.param
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
