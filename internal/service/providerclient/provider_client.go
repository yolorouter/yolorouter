// Package providerclient owns the outbound "test a provider connection"
// surface: the ProviderClient abstraction lets the provider and model
// services be unit-tested with a fake implementation, never triggering a
// real network request, while HTTPProviderClient is the production probe.
package providerclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/images"
	"github.com/yolorouter/yolorouter/internal/protocols/responses"
	"github.com/yolorouter/yolorouter/internal/protocols/videos"
	"github.com/yolorouter/yolorouter/internal/service/safehttp"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// TestOutcome is one of the 10 test-result categories. Its
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
	// TestVerificationUnsupported marks a destination whose protocol has no
	// real success-body validator yet (probeSpec.successCertifiable false —
	// no current protocol, kept for protocols added before their validator
	// is written): a 2xx response from that destination cannot be certified
	// as a genuine pass, so this outcome is returned instead of TestSuccess.
	// classifyTestResult never lets this outcome mark a key passed/enabled,
	// unlike a real TestSuccess.
	TestVerificationUnsupported
	// TestTimeout marks a destination that accepted the connection but did
	// not answer within providerClientTimeout. It is deliberately separate
	// from TestUnreachable, which means the opposite: no connection was ever
	// established. The distinction is what an operator acts on — an
	// unreachable address is a wrong URL or a blocked network, whereas a
	// timeout is a reachable address whose upstream is merely slow or is
	// working through its own failover chain, and pointing that operator at
	// their URL spelling sends them after the one thing that is not broken.
	TestTimeout
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
	// Detail is a concise, admin-facing diagnostic string for a failed test:
	// the HTTP status plus the upstream's own error message when present
	// (e.g. `HTTP 401: invalid api key`). Empty on success, with one
	// deliberate exception: the key-verification media fallbacks stamp a
	// passing result with the note of which probe shape decided it, because
	// an operator reading "verified" deserves to know the chat probe was
	// not the one that said so. It is surfaced only in the provider setup
	// UI to help an operator tell apart a bad key from a wrong model from a
	// blocked address — never returned to end users, so echoing the
	// upstream message here is intentional.
	Detail string
}

// ListModelsResult is what ProviderClient returns for a model-catalogue
// fetch. On success Outcome is TestSuccess and Models holds the upstream
// model ids; on failure Models is empty and Outcome/Detail describe why,
// classified the same way as a credential test so the UI can reuse one set
// of friendly messages.
type ListModelsResult struct {
	Models     []string
	Outcome    TestOutcome
	DurationMs int64
	Detail     string
}

