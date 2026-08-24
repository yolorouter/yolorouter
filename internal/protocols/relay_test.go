package protocols_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/chat"
	"github.com/yolorouter/yolorouter/internal/protocols/claude"
	"github.com/yolorouter/yolorouter/internal/protocols/gemini"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ctxCancelAfterEOFBody simulates the real behavior of a Go HTTP client after
// the client's context is canceled: the upstream has already sent the
// complete SSE stream (including [DONE]) and reached EOF, but **the next Read
// call returns ctx.Err()** instead of the normal io.EOF. This mirrors the
// production path: an HTTP/2 transport's internal reader returns ctx.Err()
// rather than io.EOF once it observes the request context has been canceled.
type ctxCancelAfterEOFBody struct {
	r io.Reader
}

func (b *ctxCancelAfterEOFBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		// After the upstream has fully finished, the next Read gets
		// ctx.Err() (not EOF).
		return n, context.Canceled
	}
	return n, err
}

func (b *ctxCancelAfterEOFBody) Close() error { return nil }

// TestIRStreamRelay_TreatsCtxCanceledAfterDoneAsSuccess is a regression test
// for a real production incident: the upstream SSE stream ended completely,
// including finish_reason and [DONE], and all token usage was captured, but
// the client closed the connection before EOF, so the subsequent Read on
// resp.Body got context.Canceled. The old IRStreamRelay implementation
// misreported this as "upstream stream read error: context canceled",
// causing the caller to rewrite the response as a 502 — the user wasn't
// billed and dispatch logs recorded a spurious conversion error. After the
// fix: as long as the decoder emitted a DeltaDone in the main loop (from
// [DONE] or finish_reason), the ctx.Err() should be ignored and the request
// settled as success (nil error).
func TestIRStreamRelay_TreatsCtxCanceledAfterDoneAsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	// Build a complete OpenAI chat-completions-style SSE: finish_reason=stop + usage + [DONE].
	upstream := strings.NewReader(strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		``,
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &ctxCancelAfterEOFBody{r: upstream},
		Header:     http.Header{},
	}

	usage, err := protocols.IRStreamRelay(protocols.NewGinClientWriter(c), resp,
		chat.NewStreamDecoder(),
		chat.NewStreamEncoder(),
		nil, nil, nil)
	require.NoError(t, err,
		"a ctx.Err() caused by the client disconnecting after [DONE] must be swallowed, otherwise the caller would rewrite it as a 502 and misreport a conversion error")
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 2, usage.CompletionTokens)
}

// TestIRStreamRelay_PropagatesCtxCanceledBeforeDone asserts the inverse case:
// if the upstream is canceled before it has sent [DONE] / finish_reason at
// all, the relay must still report an error on the failure path (the new
// leniency must not silently swallow a genuine upstream interruption and
// settle it as success, which would bill the caller incorrectly).
func TestIRStreamRelay_PropagatesCtxCanceledBeforeDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	// No finish_reason, no [DONE] — only the opening chunk was sent before cancellation.
	upstream := strings.NewReader(strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		``,
		``,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &ctxCancelAfterEOFBody{r: upstream},
		Header:     http.Header{},
	}

	_, err := protocols.IRStreamRelay(protocols.NewGinClientWriter(c), resp,
		chat.NewStreamDecoder(),
		chat.NewStreamEncoder(),
		nil, nil, nil)
	require.Error(t, err,
		"a ctx-canceled before [DONE] / finish_reason arrived is a genuine upstream interruption and must report an error on the failure path")
	assert.True(t, errors.Is(err, context.Canceled),
		"the returned error must preserve context.Canceled so the caller can identify it")
}