// ProviderClient is implemented by HTTPProviderClient in production and by
// a fake in provider_service_test.go. Every method takes the wire protocol
// the provider's credential must be tested against (openai/anthropic/
// gemini/responses) — a provider configured as an anthropic upstream must be
// probed with /v1/messages + x-api-key, never OpenAI's /chat/completions +
// Bearer token, or the "test" would exercise a completely different
// endpoint than production traffic actually dispatches to.
type ProviderClient interface {
	TestChatCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (TestResult, error)
	// TestStreamingCompletion validates that baseURL+model can serve a
	// streaming response:
	// success requires at least one structurally valid `delta` chunk
	// followed by a normal `data: [DONE]` termination.
	TestStreamingCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (TestResult, error)
	// TestFunctionCalling validates that baseURL+model can return a
	// structurally valid tool_calls response to a minimal tool definition.
	TestFunctionCalling(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (TestResult, error)
	// TestImageGeneration validates that baseURL+model can generate an
	// image: a minimal generation request. Rides the OpenAI wire family
	// (bearer credential, JSON body) regardless of the provider's chat
	// protocol, with one dialect exception: a DashScope host is asked
	// through its native multimodal-generation endpoint, the same way the
	// gateway's image delivery reaches it.
	TestImageGeneration(ctx context.Context, baseURL, apiKey, model string) (TestResult, error)
	// TestVideoGeneration validates that baseURL+model can run the video
	// task conversation a routed request runs: a submit the upstream
	// accepts (task id back) and one query it answers — completion is not
	// waited for, a render outlasting the probe budget is a healthy
	// mapping, not a broken one. Serves the two native task dialects
	// (DashScope wan, Volcengine Ark); other bases refuse rather than
	// measuring an endpoint no routed request would hit. Key
	// verification reuses it as the first media fallback for a
	// media-dialect base whose chat probe cannot serve the model.
	TestVideoGeneration(ctx context.Context, baseURL, apiKey, model string) (TestResult, error)
	// ListModels fetches the upstream model catalogue for a credential
	// (openai/anthropic/responses: GET /v1/models; gemini: GET /v1beta/models),
	// used to populate the admin UI's test-model picker before a provider row
	// exists. It never persists anything.
	ListModels(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey string) (ListModelsResult, error)
}

const (
	// providerClientTimeout caps one whole test call. It is sized for the
	// FAILURE path, not the success path: a healthy upstream answers this
	// max_tokens=1 ping in a few seconds, but an upstream that is going to
	// fail usually walks its entire candidate/key chain first and only then
	// reports the error, which takes far longer. A budget that expires
	// between those two — 15s did — discards precisely the responses with
	// the most diagnostic value, replacing an upstream's own explanation
	// (e.g. `HTTP 502: All provider attempts failed`) with a bare timeout
	// that names nothing. 60s clears the failover path while still bounding
	// an admin who is synchronously watching a spinner.
	providerClientTimeout      = 60 * time.Second
	providerClientMaxBodyBytes = 64 * 1024
	// providerClientConnectTimeout bounds each TCP dial during a provider
	// connection test. Provider connection tests are an admin-only path with
	// their own timeout model (providerClientTimeout caps the whole call at
	// 60s; there is no streaming relay and no failover budget), so the dial
	// bound belongs to this layer rather than being threaded through
	// GatewayConfig.ConnectTimeout (which governs only the gateway relay path).
	providerClientConnectTimeout = 5 * time.Second
	// providerClientTLSHandshakeTimeout bounds the TLS handshake during a
	// provider connection test. Like providerClientConnectTimeout it belongs to
	// this admin-only layer (providerClientTimeout caps the whole call at 60s;
	// no streaming relay, no failover budget) rather than being threaded from
	// GatewayConfig.TLSHandshakeTimeout, which governs only the relay path.
	providerClientTLSHandshakeTimeout = 10 * time.Second
	// providerClientConcurrency caps simultaneous in-flight real provider
	// test calls per HTTPProviderClient — each one allocates its own semaphore,
	// so the provider-key and model-candidate services get a pool each rather
	// than sharing one. Chosen
	// generously enough for a single admin clicking several test buttons in
	// quick succession or one batch test, without letting an unbounded
	// number of outbound sockets accumulate.
	providerClientConcurrency = 8
)

// HTTPProviderClient is the production ProviderClient: a real minimal
// protocol-appropriate call through safehttp's SSRF-safe transport, with a
// hard per-call timeout and a shared concurrency cap.
type HTTPProviderClient struct {
	httpClient *http.Client
	limiter    *middleware.Semaphore
}

// NewHTTPProviderClient builds the connection-test client. allowPrivate is
// forwarded to safehttp.NewTransport so a self-hosted operator testing a
// LAN/localhost model server can opt out of the SSRF IP-range denial (see
// config.SecurityConfig.AllowPrivateUpstreams). The dial bound is
// providerClientConnectTimeout (not GatewayConfig.ConnectTimeout) because
// connection tests are an admin-only path with their own timeout model
// (providerClientTimeout=60s overall cap, no streaming relay, no failover
// budget), so the dial bound stays at this layer instead of being threaded
// from the gateway config.
func NewHTTPProviderClient(allowPrivate bool) *HTTPProviderClient {
	return &HTTPProviderClient{
		httpClient: &http.Client{
			Transport: safehttp.NewTransport(allowPrivate, providerClientConnectTimeout, providerClientTLSHandshakeTimeout),
			// Never follow redirects. Without this,
			// Go's default http.Client follows up to 10 redirect hops and
			// may carry the credential header (the decrypted upstream
			// key) to a host the admin never confirmed.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		limiter: middleware.NewSemaphore(providerClientConcurrency),
	}
}

// requestEncoderFor returns the codec whose EgressPath/SetupRequest a
// credential test must drive for proto — the exact same encoders runtime
// dispatch uses, so a provider that passes its test is guaranteed to be
// hit the same way in production. Unknown protocols fall back to the
// OpenAI codec, mirroring providerproto.TypeOf's own default.
func requestEncoderFor(proto protocols.ProtocolID) protocols.RequestEncoder {
	return probeSpecFor(proto).encoder
}

// chatCompletionErrorBody and chatCompletionSuccessBody are OpenAI Chat
// Completions body shapes.
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
		// sets it explicitly to null, both decode to nil — isValidOpenAIChatSuccessBody
		// treats either as "not actually a valid completion", not a
		// zero-value message that would otherwise satisfy len(Choices) > 0.
		Message *struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// claudeErrorBody and claudeSuccessBody are Anthropic Messages API body
// shapes: a success body carries "type":"message" with a non-empty content
// array, an error body carries "type":"error" with a nested error object.
type claudeErrorBody struct {
	Type  string `json:"type"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type claudeSuccessBody struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
	} `json:"content"`
}

// geminiSuccessBody and responsesSuccessBody are the minimal shapes the
// credential test certifies against. They deliberately stay independent of
// the runtime response decoders, which are lenient by design (they serve
// partial or error-carrying 200 bodies so billing and passthrough keep
// working); certification needs the stricter question "did the model
// actually generate output for this credential" — a placeholder element
// (null, {}) must not count as generated output, so the elements carry just
// enough fields to recognize real content. Error is a pointer so an
// explicit "error": null (which both APIs emit on success) decodes to nil
// rather than counting as an error.
type geminiSuccessBody struct {
	Error      *struct{} `json:"error"`
	Candidates []struct {
		Content *struct {
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string `json:"name"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

type responsesSuccessBody struct {
	Error  *struct{} `json:"error"`
	Status string    `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
}

// providerTestURL builds the connection-test endpoint URL using the exact
// same builder runtime dispatch uses (protocols.JoinUpstreamURL with the
// protocol's own egress path) — otherwise a bare-host or path-prefixed
// baseURL could pass verification against one endpoint while production
// traffic actually dispatches to another (e.g. a bare host silently NOT
// getting /v1 inserted at verification time while runtime dispatch does
// insert it). model is passed through to EgressPath (rather than an empty
// string) because Gemini's egress path embeds the model name
// (/models/X:generateContent); every other codec here ignores it.
func providerTestURL(baseURL string, proto protocols.ProtocolID, model string) string {
	return protocols.JoinUpstreamURL(baseURL, requestEncoderFor(proto).EgressPath(model, false), proto)
}

// runTestRequest builds and sends a POST request against the protocol's own
// test endpoint, holding
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
	ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string, reqPayload interface{},
	handle func(resp *http.Response, durationMs int64) (TestResult, error),
) (TestResult, error) {
	return c.runTestRequestAt(ctx, providerTestURL(baseURL, proto, model), proto, apiKey, model, reqPayload, handle)
}

// runTestRequestAt is runTestRequest against a caller-stated URL, for the
// probes whose endpoint is not their protocol's chat path (the images probe
// rides the OpenAI wire family but not its chat route).
func (c *HTTPProviderClient) runTestRequestAt(
	ctx context.Context, url string, proto protocols.ProtocolID, apiKey, model string, reqPayload interface{},
	handle func(resp *http.Response, durationMs int64) (TestResult, error),
) (TestResult, error) {
	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		return TestResult{}, fmt.Errorf("marshal request body: %w", err)
	}
	return c.runRawTestRequestAt(ctx, url, proto, apiKey, "application/json", reqBody, nil, handle)
}

// runRawTestRequestAt is runTestRequestAt for a body that is already bytes
// and a content type that says what it is — the multipart edit probe cannot
// ride the JSON marshal the generic runner does. An explicit content type
// overrides the one the codec set, so a multipart upload is not announced
// as JSON.
func (c *HTTPProviderClient) runRawTestRequestAt(
	ctx context.Context, url string, proto protocols.ProtocolID, apiKey, contentType string, reqBody []byte,
	extraHeaders map[string]string,
	handle func(resp *http.Response, durationMs int64) (TestResult, error),
) (TestResult, error) {
	if !c.limiter.TryAcquire() {
		return TestResult{}, fmt.Errorf("too many concurrent provider test calls in flight")
	}
	defer c.limiter.Release()

	ctx, cancel := context.WithTimeout(ctx, providerClientTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return TestResult{}, fmt.Errorf("build request: %w", err)
	}
	// The codec sets Content-Type plus the protocol-correct auth header:
	// Authorization: Bearer for openai/responses, x-api-key +
	// anthropic-version for anthropic, an API-key header/param for gemini.
	requestEncoderFor(proto).SetupRequest(req, apiKey)
	// Dialect-required headers (a task-mode switch an upstream gates its
	// endpoint on), applied after the codec's own.
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	watcher := &connectionWatcher{}
	req = watcher.attach(req)

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		// Separate "connected but never answered in time" from "never
		// connected". The nested dial/TLS bounds carry deadlines of their own
		// and expire while this call's budget is still healthy, so neither the
		// error text nor ctx alone can make this call — hence the watcher.
		detail := fmt.Sprintf("request failed after %dms: %v", duration, redactErr(err))
		logger.Warn("provider test: request error", zap.String("url", protocols.RedactURL(url)), zap.Int64("duration_ms", duration), zap.String("error", redactErr(err)))
		return TestResult{Outcome: timeoutOrUnreachable(ctx, watcher), DurationMs: duration, Detail: detail}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	res, handleErr := handle(resp, duration)
	if handleErr != nil {
		return res, handleErr
	}
	// A stall while reading the body or the SSE stream expires the budget
	// INSIDE handle, where it can only present as a body-shape failure
	// (unreadable body, incomplete stream) and settles as TestUpstreamError.
	// That blames the upstream's reply for our own budget running out and
	// hides the case from the timeout category entirely, so re-check here.
	// duration only covers time-to-headers; the body read is why we are here,
	// so the reported figure is re-measured over the whole call.
	if res.Outcome != TestSuccess && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		total := time.Since(start).Milliseconds()
		res.Outcome = TestTimeout
		res.DurationMs = total
		res.Detail = fmt.Sprintf("response incomplete after %dms: %v", total, ctx.Err())
	}
	return res, nil
}

// connectionWatcher records whether the transport ever handed this client a
// live connection. It is the evidence TestTimeout requires, and the error
// value alone cannot supply it: a stalled DNS resolve and a stalled response
// both surface as context.DeadlineExceeded, yet only the second one proves the
// address answered at all. Reporting the first as a timeout would tell an
// operator their base URL is reachable on no evidence whatsoever.
type connectionWatcher struct{ got atomic.Bool }

func (w *connectionWatcher) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { w.got.Store(true) }}
}

// attach returns req bound to a context carrying this watcher's trace,
// preserving whatever context req already had (its call budget).
func (w *connectionWatcher) attach(req *http.Request) *http.Request {
	return req.WithContext(httptrace.WithClientTrace(req.Context(), w.trace()))
}

// timeoutOrUnreachable classifies a failed attempt. TestTimeout requires BOTH
// an expired call budget and a connection that was actually established;
// anything else is unreachability, including a cancelled call (a disconnected
// admin), which is not a statement about the upstream at all.
func timeoutOrUnreachable(ctx context.Context, w *connectionWatcher) TestOutcome {
	if w.got.Load() && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return TestTimeout
	}
	return TestUnreachable
}

// redactErr returns a log-safe description of err: for *url.Error (returned
// by http.Client.Do), it keeps the operation and inner error but drops the
// embedded URL, which may carry credentials and is logged separately via
// redactURL. Other errors pass through unchanged.
func redactErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Sprintf("%s: %v", ue.Op, ue.Err)
	}
	return err.Error()
}

// readBoundedBody applies the same "don't trust an unbounded upstream body"
// limit every test method needs before classifying a non-streaming response.
func readBoundedBody(resp *http.Response) ([]byte, bool) {
	return readBoundedBodyN(resp, providerClientMaxBodyBytes)
}

// readBoundedBodyN is readBoundedBody with a caller-chosen cap, so a
// model-catalogue fetch (which can legitimately be much larger than a test
// ping reply) can read up to its own higher limit.
func readBoundedBodyN(resp *http.Response, maxBytes int64) ([]byte, bool) {
	limited := io.LimitReader(resp.Body, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil || int64(len(body)) > maxBytes {
		return nil, false
	}
	return body, true
}

// chatCompletionPayload builds the minimal request body for a basic-text
// credential test, shaped for proto — see the per-protocol notes on the
// probeSpecs entries.
func chatCompletionPayload(proto protocols.ProtocolID, model string) map[string]interface{} {
	return probeSpecFor(proto).basicPayload(model)
}

// providerModelsMaxBodyBytes bounds a model-list response. It is far larger
// than providerClientMaxBodyBytes (a test call reads only a tiny ping reply)
// because an aggregator's /v1/models can legitimately enumerate hundreds of
// models and run well past 64KiB.
const providerModelsMaxBodyBytes = 1 << 20

// providerModelsURL builds the model-catalogue endpoint for proto using the
// same version-aware joiner as the credential test, so a base URL with or
// without an explicit /v1 (or /v1beta) segment resolves correctly.
func providerModelsURL(baseURL string, proto protocols.ProtocolID) string {
	return protocols.JoinUpstreamURL(baseURL, probeSpecFor(proto).modelsPath, proto)
}

// providerModelsMaxPages caps how many catalogue pages ListModels follows — a
// backstop against an upstream that returns an endless pagination cursor. Real
// catalogues (a few hundred models at ~100/page) stay well under it.
const providerModelsMaxPages = 20

// ListModels implements ProviderClient. See the interface comment. It reuses
// the credential-test transport (SSRF-safe, no redirects), concurrency cap and
// per-call timeout, and follows pagination (Anthropic has_more/last_id, Gemini
// nextPageToken) within that single timeout. A non-200 first page is classified
// with the same status mapping as a credential test so the UI shows one error
// set — except a 404, which for a catalogue fetch means the upstream simply
// exposes no /models endpoint: that is reported as an empty (successful)
// catalogue so the UI offers manual model entry instead of "model not found".
func (c *HTTPProviderClient) ListModels(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey string) (ListModelsResult, error) {
	if !c.limiter.TryAcquire() {
		return ListModelsResult{}, fmt.Errorf("too many concurrent provider test calls in flight")
	}
	defer c.limiter.Release()

	ctx, cancel := context.WithTimeout(ctx, providerClientTimeout)
	defer cancel()

	baseModelsURL := providerModelsURL(baseURL, proto)
	models := make([]string, 0)
	var nextParam, nextValue string
	start := time.Now()
	// One watcher across every page: the question it answers is "did this
	// destination ever accept a connection", which does not reset per page.
	watcher := &connectionWatcher{}

	for page := 0; page < providerModelsMaxPages; page++ {
		pageURL, err := modelsPageURL(baseModelsURL, nextParam, nextValue)
		if err != nil {
			return ListModelsResult{}, fmt.Errorf("build request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return ListModelsResult{}, fmt.Errorf("build request: %w", err)
		}
		// SetupRequest applies the protocol-correct auth header (Bearer for
		// openai/responses, x-api-key for anthropic, x-goog-api-key for gemini);
		// its Content-Type header is harmless on a bodyless GET.
		requestEncoderFor(proto).SetupRequest(req, apiKey)
		req = watcher.attach(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// First-page transport failure is a real "can't reach it" — unless
			// the budget expired on an established connection, which is a
			// timeout and must be reported as one here too, or a stalled
			// catalogue fetch still sends the operator off to check a URL that
			// is demonstrably fine. On a later page keep the models already
			// gathered rather than throwing away a partial-but-useful
			// catalogue.
			if page == 0 {
				return ListModelsResult{Outcome: timeoutOrUnreachable(ctx, watcher), DurationMs: time.Since(start).Milliseconds()}, nil
			}
			break
		}
		body, ok := readBoundedBodyN(resp, providerModelsMaxBodyBytes)
		_ = resp.Body.Close()
		if !ok {
			if page == 0 {
				// Same re-check as the credential test's: a body that stalled
				// past the budget reads as "unreadable body" here, which would
				// otherwise settle as an upstream error.
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return ListModelsResult{Outcome: TestTimeout, DurationMs: time.Since(start).Milliseconds()}, nil
				}
				return ListModelsResult{Outcome: TestUpstreamError, DurationMs: time.Since(start).Milliseconds(), Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
			}
			break
		}
		if resp.StatusCode == http.StatusNotFound && page == 0 {
			// No catalogue endpoint — an empty catalogue, not a missing model.
			// Fall through to the empty-success return below.
			break
		}
		if resp.StatusCode != http.StatusOK {
			if page == 0 {
				// model="" — none of the model-scoped branches apply here.
				res := classifyResponse(proto, resp, body, "", time.Since(start).Milliseconds())
				return ListModelsResult{Outcome: res.Outcome, DurationMs: res.DurationMs, Detail: res.Detail}, nil
			}
			break
		}
		pageModels, np, nv := parseModelPage(proto, body)
		models = append(models, pageModels...)
		if np == "" {
			break
		}
		nextParam, nextValue = np, nv
	}
	return ListModelsResult{Models: models, Outcome: TestSuccess, DurationMs: time.Since(start).Milliseconds()}, nil
}

// modelsPageURL builds the URL for one catalogue page: baseModelsURL for the
// first page (param ""), or baseModelsURL with the pagination cursor set for a
// later page. net/url does the escaping and ?/& handling, and Set preserves
// any query params already on the base URL.
func modelsPageURL(baseModelsURL, param, value string) (string, error) {
	if param == "" {
		return baseModelsURL, nil
	}
	u, err := url.Parse(baseModelsURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set(param, value)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// parseModelPage extracts one page of model ids from a 200 catalogue body and
// returns the pagination cursor (param name + value) for the next page, or ""
// when there is none — the page shapes live on the probeSpecs entries.
func parseModelPage(proto protocols.ProtocolID, body []byte) (ids []string, nextParam, nextValue string) {
	return probeSpecFor(proto).parseModelPage(body)
}

func (c *HTTPProviderClient) TestChatCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (TestResult, error) {
	payload := chatCompletionPayload(proto, model)
	return c.runTestRequest(ctx, proto, baseURL, apiKey, model, payload, func(resp *http.Response, duration int64) (TestResult, error) {
		body, ok := readBoundedBody(resp)
		if !ok {
			return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
		}
		return classifyResponse(proto, resp, body, model, duration), nil
	})
}

// imageProbePrompt is the minimal generation prompt every image probe sends:
// cheap to generate, no size (so a dialect's default spelling cannot 400 the
// probe before the mapping is judged).
const imageProbePrompt = "a small red square on a white background"

// isDashScopeBase is images.IsDashScopeBase, overridable in tests: a
// local httptest server can never carry the real hostname, and the native
// branch below (origin-joined URL, native body, business-error
// classification) still needs exercising against a live HTTP stub.
var isDashScopeBase = images.IsDashScopeBase

// isArkBase is videos.IsArkBase, overridable in tests for the same
// reason isDashScopeBase is: a local httptest server never carries the
// real hostname.
var isArkBase = videos.IsArkBase

// isKlingBase is videos.IsKlingBase, overridable in tests for the same
// reason isArkBase is.
var isKlingBase = videos.IsKlingBase

// TestImageGeneration probes a mapping the way an image request actually
// reaches the provider: the images endpoint on the provider's base, a
// minimal prompt. A DashScope-compatible base is the exception the
// gateway's image delivery already knows — those hosts serve image models
// through the native multimodal-generation endpoint, reachable from the
// provider's origin rather than its versioned base — so the probe asks the
// same way; asking the OpenAI-shaped images path there would measure a 404
// no routed request would ever hit.
func (c *HTTPProviderClient) TestImageGeneration(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	if isKlingBase(baseURL) {
		return c.testKlingImageGeneration(ctx, baseURL, apiKey, model)
	}
	if isDashScopeBase(baseURL) {
		if isEditShapedModel(model) {
			return c.testDashScopeImageEdit(ctx, baseURL, apiKey, model)
		}
		return c.testDashScopeImageGeneration(ctx, baseURL, apiKey, model)
	}
	if isEditShapedModel(model) {
		return c.testImageEdit(ctx, baseURL, apiKey, model)
	}
	payload := map[string]interface{}{"model": model, "prompt": imageProbePrompt, "n": 1}
	imagesURL := protocols.JoinUpstreamURL(baseURL, "/v1/images/generations", protocols.ProtocolOpenAI)
	return c.runTestRequestAt(ctx, imagesURL, protocols.ProtocolOpenAI, apiKey, model, payload,
		func(resp *http.Response, duration int64) (TestResult, error) {
			body, ok := readBoundedBody(resp)
			if !ok {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
			}
			if resp.StatusCode == http.StatusOK {
				return classifyOpenAIImageProbeBody(body, duration), nil
			}
			return classifyResponse(protocols.ProtocolOpenAI, resp, body, model, duration), nil
		})
}

// isEditShapedModel reports whether a model's name says it is an edit model
// (qwen-image-edit and kin). The edit family requires a reference image — a
// text-only generation probe would measure the family's own input rule, not
// the mapping — so the probe switches to the edits shape for these names. A
// name heuristic, like the import-time modality heuristic it belongs with:
// catalogues say "edit", they do not carry a capability flag for it.
func isEditShapedModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "edit")
}

// editProbeImage is the reference image the edit-shaped probes attach: a
// solid red 64x64 square, rendered with the standard library on first use
// rather than embedded as a byte literal or loaded from a file the binary
// would have to carry. A flat colour compresses to a few hundred PNG bytes,
// cheap enough for a connectivity probe.
var editProbeImage = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{R: 200, G: 30, B: 30, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("providerclient: render edit probe image: " + err.Error())
	}
	return buf.Bytes()
}()

// classifyOpenAIImageProbeBody judges a 200 ONLY by the images shape. The
// generic classifier's 200 validator recognizes chat bodies, so a
// chat-shaped answer on an images path (one that is really serving chat)
// would otherwise certify a mapping that cannot deliver a single image.
func classifyOpenAIImageProbeBody(body []byte, duration int64) TestResult {
	if res, recognized := classifyImageSuccessBody(body, duration); recognized {
		return res
	}
	return TestResult{
		Outcome:    TestUpstreamError,
		DurationMs: duration,
		Detail:     "HTTP 200: response carries no data array",
	}
}

// testImageEdit probes an edit-shaped mapping the way an edits request
// actually reaches an OpenAI-compatible provider: the edits endpoint, a
// multipart upload with the reference image attached.
func (c *HTTPProviderClient) testImageEdit(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for field, value := range map[string]string{
		"model":  model,
		"prompt": imageProbePrompt,
		"n":      "1",
	} {
		if err := w.WriteField(field, value); err != nil {
			return TestResult{}, fmt.Errorf("build image edit probe: %w", err)
		}
	}
	part, err := w.CreateFormFile("image", "probe.png")
	if err != nil {
		return TestResult{}, fmt.Errorf("build image edit probe: %w", err)
	}
	if _, err := part.Write(editProbeImage); err != nil {
		return TestResult{}, fmt.Errorf("build image edit probe: %w", err)
	}
	if err := w.Close(); err != nil {
		return TestResult{}, fmt.Errorf("build image edit probe: %w", err)
	}

	editsURL := protocols.JoinUpstreamURL(baseURL, images.EditPath, protocols.ProtocolOpenAI)
	return c.runRawTestRequestAt(ctx, editsURL, protocols.ProtocolOpenAI, apiKey, w.FormDataContentType(), buf.Bytes(), nil,
		func(resp *http.Response, duration int64) (TestResult, error) {
			body, ok := readBoundedBody(resp)
			if !ok {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
			}
			if resp.StatusCode == http.StatusOK {
				return classifyOpenAIImageProbeBody(body, duration), nil
			}
			return classifyResponse(protocols.ProtocolOpenAI, resp, body, model, duration), nil
		})
}

// testDashScopeImageEdit is the edit-shaped half of the native probe: the
// reference image travels as a base64 data URI content item in the native
// encoding, and the answer is judged by the native dialect's rules.
func (c *HTTPProviderClient) testDashScopeImageEdit(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	native, err := images.EncodeEditRequest(imageProbePrompt, model,
		[]images.EditFile{{FieldName: "image", FileName: "probe.png", ContentType: "image/png", Data: editProbeImage}}, 1, "")
	if err != nil {
		return TestResult{}, fmt.Errorf("encode dashscope image edit probe: %w", err)
	}
	return c.runRawTestRequestAt(ctx, images.UpstreamURL(baseURL), protocols.ProtocolOpenAI, apiKey, "application/json", native, nil,
		func(resp *http.Response, duration int64) (TestResult, error) {
			body, ok := readBoundedBody(resp)
			if !ok {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
			}
			if resp.StatusCode == http.StatusOK {
				if res, ok := classifyDashScopeImageProbeBody(body, duration); ok {
					return res, nil
				}
			}
			return classifyResponse(protocols.ProtocolOpenAI, resp, body, model, duration), nil
		})
}

// testDashScopeImageGeneration runs the probe against the native dialect:
// the body is the native multimodal-generation encoding, and a 200 answer is
// judged ONLY by that dialect's rules — a delivered image passes, a
// 200-carried business code fails with the upstream's own message, and any
// other 200 (unparseable, or no image in it) is an upstream error with the
// decode failure said so. Falling to the shared classifier instead would
// hand the 200 to its chat-body validator, certifying a chat-shaped answer
// as an image mapping. Non-200 statuses do use the shared classification
// (its status mapping is protocol-neutral and its error-detail fallback
// keeps a snippet of the dashscope error body).
func (c *HTTPProviderClient) testDashScopeImageGeneration(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	native, err := images.EncodeRequest(imageProbePrompt, model, 1, "")
	if err != nil {
		return TestResult{}, fmt.Errorf("encode dashscope image probe: %w", err)
	}
	return c.runRawTestRequestAt(ctx, images.UpstreamURL(baseURL), protocols.ProtocolOpenAI, apiKey, "application/json", json.RawMessage(native), nil,
		func(resp *http.Response, duration int64) (TestResult, error) {
			body, ok := readBoundedBody(resp)
			if !ok {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
			}
			if resp.StatusCode == http.StatusOK {
				if res, ok := classifyDashScopeImageProbeBody(body, duration); ok {
					return res, nil
				}
			}
			return classifyResponse(protocols.ProtocolOpenAI, resp, body, model, duration), nil
		})
}

// classifyDashScopeImageProbeBody judges a native-dialect 200 by the
// dialect's own rules: a delivered image passes, a 200-carried business code
// fails with the upstream's own message, any other 200 (unparseable, or no
// image in it) is an upstream error that says so. The boolean says whether
// the body was judged at all, so a non-200 caller can fall to the shared
// status classification.
func classifyDashScopeImageProbeBody(body []byte, duration int64) (TestResult, bool) {
	_, _, decodeErr := images.DecodeResponse(body)
	if decodeErr == nil {
		return TestResult{Outcome: TestSuccess, DurationMs: duration}, true
	}
	var business *images.BusinessError
	if errors.As(decodeErr, &business) {
		return TestResult{
			Outcome:    TestUpstreamError,
			DurationMs: duration,
			Detail:     fmt.Sprintf("HTTP 200: dashscope error %s: %s", business.Code, business.Message),
		}, true
	}
	return TestResult{
		Outcome:    TestUpstreamError,
		DurationMs: duration,
		Detail:     fmt.Sprintf("HTTP 200: %v", decodeErr),
	}, true
}

// classifyImageSuccessBody judges a 200 images answer. A data array — even
// an empty one — proves the endpoint recognized the request; the gateway's
// own delivery rules handle an empty answer at traffic time, and a mapping
// probe only owes the verdict that the mapping works. A 200 whose body
// parses as JSON but carries no data array (a chat-shaped answer, an
// unrelated document) is NOT an images answer — the pointer stays nil and
// the caller falls to the generic classifier instead of certifying a
// mapping that cannot deliver.
func classifyImageSuccessBody(body []byte, durationMs int64) (TestResult, bool) {
	var parsed struct {
		Data *[]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Data == nil {
		return TestResult{}, false
	}
	return TestResult{Outcome: TestSuccess, DurationMs: durationMs}, true
}

// streamChunk mirrors the minimal OpenAI streaming chunk shape this test
// needs to recognize a structurally valid delta — same "don't trust a 200
// status code alone" principle as isValidOpenAIChatSuccessBody. Only OpenAI
// SSE deltas are parsed this deeply; other protocols' stream shapes
// (Claude's content_block_delta events, etc.) are deferred — see
// TestStreamingCompletion.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
}

// hasContent reports whether the chunk carries any produced content — either
// a regular content delta or a reasoning_content delta (OpenAI-compatible
// reasoning models stream delta.reasoning_content, often with little or no
// delta.content).
func (c streamChunk) hasContent() bool {
	if len(c.Choices) == 0 {
		return false
	}
	d := c.Choices[0].Delta
	return d.Content != "" || d.ReasoningContent != ""
}

// scanSSEStream reads a bounded number of bytes off r, reporting whether it
// saw at least one produced content delta (sawValidDelta — content or
// reasoning_content, not a bare role prelude) and whether the stream ended
// cleanly (cleanTerminate — an explicit `data: [DONE]` marker, or a normal
// EOF with no read error). A read error mid-stream (context timeout, TCP
// reset) leaves cleanTerminate false, so a broken endpoint that emits one
// delta then hangs/resets is not misclassified as streaming-capable.
// Non-"data:" lines (blank lines, comments) are skipped. Many OpenAI-
// compatible upstreams omit [DONE]; a clean EOF still counts as valid
// termination, so those working streams pass.
func scanSSEStream(r io.Reader) (sawValidDelta, cleanTerminate bool) {
	scanner := bufio.NewScanner(io.LimitReader(r, providerClientMaxBodyBytes))
	for scanner.Scan() {
		line := scanner.Text()
		// Classified with the strict, non-trimming reading — the same one the
		// live forwarder answers with. This probe persists a capability the
		// forwarding path then has to honour: accepting an indented line here
		// that forwarding treats as preamble would record a streaming-capable
		// endpoint whose real stream never commits a data frame.
		start, ok := protocols.SSEDataPayloadStart([]byte(line))
		if !ok {
			continue // not an SSE data line
		}
		data := strings.TrimSpace(line[start:])
		if data == "[DONE]" {
			return sawValidDelta, true
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.hasContent() {
			sawValidDelta = true
			// Keep scanning to observe how the stream terminates — a content
			// delta alone is not enough; the upstream must also close cleanly.
		}
	}
	if scanner.Err() != nil {
		// A read error (context timeout mid-stream, TCP reset) is not a clean
		// termination — a broken endpoint that emits a delta then hangs/reset
		// must not be certified as streaming-capable.
		return sawValidDelta, false
	}
	// scanner stopped at EOF with no read error. That could be a clean upstream
	// close OR the LimitReader cap (the stream still had data). Read one byte
	// past the cap to tell them apart — a byte means the stream exceeded the
	// cap (not cleanly terminated; a max_tokens=1 probe should never reach
	// 64 KiB), EOF means the upstream closed.
	var b [1]byte
	n, _ := io.ReadFull(r, b[:])
	return sawValidDelta, n == 0
}

// streamingCompletionPayload mirrors chatCompletionPayload with stream
// enabled where the protocol's wire format needs an explicit flag — see the
// per-protocol notes on the probeSpecs entries.
func streamingCompletionPayload(proto protocols.ProtocolID, model string) map[string]interface{} {
	return probeSpecFor(proto).streamingPayload(model)
}

// openAIStreamBody is the real streaming validator: success requires BOTH a
// produced content delta (content or reasoning_content — a bare role prelude
// is not enough) AND a clean termination (explicit `data: [DONE]` or a normal
// EOF). A delta followed by a hang/reset must not certify streaming — the
// endpoint would leave real clients hanging. Many OpenAI-compatible upstreams
// omit [DONE]; a clean EOF still counts as valid termination.
func openAIStreamBody(resp *http.Response, durationMs int64) (bool, string) {
	sawValidDelta, cleanTerminate := scanSSEStream(resp.Body)
	if sawValidDelta && cleanTerminate {
		return true, ""
	}
	return false, fmt.Sprintf("openai stream incomplete (content_delta=%v, clean_terminate=%v, %dms)", sawValidDelta, cleanTerminate, durationMs)
}

// unverifiedStreamPass is the deferred streaming validator for entries whose
// SSE delta shape (Claude's content_block_delta/message_stop events, Gemini's
// chunked generateContent stream, Responses' response.output_text.delta
// events) is not yet modelled. A 200 with a non-empty body is treated as a
// structurally-unverified pass — it proves the endpoint/auth/payload shape
// was not rejected outright, nothing more.
func unverifiedStreamPass(resp *http.Response, _ int64) (bool, string) {
	body, ok := readBoundedBody(resp)
	if !ok || len(body) == 0 {
		return false, "HTTP 200 with empty streaming body"
	}
	return true, ""
}

func (c *HTTPProviderClient) TestStreamingCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (TestResult, error) {
	payload := streamingCompletionPayload(proto, model)
	return c.runTestRequest(ctx, proto, baseURL, apiKey, model, payload, func(resp *http.Response, duration int64) (TestResult, error) {
		if resp.StatusCode != http.StatusOK {
			body, ok := readBoundedBody(resp)
			if !ok {
				return TestResult{Outcome: TestUpstreamError, DurationMs: duration, Detail: fmt.Sprintf("HTTP %d (response body unreadable)", resp.StatusCode)}, nil
			}
			return classifyResponse(proto, resp, body, model, duration), nil
		}

		ok, detail := probeSpecFor(proto).validStreamBody(resp, duration)
		if ok {
			return TestResult{Outcome: TestSuccess, DurationMs: duration}, nil
		}
		logger.Warn("provider test: streaming validation failed",
			zap.String("proto", string(proto)),
			zap.String("base_url", protocols.RedactURL(baseURL)),
			zap.String("model", model),
			zap.Int64("duration_ms", duration),
			zap.String("detail", detail))
		return TestResult{Outcome: TestUpstreamError, DurationMs: duration, Detail: detail}, nil
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
// enough (mirrors isValidOpenAIChatSuccessBody's "don't trust the status
// code alone" principle, applied to the tool-call response shape). This is
// OpenAI's own tool_calls shape; see TestFunctionCalling for other
// protocols.
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

// weatherToolDescription/weatherToolParameters are the shared "get_weather"
// tool definition content reused (in each protocol's own shape) across
// functionCallingPayload's branches.
const (
	weatherToolName        = "get_weather"
	weatherToolDescription = "Get the current weather for a location"
)

func weatherToolParameters() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"location": map[string]string{"type": "string"}},
		"required":   []string{"location"},
	}
}

// functionCallingPayload builds the minimal tool-calling credential test
// body per protocol; the response-side deferrals for gemini/responses are
// noted on the probeSpecs entries — see TestFunctionCalling.
func functionCallingPayload(proto protocols.ProtocolID, model string) map[string]interface{} {
	return probeSpecFor(proto).functionCallingPayload(model)
}

func (c *HTTPProviderClient) TestFunctionCalling(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (TestResult, error) {
	payload := functionCallingPayload(proto, model)
	return c.runTestRequest(ctx, proto, baseURL, apiKey, model, payload, func(resp *http.Response, duration int64) (TestResult, error) {
		body, ok := readBoundedBody(resp)
		if !ok {
			return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
		}
		if resp.StatusCode != http.StatusOK {
			return classifyResponse(proto, resp, body, model, duration), nil
		}
		if !probeSpecFor(proto).validToolCallBody(body) {
			return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
		}
		return TestResult{Outcome: TestSuccess, DurationMs: duration}, nil
	})
}

// classifyResponse maps a non-streaming test/list response to a TestResult
// and, for every non-success outcome, attaches an admin-facing Detail string
// (HTTP status + the upstream's own error message when present). The
// outcome classification itself lives in classifyResponseByStatus.
func classifyResponse(proto protocols.ProtocolID, resp *http.Response, body []byte, model string, durationMs int64) TestResult {
	res := classifyResponseByStatus(proto, resp, body, model, durationMs)
	if res.Outcome != TestSuccess && res.Outcome != TestVerificationUnsupported {
		res.Detail = upstreamErrorDetail(proto, resp.StatusCode, body)
	}
	return res
}

// upstreamErrorDetail builds the concise, admin-facing diagnostic string for
// a failed response: the HTTP status, plus the upstream's own error message
// (truncated) when the body carries a recognizable one.
// maxUpstreamDetailBytes bounds the upstream error message kept in a Detail
// string — long enough to be diagnostic, short enough not to bloat the field.
const maxUpstreamDetailBytes = 300

func upstreamErrorDetail(proto protocols.ProtocolID, statusCode int, body []byte) string {
	msg := extractUpstreamMessage(proto, body)
	if msg == "" {
		return fmt.Sprintf("HTTP %d", statusCode)
	}
	if len(msg) > maxUpstreamDetailBytes {
		msg = truncateRuneSafe(msg, maxUpstreamDetailBytes) + "…"
	}
	return fmt.Sprintf("HTTP %d: %s", statusCode, msg)
}

// extractUpstreamMessage pulls the human-readable error text from an upstream
// error body, falling back to a trimmed snippet of the raw body for shapes
// this codebase does not model (gemini errors, plain-text gateways, etc.).
func extractUpstreamMessage(proto protocols.ProtocolID, body []byte) string {
	if m := probeSpecFor(proto).extractMessage(body); m != "" {
		return m
	}
	// Raw-body fallback: cap the byte slice BEFORE converting so an
	// unrecognized megabyte-sized error page (the ListModels path reads up to
	// 1 MiB) isn't fully allocated as a string just to keep a short snippet.
	raw := body
	if len(raw) > maxUpstreamDetailBytes {
		raw = raw[:maxUpstreamDetailBytes]
	}
	return strings.TrimSpace(truncateRuneSafe(string(raw), maxUpstreamDetailBytes))
}

// truncateRuneSafe returns s truncated to at most maxBytes bytes, backing off
// to the nearest whole-rune boundary so the result is always valid UTF-8 —
// never a multi-byte character (e.g. Chinese, common in upstream error
// messages) sliced in half. It also strips a trailing partial rune left by a
// caller's own byte-slice. No truncation marker is appended; callers add one.
func truncateRuneSafe(s string, maxBytes int) string {
	if len(s) > maxBytes {
		s = s[:maxBytes]
	}
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// classifyResponseByStatus maps an HTTP response into one of the 9 TestOutcome
// categories. The status-code switch itself is protocol-neutral — every
// protocol here uses the same HTTP status conventions (401 auth, 403
// permission, 404 not-found, 429 rate/quota, 5xx upstream) — only the
// body-shape predicates it calls out to (isValidSuccessBody,
// isModelScopedError, isQuotaError, isModelNotFoundError) branch on proto.
func classifyResponseByStatus(proto protocols.ProtocolID, resp *http.Response, body []byte, model string, durationMs int64) TestResult {
	switch resp.StatusCode {
	case http.StatusOK:
		if !probeSpecFor(proto).successCertifiable {
			// The entry declares its success-body validator is only the
			// leniency check — a 2xx proves the request wasn't rejected
			// outright, not that the credential actually works. Treating
			// that as TestSuccess would let a key be authorized against a
			// destination that was never truly verified, so it is reported
			// as "cannot certify yet" instead; real error statuses below
			// stay meaningful for these protocols too.
			return TestResult{Outcome: TestVerificationUnsupported, DurationMs: durationMs}
		}
		if isValidSuccessBody(proto, resp, body) {
			return TestResult{Outcome: TestSuccess, DurationMs: durationMs}
		}
		return TestResult{Outcome: TestUpstreamError, DurationMs: durationMs}
	case http.StatusUnauthorized:
		return TestResult{Outcome: TestAuthFailed, DurationMs: durationMs}
	case http.StatusForbidden:
		modelScoped := isModelScopedError(proto, body, model)
		return TestResult{Outcome: TestPermissionDenied, DurationMs: durationMs, IsModelScoped: modelScoped}
	case http.StatusNotFound:
		return TestResult{Outcome: TestModelNotFound, DurationMs: durationMs}
	case http.StatusTooManyRequests:
		if isQuotaError(proto, body) {
			return TestResult{Outcome: TestQuotaUnavailable, DurationMs: durationMs}
		}
		return TestResult{Outcome: TestRateLimited, DurationMs: durationMs}
	default:
		if resp.StatusCode >= 500 {
			return TestResult{Outcome: TestUpstreamError, DurationMs: durationMs}
		}
		if isModelNotFoundError(proto, body) {
			return TestResult{Outcome: TestModelNotFound, DurationMs: durationMs}
		}
		return TestResult{Outcome: TestUpstreamError, DurationMs: durationMs}
	}
}

// isValidSuccessBody enforces the "success cannot be judged by the status
// code alone" rule: the shared content-type check, then the entry's own
// body-shape validator. classifyResponseByStatus consults the entry's
// successCertifiable field first, so an entry carrying only the leniency
// validator never has a 200 certified through this path.
func isValidSuccessBody(proto protocols.ProtocolID, resp *http.Response, body []byte) bool {
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(contentType), "application/json") {
		return false
	}
	return probeSpecFor(proto).validSuccessBody(body)
}

// isValidOpenAIChatSuccessBody is the original OpenAI Chat Completions
// success-body check: the body must parse, carry no top-level error, and
// have at least one choice with a non-null message.
func isValidOpenAIChatSuccessBody(body []byte) bool {
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

// isValidClaudeSuccessBody requires the Anthropic Messages API success
// shape: "type":"message" with a non-empty content array. A "type":"error"
// body (or anything else) is rejected outright by the type check.
func isValidClaudeSuccessBody(body []byte) bool {
	var success claudeSuccessBody
	if err := json.Unmarshal(body, &success); err != nil {
		return false
	}
	return success.Type == "message" && len(success.Content) > 0
}

// isValidGeminiSuccessBody requires a generateContent success shape: no
// top-level error, and at least one candidate whose content carries a part
// the runtime decoder actually delivers (non-empty text, or a functionCall
// with a name) — proof the model generated something this gateway can relay.
// Candidates without content (e.g. a safety block reported inside a 200),
// placeholder parts (null, {}, empty nested objects) and media-only parts
// (inlineData, which the runtime decoder does not deliver) do not certify.
func isValidGeminiSuccessBody(body []byte) bool {
	var parsed geminiSuccessBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	if parsed.Error != nil {
		return false
	}
	for _, cand := range parsed.Candidates {
		if cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part.Text != "" {
				return true
			}
			if part.FunctionCall != nil && part.FunctionCall.Name != "" {
				return true
			}
		}
	}
	return false
}

// isValidResponsesSuccessBody requires an OpenAI Responses API success
// shape: no non-null top-level error, a status the runtime decoders would
// serve (responses.StatusIsNonServed is the single shared blacklist —
// compatible relays may omit status entirely, and an absent or unknown
// value must not fail a body that carries real output), and a "message"
// output item with non-empty content. Reasoning-only, placeholder (null,
// {}) and empty-message items do not certify. The probe never requests a
// background run, so requiring a delivered message is safe —
// queued/in_progress bodies without one do not certify a credential.
func isValidResponsesSuccessBody(body []byte) bool {
	var parsed responsesSuccessBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	if parsed.Error != nil {
		return false
	}
	if responses.StatusIsNonServed(parsed.Status) {
		return false
	}
	for _, item := range parsed.Output {
		if item.Type != "message" {
			continue
		}
		// Only the content types the runtime decoder reads count as delivered
		// output; a placeholder element (null, {}) or an unknown type proves
		// nothing about the credential.
		for _, part := range item.Content {
			switch part.Type {
			case "output_text":
				if part.Text != "" {
					return true
				}
			case "refusal":
				if part.Refusal != "" {
					return true
				}
			}
		}
	}
	return false
}

// isParseableJSONObjectWithoutError is the leniency check kept for the
// probe paths whose body shapes are still unmodelled: the body must parse
// as a JSON object and carry no top-level "error" field, nothing deeper.
// Used by the claude/gemini/responses tool-call classification (their
// tool-call body shapes remain a deferred concern; the basic credential
// test now has real per-protocol success validators).
func isParseableJSONObjectWithoutError(body []byte) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	_, hasError := obj["error"]
	return !hasError
}

func parseErrorBody(body []byte) *chatCompletionErrorBody {
	var parsed chatCompletionErrorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	return &parsed
}

func parseClaudeErrorBody(body []byte) *claudeErrorBody {
	var parsed claudeErrorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	return &parsed
}

// isModelScopedError reports whether a 403 body structurally names the
// tested model as the reason. OpenAI carries a structured error.param
// field for this; Anthropic doesn't, so only the message-text heuristic
// applies there. gemini/responses body parsing is deferred, so this always
// reports false for them — IsModelScoped only affects verification_status
// write rules, never whether the test itself passed or failed.
func isModelScopedError(proto protocols.ProtocolID, body []byte, model string) bool {
	return probeSpecFor(proto).modelScopedError(body, model)
}

func isQuotaError(proto protocols.ProtocolID, body []byte) bool {
	return probeSpecFor(proto).quotaError(body)
}

func isModelNotFoundError(proto protocols.ProtocolID, body []byte) bool {
	return probeSpecFor(proto).modelNotFoundError(body)
}