// TestIRStreamRelay_FlushesDecoderBufferBeforeReadErrorCheck covers a case
// where the upstream's last line ([DONE]) lacks a trailing blank line, so
// the OpenAI chat decoder still hasn't emitted DeltaDone from its internal
// buffer. If the read error were checked directly off scanner.Err with
// sawDone still false, a terminal frame that fully arrived would be
// misclassified as a failure. The fix requires calling decoder.Finish() to
// flush the buffer before judging the read error.
func TestIRStreamRelay_FlushesDecoderBufferBeforeReadErrorCheck(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	// The upstream's last line, [DONE], is followed by only a single \n (not
	// \n\n), so the chat decoder's internal buffer still holds
	// `data: [DONE]\n` and won't emit DeltaDone in the main loop.
	upstream := strings.NewReader(strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		``,
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
		``,
		`data: [DONE]`,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &ctxCancelAfterEOFBody{r: upstream},
		Header:     http.Header{},
	}

	usage, err := protocols.IRStreamRelay(protocols.NewGinClientWriter(c), resp,
		chat.NewStreamDecoder(),
		chat.NewStreamEncoder(),
		nil, nil, nil)
	require.NoError(t, err,
		"after the main loop ends, decoder.Finish() must flush the buffered [DONE] before judging scanner.Err, "+
			"otherwise a missing trailing blank line combined with a ctx-canceled error would misclassify a complete stream as failed")
	require.NotNil(t, usage)
	assert.Equal(t, 5, usage.PromptTokens)
	assert.Equal(t, 1, usage.CompletionTokens)
}

// TestIRStreamRelay_ClaudeMessageStopOnly_CtxCanceledAfterEOF covers an
// Anthropic-compatible upstream endpoint that skips message_delta and sends
// message_stop directly. For this "fully complete but no DeltaDone" stream
// shape, if the client closes only after EOF, scanner.Err gets a
// ctx-canceled error. The fix requires the Claude decoder's Finish() to emit
// a DeltaDone (not just DeltaUsage) so the relay helper's sawDone flag can
// recognize the stream as fully complete.
func TestIRStreamRelay_ClaudeMessageStopOnly_CtxCanceledAfterEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	// A real Anthropic-compatible endpoint sequence: message_start
	// (input_tokens=10) -> content_block_* -> message_stop (**no
	// message_delta**).
	upstream := strings.NewReader(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","model":"deepseek-v4-pro","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
		``,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &ctxCancelAfterEOFBody{r: upstream},
		Header:     http.Header{},
	}

	usage, err := protocols.IRStreamRelay(protocols.NewGinClientWriter(c), resp,
		claude.NewStreamDecoder(),
		claude.NewStreamEncoder(),
		nil, nil, nil)
	require.NoError(t, err,
		"a complete Claude message_stop (with no message_delta) followed by a ctx-canceled after EOF must settle as success")
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.PromptTokens, "message_start's input_tokens must be propagated to usage")
}

// unexpectedEOFAfterFinishedBody simulates another way an HTTP/2 transport
// can behave when the client cancels: the upstream SSE has already sent a
// complete [DONE], but Read returns io.ErrUnexpectedEOF instead of io.EOF.
// This is a common wrapped error net/http produces when a chunked transfer
// is cut off mid-stream.
type unexpectedEOFAfterFinishedBody struct {
	r io.Reader
}

func (b *unexpectedEOFAfterFinishedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (b *unexpectedEOFAfterFinishedBody) Close() error { return nil }

// TestIRStreamRelay_TreatsUnexpectedEOFAfterDoneAsSuccess covers the case
// where an HTTP/2 transport returns a wrapped io.ErrUnexpectedEOF instead of
// context.Canceled when the client cancels. This kind of read error is still
// benign once a complete [DONE] / finish_reason has already been received.
func TestIRStreamRelay_TreatsUnexpectedEOFAfterDoneAsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	upstream := strings.NewReader(strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &unexpectedEOFAfterFinishedBody{r: upstream},
		Header:     http.Header{},
	}

	usage, err := protocols.IRStreamRelay(protocols.NewGinClientWriter(c), resp,
		chat.NewStreamDecoder(),
		chat.NewStreamEncoder(),
		nil, nil, nil)
	require.NoError(t, err,
		"an io.ErrUnexpectedEOF returned by the transport after [DONE] is also a benign trailing error and must settle as success")
	require.NotNil(t, usage)
	assert.Equal(t, 7, usage.PromptTokens)
	assert.Equal(t, 3, usage.CompletionTokens)
}

// TestIRStreamRelay_FailsWhenCanceledBetweenFinishReasonAndUsageChunk covers
// the case where, under the OpenAI include_usage protocol, finish_reason and
// the final usage chunk arrive as **two separate SSE frames**. If the client
// disconnects after finish_reason but before usage, exempting the read error
// based on sawDone alone would let a request with encoder.Usage() still zero
// be misclassified as success, undercharging the caller and leaving
// provider_cost missing. The fix requires both conditions — sawDone AND
// usage fully collected — to be satisfied before exempting the error.
func TestIRStreamRelay_FailsWhenCanceledBetweenFinishReasonAndUsageChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	// finish_reason arrives in the previous frame, then EOF before usage
	// arrives (immediately followed by ctx-canceled overriding the EOF).
	upstream := strings.NewReader(strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		``,
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":null}`,
		``,
		``,
		// Neither the final usage chunk nor [DONE] arrives before the connection is cut.
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &ctxCancelAfterEOFBody{r: upstream},
		Header:     http.Header{},
	}

	_, err := protocols.IRStreamRelay(protocols.NewGinClientWriter(c), resp,
		chat.NewStreamDecoder(),
		chat.NewStreamEncoder(),
		nil, nil, nil)
	require.Error(t, err,
		"a ctx-canceled after finish_reason arrived but before usage arrived must be treated as a failure, otherwise it would undercharge and drop provider_cost")
}

// TestIRStreamRelayJSONLines_FailsWhenCanceledWithoutUsage symmetrically
// covers the sawDone + missing-usage scenario for the JSON-lines path. A
// Gemini streaming upstream using `alt=sse` actually returns standard SSE
// (`data: ...\n\n`); IRStreamRelayJSONLines feeds the decoder line by line so
// it frames on \n\n. This test needs to trigger the window where
// "finishReason has arrived but usageMetadata has not", so the two are split
// into separate frames.
func TestIRStreamRelayJSONLines_FailsWhenCanceledWithoutUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	// Real Gemini SSE format: each frame is data: <json>\n\n. finishReason
	// arrives in the previous frame, then EOF before usageMetadata arrives
	// (followed by ctx-canceled) — exactly the shape needed to separately
	// verify the three conditions: sawDone=true + usage missing + benign
	// read error.
	upstream := strings.NewReader(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":""}],"role":"model"},"finishReason":"STOP"}]}`,
		``,
		``,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &ctxCancelAfterEOFBody{r: upstream},
		Header:     http.Header{},
	}

	_, err := protocols.IRStreamRelayJSONLines(protocols.NewGinClientWriter(c), resp,
		gemini.NewStreamDecoder(),
		gemini.NewStreamEncoder(),
		nil, nil, nil)
	require.Error(t, err,
		"a Gemini finishReason without usageMetadata plus ctx-canceled must be treated as a failure, aligning with IRStreamRelay's behavior; "+
			"otherwise it would undercharge and dispatch logs would lose provider_cost")
}

// TestIRStreamRelayJSONLines_SuccessWhenFinishAndUsageBothArrive is the
// positive counterpart: once both finishReason and usageMetadata have
// arrived in the Gemini SSE stream (whether in the same frame or split
// across two), a subsequent client disconnect must still settle as success.
// This verifies the tightened sawDone+usage condition does not misclassify
// a normally completed stream as a failure.
func TestIRStreamRelayJSONLines_SuccessWhenFinishAndUsageBothArrive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	// finishReason + usageMetadata in the same frame (a real Gemini
	// scenario), followed by EOF -> ctx-canceled.
	upstream := strings.NewReader(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`,
		``,
		``,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &ctxCancelAfterEOFBody{r: upstream},
		Header:     http.Header{},
	}

	usage, err := protocols.IRStreamRelayJSONLines(protocols.NewGinClientWriter(c), resp,
		gemini.NewStreamDecoder(),
		gemini.NewStreamEncoder(),
		nil, nil, nil)
	require.NoError(t, err,
		"a client disconnect after both finishReason and usageMetadata have arrived must settle as success, otherwise a normal Gemini completion would be misclassified as a failure")
	require.NotNil(t, usage)
	assert.Equal(t, 7, usage.PromptTokens)
	assert.Equal(t, 2, usage.CompletionTokens)
	assert.Equal(t, 9, usage.TotalTokens)
}

// TestIRStreamRelay_RequiresSawDoneBeforeSynthesizingSuccess is a regression
// test for a real correctness bug: previously EncodeDone() (the client
// success terminator) was emitted whenever finishErr == nil, even on a
// clean EOF before any DeltaDone (an empty 200, or an upstream that closes
// mid-completion) — wrongly reporting success. The fix requires
// sig.SawDone as an additional precondition: a stream that never saw a
// done signal must return a non-nil error and must NOT emit the client's
// [DONE] terminator.
func TestIRStreamRelay_RequiresSawDoneBeforeSynthesizingSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	// A plain text delta with NO finish_reason and NO [DONE] — the upstream
	// closes cleanly (real io.EOF) before ever signaling completion.
	upstream := strings.NewReader(strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		``,
		``,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(upstream),
		Header:     http.Header{},
	}

	_, err := protocols.IRStreamRelay(protocols.NewGinClientWriter(c), resp,
		chat.NewStreamDecoder(),
		chat.NewStreamEncoder(),
		nil, nil, nil)
	require.Error(t, err, "a clean EOF before any done signal must not be synthesized into success")
	assert.NotContains(t, w.Body.String(), "[DONE]", "the client must not receive the success terminator for a truncated stream")
}

// TestIRStreamRelay_CompleteStreamStillSucceeds is the positive counterpart
// to the regression above: a genuinely complete stream (finish_reason +
// [DONE]) must still settle as success and emit [DONE] to the client,
// confirming the sawDone gate does not regress the normal path — this also
// pins the shape the existing cross-protocol integration test
// (the gateway's cross-protocol suite, complete Claude SSE with message_stop)
// depends on.
func TestIRStreamRelay_CompleteStreamStillSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	upstream := strings.NewReader(strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		``,
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(upstream),
		Header:     http.Header{},
	}

	usage, err := protocols.IRStreamRelay(protocols.NewGinClientWriter(c), resp,
		chat.NewStreamDecoder(),
		chat.NewStreamEncoder(),
		nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Contains(t, w.Body.String(), "[DONE]", "a complete stream must still emit the client's success terminator")
}

// TestIRStreamRelay_EmptyUpstreamReturnsErrorWithoutCommittingHeaders is a
// regression test for pre-first-event candidate failover: when the upstream
// returns a 200 status but the body is completely empty (no encoded event is
// ever produced before EOF), IRStreamRelay must return an error WITHOUT
// having written the SSE response headers to the client. Before the fix, the
// 200 + text/event-stream headers were written unconditionally at the top of
// the function, so a pre-first-event failure like this one had already
// committed the response and could never fail over to a healthy candidate.
func TestIRStreamRelay_EmptyUpstreamReturnsErrorWithoutCommittingHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
	}

	var sawFirstChunk bool
	_, err := protocols.IRStreamRelay(protocols.NewGinClientWriter(c), resp,
		chat.NewStreamDecoder(),
		chat.NewStreamEncoder(),
		nil, func() { sawFirstChunk = true }, nil)
	require.Error(t, err, "an empty 200 upstream response must be reported as an error, not synthesized into success")
	assert.False(t, sawFirstChunk, "onFirstChunk must not fire when no event was ever emitted")
	assert.Empty(t, w.Header().Get("Content-Type"), "the SSE headers must not be committed to the client when no event was ever emitted, so the caller can still fail over to another candidate")
	assert.Empty(t, w.Body.String(), "nothing should have been written to the client")
	assert.False(t, w.Flushed, "the writer must not have been flushed before headers were committed")
}

// TestIRStreamRelayJSONLines_EmptyUpstreamReturnsErrorWithoutCommittingHeaders
// is the JSON-lines counterpart of
// TestIRStreamRelay_EmptyUpstreamReturnsErrorWithoutCommittingHeaders.
func TestIRStreamRelayJSONLines_EmptyUpstreamReturnsErrorWithoutCommittingHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
	}

	var sawFirstChunk bool
	_, err := protocols.IRStreamRelayJSONLines(protocols.NewGinClientWriter(c), resp,
		gemini.NewStreamDecoder(),
		gemini.NewStreamEncoder(),
		nil, func() { sawFirstChunk = true }, nil)
	require.Error(t, err, "an empty 200 upstream response must be reported as an error, not synthesized into success")
	assert.False(t, sawFirstChunk, "onFirstChunk must not fire when no event was ever emitted")
	assert.Empty(t, w.Header().Get("Content-Type"), "the SSE headers must not be committed to the client when no event was ever emitted, so the caller can still fail over to another candidate")
	assert.Empty(t, w.Body.String(), "nothing should have been written to the client")
	assert.False(t, w.Flushed, "the writer must not have been flushed before headers were committed")
}

// TestIRStreamRelayJSONLines_RequiresSawDoneBeforeSynthesizingSuccess is the
// JSON-lines counterpart of
// TestIRStreamRelay_RequiresSawDoneBeforeSynthesizingSuccess: a Gemini-style
// upstream that closes cleanly without ever emitting finishReason must
// return an error, not be synthesized into success.
func TestIRStreamRelayJSONLines_RequiresSawDoneBeforeSynthesizingSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	// A content delta with no finishReason at all — the upstream closes
	// cleanly (real io.EOF) before ever signaling completion.
	upstream := strings.NewReader(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"}}]}`,
		``,
		``,
	}, "\n"))

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(upstream),
		Header:     http.Header{},
	}

	_, err := protocols.IRStreamRelayJSONLines(protocols.NewGinClientWriter(c), resp,
		gemini.NewStreamDecoder(),
		gemini.NewStreamEncoder(),
		nil, nil, nil)
	require.Error(t, err, "a clean EOF before any done signal must not be synthesized into success")
}

// TestIsBenignPostDoneReadErr_Whitelist locks down which read errors count
// as benign trailing errors when sawDone=true. Callers must first verify
// sawDone=true before using this function — this test only covers the
// allowlist of the error types themselves.
func TestIsBenignPostDoneReadErr_Whitelist(t *testing.T) {
	cases := []struct {
		err    error
		benign bool
	}{
		{context.Canceled, true},
		{io.ErrUnexpectedEOF, true},
		// non-standard HTTP/2 stream close by some upstreams
		{errors.New("http2: response body closed"), true},
		// must still match when wrapped
		{fmt.Errorf("read body: %w", errors.New("http2: response body closed")), true},
		// gateway.ErrIdleTimeout ("idle timeout between chunks"): some 2xx
		// upstreams send [DONE] + final usage and then just hold the
		// connection open instead of closing it — the idle-read timeout that
		// eventually fires on an otherwise fully-delivered stream is benign,
		// not a genuine interruption. Matched by string (see the doc comment
		// on IsBenignPostDoneReadErr) since this package cannot import
		// internal/gateway.
		{errors.New("idle timeout between chunks"), true},
		// gateway.ErrFirstByteTimeout ("first byte timeout") is deliberately
		// NOT exempted: by the time sawDone=true has been observed, the
		// first byte has necessarily already arrived, so a first-byte
		// timeout can never legitimately fire post-DONE — if it somehow did,
		// it must not be silently swallowed as success.
		{errors.New("first byte timeout"), false},
		{io.EOF, false}, // a normal EOF should not take this exemption path (handled earlier via errors.Is(rawReadErr, io.EOF))
		{nil, false},    // defensive: nil must never be treated as benign
		{errors.New("upstream returned 502"), false}, // a genuine upstream error is never exempted
	}
	for _, tc := range cases {
		got := protocols.IsBenignPostDoneReadErr(tc.err)
		assert.Equal(t, tc.benign, got, "err=%v", tc.err)
	}
}
