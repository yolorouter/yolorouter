package protocols

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// maxStreamBufSize caps how much upstream data a stream relay buffer retains,
// preventing an unbounded (or misbehaving) upstream stream from growing the
// buffer without limit.
const maxStreamBufSize = 2 * 1024 * 1024

// errStreamTruncated is returned by IRStreamRelay / IRStreamRelayJSONLines
// when the upstream stream ends (clean EOF, no transport error) without ever
// emitting a DeltaDone (sig.SawDone stays false) — e.g. an empty 200
// response or an upstream that closes mid-completion. Without this check the
// relay would emit the client's success terminator (EncodeDone) and call
// onFinish for a response the caller never actually finished receiving,
// wrongly recording success and billing. Every decoder's Finish() emits a
// DeltaDone fallback for a genuinely complete stream (claude
// response.go:314, gemini:383, responses:159), so this only fires for a
// truly incomplete stream.
var errStreamTruncated = errors.New("upstream stream ended before completion")

// ErrClientWrite is the sentinel wrapped around every downstream (client-side)
// Write/Flush failure inside the IR streaming relay loops. It exists so the
// gateway can distinguish a genuine downstream write fault (slow client,
// disconnect) from an upstream read timeout (context.DeadlineExceeded also
// satisfies net.Error.Timeout(), so a broad net.Error check would
// misclassify upstream timeouts as client-side). Callers use errors.Is to
// detect it.
var ErrClientWrite = errors.New("downstream client write failure")

// maxIRResponseBytes caps a single non-stream cross-protocol upstream
// response body read by IRNonStreamRelay. A buggy or hostile provider can
// otherwise return an arbitrarily large body; without this cap
// io.ReadAll would grow the buffer until OOM before the request timeout
// fires (the response body has no bodylimit guard the way the request body
// does). Mirrors the gateway's same-protocol passthrough bound
// (maxNonStreamResponseBytes in internal/gateway/relay.go). A package var
// (not const) so tests can shrink it instead of buffering a real 32 MiB
// body.
var maxIRResponseBytes int64 = 32 * 1024 * 1024

// maxJSONLineBytes caps how large IRStreamRelayJSONLines's incomplete-line
// buffer (lineBuf) may grow while waiting for a newline. Without this bound,
// an upstream that sends bytes without ever emitting '\n' would grow lineBuf
// without limit. Comparable to the SSE scanner's per-line buffer cap
// (bufio.Scanner's 1 MiB max token size used elsewhere in this package). A
// package var (not const) so tests can shrink it instead of sending a real
// 1 MiB line.
var maxJSONLineBytes = 1 * 1024 * 1024

// allowedUpstreamHeaders is the allowlist of upstream response headers that the
// relay is permitted to forward to tenants. This uses an allowlist strategy:
// every unlisted header is dropped, preventing provider-internal details
// (organization IDs, server identifiers, diagnostic headers, rate-limit status,
// etc.) from leaking to tenants. Content-Type is deliberately excluded: each
// handler sets the correct Content-Type itself, so there's no need to copy it
// from upstream (this avoids duplicating or conflicting with the relay's own
// setting).
var allowedUpstreamHeaders = map[string]bool{
	"Cache-Control": true,
	"Retry-After":   true,
}

// hasMeaningfulUsage reports whether usage has been fully collected (any token
// field is non-zero). Combined with sawDone, this acts as a precondition for
// swallowing a ctx-canceled/EOF-race error as benign: under the OpenAI
// include_usage protocol, finish_reason can arrive before the standalone usage
// chunk does, and without this guard a request could be settled as successful
// in that window even though the usage chunk never arrived, undercharging the
// caller.
func hasMeaningfulUsage(u IRUsage) bool {
	// An impossible record proves nothing about whether real usage arrived, so
	// it must not buy the request the benign-post-DONE exemption below.
	if u.Invalid {
		return false
	}
	return u.PromptTokens > 0 || u.CompletionTokens > 0 || u.TotalTokens > 0
}

// IsBenignPostDoneReadErr reports whether a read error is a benign trailing
// error that only appears after the upstream has already sent its terminal
// DONE signal. Callers must first verify sawDone=true before using this
// function — these same error codes indicate a genuine upstream interruption
// when DONE was never seen.
//
// Covers four cases observed in production:
//  1. context.Canceled — the client closes the connection before EOF,
//     canceling the request context; the HTTP transport returns ctx.Err()
//     from Read before it would otherwise have returned io.EOF.
//  2. io.ErrUnexpectedEOF — HTTP/2 transports often translate the tail of a
//     chunked stream into an unexpected EOF (rather than io.EOF) when the
//     client cancels; errors.Is can see through the transport's fmt.Errorf
//     wrapping.
//  3. "http2: response body closed" — some upstream providers close the
//     HTTP/2 stream non-standardly after sending [DONE] (RST_STREAM, or
//     dropping the connection outright), which causes the Go HTTP/2
//     transport to return this error on the next Read; the error is an
//     unexported net/http internal var, so it can only be matched by string.
//  4. gateway.ErrIdleTimeout — some 2xx upstreams send [DONE] plus the final
//     usage frame and then hold the connection open (no EOF, no RST) instead
//     of closing it. The gateway wraps resp.Body in an idle-read timeout for
//     exactly this case; once that idle budget elapses on an otherwise
//     fully-delivered stream, the timeout is a benign trailing artifact of
//     the upstream's own keep-alive behavior, not a genuine interruption —
//     without this case, a successfully completed stream would still get an
//     extra inline error frame written to it and be misrecorded as
//     stream_partial, penalizing a provider that actually delivered
//     everything. gateway.ErrFirstByteTimeout is deliberately NOT included:
//     by the time DONE has been observed the first byte has necessarily
//     already arrived, so a first-byte timeout can never fire post-DONE.
//
// Exported so the relay layer's same-protocol passthrough path can reuse it,
// keeping the read-error allowlist consistent across all three streaming
// helpers. Matched by string (not errors.Is) because gateway.ErrIdleTimeout
// lives in internal/gateway, which imports this package — importing it back
// here would create a cycle.
func IsBenignPostDoneReadErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// "http2: response body closed" is an unexported net/http internal error,
	// so it cannot be matched with errors.Is.
	if strings.Contains(err.Error(), "http2: response body closed") {
		return true
	}
	// gateway.ErrIdleTimeout.Error() == "idle timeout between chunks" — see
	// case 4 above. Matched by string, not errors.Is/import, to avoid an
	// import cycle back into internal/gateway.
	return strings.Contains(err.Error(), "idle timeout between chunks")
}

// UpstreamBuffer is a minimal interface for recording upstream response data.
type UpstreamBuffer interface {
	// AppendUpstream records one raw (pre-IR-decode) upstream SSE/JSON-lines
	// line for a streaming request. NOT the caller-facing bytes — see
	// AppendResponse for that. Implementations are free to make this a
	// no-op; raw upstream lines are not part of the caller-facing audit
	// contract in this version (see AppendResponse's doc comment).
	AppendUpstream(data []byte)
	SetBody(data []byte)
	// SetResponseBody records the caller-facing (post-IR-encode) bytes
	// actually written to the client — the cross-protocol counterpart of
	// SetBody, which only captures the raw pre-decode upstream body.
	// Without this, the caller-facing audit body (request_log_bodies.
	// response_body) stays empty for every cross-protocol non-stream
	// success, unlike the same-protocol passthrough path.
	SetResponseBody(data []byte)
	// AppendResponse records one already-caller-facing (post-IR-encode) SSE
	// fragment actually written to the client for a streaming request — the
	// streaming counterpart of SetResponseBody. IRStreamRelay and
	// IRStreamRelayJSONLines call this at every point an encoded event is
	// written to c.Writer, so a per-request stream audit capture built from
	// these calls ends up byte-for-byte identical to what the client
	// received, matching the same-protocol passthrough path's capture
	// contract. Must NOT be conflated with AppendUpstream's raw upstream
	// lines — mixing the two into one capture would interleave pre-decode
	// and post-encode bytes.
	AppendResponse(data []byte)
}

// streamWriteWindow is the per-write sliding deadline for streaming relay
// loops. Each batch of Write/Flush calls sets a write deadline of now +
// streamWriteWindow, so a slow-reading client that stalls a Write is cut
// within this window rather than holding the handler (and its concurrency
// slot) open beyond the request timeout. A healthy stream that actively
// writes resets the deadline on every chunk, keeping the connection alive
// indefinitely (up to the request-level context timeout).
//
// cmd/yolorouter/serve.go's http.Server.WriteTimeout carries a slack on top
// of gateway.RequestTimeout, derived directly from StreamWriteWindow() (not a
// separate hard-coded literal) so it can never drift below this value: that
// slack is what covers a stream's pre-first-write gap, before anything has
// slid the deadline forward for the first time.
//
// A package var so tests can shrink it to keep the suite sub-second.
var streamWriteWindow = 60 * time.Second

// StreamWriteWindow returns the current streamWriteWindow value. Exported
// for tests that need to shrink it to keep the suite sub-second.
func StreamWriteWindow() time.Duration { return streamWriteWindow }

// SetStreamWriteWindow sets streamWriteWindow for the duration of a test.
// The caller is responsible for restoring the original value via t.Cleanup.
func SetStreamWriteWindow(d time.Duration) { streamWriteWindow = d }

// flusherWithError is the internal interface that net/http's *http.response
// (production) implements to return a flush error. gin's responseWriter
// implements http.Flusher but NOT this method, so it would shadow it from
// http.NewResponseController — hence the manual unwrap in FlushAndCheckError
// below.
type flusherWithError interface {
	FlushError() error
}

// responseWriterUnwrapper is satisfied by gin's responseWriter (and any
// middleware wrapper that follows the standard Unwrap convention).
type responseWriterUnwrapper interface {
	Unwrap() http.ResponseWriter
}

// FlushAndCheckError flushes the response writer and returns any deferred
// write error. It walks the Unwrap chain past gin's responseWriter (which
// implements http.Flusher and thus stops http.NewResponseController from
// discovering the inner FlushError method) to reach the innermost writer. If
// that writer implements FlushError() error, it is called and its error
// returned.
//
// When no FlushError is found in the chain, this falls back to the plain
// Flusher (Flush(), no error). In production that fallback is never
// exercised — gin's responseWriter always unwraps to *http.response, which
// implements FlushError — but a future middleware wrapper that forgets to
// implement Unwrap would silently degrade to this no-error fallback,
// disabling the flush-error-based slow/dead-client detection.
//
// Exported so both the IR streaming relay loops (this package) and the
// same-protocol passthrough stream pumps (internal/gateway) share one
// implementation instead of drifting apart — internal/gateway cannot import
// this package's caller (it already depends on protocols), so the canonical
// implementation lives here.
func FlushAndCheckError(c *gin.Context) error {
	w := c.Writer
	var inner http.ResponseWriter = w
	for {
		if fe, ok := inner.(flusherWithError); ok {
			return fe.FlushError()
		}
		if u, ok := inner.(responseWriterUnwrapper); ok {
			inner = u.Unwrap()
			continue
		}
		break
	}
	// No FlushError in the chain — fall back to the standard Flusher.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// WatchClientClose starts a watcher that immediately closes the upstream
// connection when the client disconnects or c.Request.Context() is canceled,
// letting a blocked scanner.Scan / Body.Read return right away. This avoids
// reading the entire upstream response to completion after the client has
// already left (wasting tokens and billing the user for content the client
// never receives).
//
// Key detail: this must call c.Writer.CloseNotify() — a Go HTTP/1.x server
// does not actively detect a half-closed client connection while a handler
// is running by default; calling CloseNotify() has the side effect of
// activating net/http's backgroundRead goroutine, which is what actually
// propagates a client FIN/RST to c.Request.Context().
//
// Returns stop(); callers should defer stop() so the watcher exits cleanly
// on normal completion without closing the upstream connection.
func WatchClientClose(c *gin.Context, upstream io.Closer) (stop func()) {
	done := make(chan struct{})
	var closeNotify <-chan bool
	// CloseNotify is deprecated, but **calling it** is itself the necessary
	// side effect: it activates net/http's backgroundRead goroutine, which is
	// what propagates a client FIN to c.Request.Context(). Watching
	// ctx.Done() alone without calling CloseNotify does not work under
	// HTTP/1.x.
	//
	// Gin's ResponseWriter interface explicitly declares that it implements
	// http.CloseNotifier, so the type assertion always succeeds; but it
	// delegates to the underlying http.ResponseWriter (in production,
	// *http.response; in unit tests, often *httptest.ResponseRecorder). The
	// latter doesn't necessarily implement CloseNotifier, in which case
	// calling CloseNotify() panics. The recover here guards against that in
	// unit tests without affecting the production path.
	func() {
		defer func() { _ = recover() }()
		//nolint:staticcheck // SA1019: CloseNotify is deprecated, but calling it is
		// the necessary side effect described above; there is no ctx.Done()-only
		// replacement under HTTP/1.x.
		if cn, ok := c.Writer.(http.CloseNotifier); ok {
			closeNotify = cn.CloseNotify()
		}
	}()
	go func() {
		select {
		case <-c.Request.Context().Done():
		case <-closeNotify:
		case <-done:
			return
		}
		_ = upstream.Close()
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// newSSEEmitter builds the emit function both streaming relays share: defer
// the SSE headers and the 200 until there is a first real event to send, and
// capture caller-facing bytes only after BOTH Write and Flush succeed.
//
// One constructor rather than two inline closures, because the two relays'
// copies had already drifted into being kept in step by comments alone. The
// rules it encodes are load-bearing:
//   - Nothing may touch the writer before the first non-empty batch: an early
//     Flush or Write would implicitly commit a bare 200 with none of the SSE
//     headers set, and the window for handing this candidate's failure to the
//     next one would close for nothing.
//   - A refused commit aborts: the response was already committed by somebody
//     else, and carrying on would stream a body under a status this call did
//     not choose.
//   - Capture happens only after Flush reports success — net/http may buffer
//     a small Write and return nil without pushing bytes onto the socket, so
//     capturing at Write time could record bytes the client never received.
//     This is the only capture point that keeps the stream audit file
//     byte-for-byte identical to what the client got.
func newSSEEmitter(w ClientWriter, buf UpstreamBuffer, onFirstChunk func()) func([]SSEEvent) error {
	headerWritten := false
	return func(events []SSEEvent) error {
		if len(events) == 0 {
			return nil
		}
		if !headerWritten {
			w.Inject(http.Header{
				"Content-Type":      {"text/event-stream"},
				"Cache-Control":     {"no-cache"},
				"Connection":        {"keep-alive"},
				"X-Accel-Buffering": {"no"},
			})
			if cerr := w.Commit(http.StatusOK); cerr != nil {
				return fmt.Errorf("%w: commit stream to client: %w", ErrClientWrite, cerr)
			}
			headerWritten = true
			if onFirstChunk != nil {
				onFirstChunk()
				onFirstChunk = nil
			}
		}
		var written [][]byte
		for _, event := range events {
			b := []byte(event.String())
			if _, err := w.Write(b); err != nil {
				return fmt.Errorf("%w: write to client: %w", ErrClientWrite, err)
			}
			if buf != nil {
				written = append(written, b)
			}
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("%w: flush to client: %w", ErrClientWrite, err)
		}
		for _, b := range written {
			buf.AppendResponse(b)
		}
		return nil
	}
}

// IRStreamRelay proxies an upstream SSE stream through IR: decode -> encode.
// Returns partial usage even on upstream read errors.
//
// The client-facing SSE response headers (200 + text/event-stream) are
// DEFERRED until the first encoded event is actually about to be written —
// mirroring the same-protocol passthrough pump's deferred-header behavior. If the upstream ends (clean EOF) or errors
// before any event is ever emitted, this function returns an error WITHOUT
// having written anything to the client, so the caller can still fail over
// to a healthy candidate instead of being stuck with an already-committed
// empty 200.
//
// onFirstChunk fires once, at that same first-event point (pass nil to
// skip); used for TTFT (time-to-first-token) measurement and — via the
// caller — for marking the response as committed (no more failover).
// onFinish is called when the stream ends normally, with the raw finish
// reason, whether a tool call was seen, and whether any content was produced
// (pass nil to skip); used for finish_reason collection.
func IRStreamRelay(
	w ClientWriter,
	resp *http.Response,
	decoder StreamDecoder,
	encoder StreamEncoder,
	buf UpstreamBuffer,
	onFirstChunk func(),
	onFinish func(rawReason string, sawToolCall, produced bool),
) (*IRUsage, error) {
	defer func() { _ = resp.Body.Close() }()
	// Watching the caller's connection is the caller's own job: it owns that
	// connection, and doing it here would need the framework's request object
	// back. See WatchClientClose.
	//
	// A slow-reading client is bounded by the writer, which slides a deadline
	// forward on every write and flush, rather than by this loop doing it per
	// batch — so every path through this interface gets the same protection.

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var sig FinishSignals
	// The emitter owns the deferred-header rule; see newSSEEmitter. It must be
	// the only thing that touches the writer before the first event goes out.
	emit := newSSEEmitter(w, buf, onFirstChunk)

	for scanner.Scan() {
		line := scanner.Text()

		if buf != nil {
			buf.AppendUpstream(append([]byte(line), '\n'))
		}

		deltas, err := decoder.DecodeChunk(line + "\n")
		if err != nil {
			continue
		}

		sig.Accumulate(deltas)
		if werr := emit(encoder.EncodeDeltas(deltas)); werr != nil {
			u := encoder.Usage()
			return &u, fmt.Errorf("stream write failed: %w", werr)
		}
	}

	// The decoder buffer must be flushed before checking scanner.Err: if the
	// upstream's last line ([DONE] / finish_reason) lacks a trailing blank
	// line (the OpenAI chat decoder only emits deltas from its buffer on
	// \n\n), sig.SawDone is still false at this point in the main loop. If
	// the scanner then errors with a ctx-canceled error and we return
	// immediately, a terminal frame that fully arrived would be
	// misclassified as a failed stream. Finish flushes the remaining buffer;
	// on the normal path, receiving DeltaDone sets sig.SawDone=true, which
	// lets a subsequent read error be recognized as "client closed after
	// receiving everything".
	deltas, finishErr := decoder.Finish()
	sig.Accumulate(deltas)
	if len(deltas) > 0 {
		if werr := emit(encoder.EncodeDeltas(deltas)); werr != nil {
			u := encoder.Usage()
			return &u, fmt.Errorf("stream write failed: %w", werr)
		}
	}

	var scanErr error
	if err := scanner.Err(); err != nil {
		// Tightened exemption: settle as success only when sawDone
		// (DeltaDone was emitted) AND usage has been fully collected AND the
		// error belongs to the benign-trailing family. Under the OpenAI
		// include_usage protocol, finish_reason and the final usage chunk
		// arrive as **two separate SSE frames** — if the client disconnects
		// after finish_reason but before usage, relying on sawDone alone
		// would let a request with encoder.Usage() still zero be swallowed
		// and settled as success, undercharging the user and leaving
		// provider_cost missing. Only once usage is fully collected can we
		// be sure the billing fields have actually all arrived.
		u := encoder.Usage()
		//nolint:staticcheck // QF1001: kept as a positive "all three exemption
		// conditions hold" grouping to match the doc comment above; a De
		// Morgan'd form would obscure the exemption logic being described.
		if !(sig.SawDone && hasMeaningfulUsage(u) && IsBenignPostDoneReadErr(err)) {
			scanErr = fmt.Errorf("upstream stream read error: %w", err)
			return &u, scanErr
		}
	}

	// finishErr != nil means the upstream stream reported an explicit
	// failure inline (e.g. a response.failed / error event). In that case we
	// must **not** call EncodeDone() — that would write the client
	// protocol's "successful termination frame" (Claude message_stop / Chat
	// finish_reason=stop / Gemini STOP), and the client would treat a failed
	// request as a complete response. Leaving out the termination frame lets
	// the client SDK treat the stream as truncated, consistent with the
	// server settling this as a 502.
	//
	// sig.SawDone is required too: a clean EOF before any DeltaDone (an
	// empty 200 response, or an upstream that closes mid-completion without
	// an inline error) must not be synthesized into a success terminator
	// either — see errStreamTruncated's doc comment.
	terminalErr := finishErr
	if finishErr == nil && sig.SawDone {
		if werr := emit(encoder.EncodeDone()); werr != nil {
			u := encoder.Usage()
			return &u, fmt.Errorf("stream write failed: %w", werr)
		}
		// Only notify the collection point when the stream ended normally
		// (no finishErr); a failed stream leaves finish_reason unset.
		if onFinish != nil {
			onFinish(sig.Raw, sig.SawToolCall, sig.Produced)
		}
	} else if finishErr == nil {
		terminalErr = errStreamTruncated
	}

	usage := encoder.Usage()
	return &usage, terminalErr
}

// IRNonStreamRelay proxies a non-streaming response through IR: decode -> encode.
//
// Behavior on errors:
//   - Upstream non-2xx: the raw body is passed through to the client
//     (standard error passthrough, no IR round trip), returning a nil error.
//   - Upstream 2xx but IR decode fails (including a status=failed / error
//     field): **nothing is written to the client**; decErr is returned so
//     the caller can decide to write a client-protocol-native error body,
//     rewrite the status code, and skip billing.
//   - Reading the body fails: an IO error is returned.
//
// This lets the caller correctly write a 502 with a client-protocol-formatted
// error body on a decode error, instead of the client ending up with both an
// upstream 200 OK and an unrecognized Responses JSON failure body (which
// client SDKs would treat as a 200).
// onFinish is called after a successful decode and response send, with the
// raw finish reason, whether a tool call occurred, and whether any content
// was produced (pass nil to skip).
func IRNonStreamRelay(
	w ClientWriter,
	resp *http.Response,
	decoder ResponseDecoder,
	encoder ResponseEncoder,
	buf UpstreamBuffer,
	onFinish func(rawReason string, sawToolCall, produced bool),
) (*IRUsage, error) {
	defer func() { _ = resp.Body.Close() }()

	// Read up to N+1 bytes so an overflow is detectable, then fail instead
	// of buffering an unbounded upstream body (see maxIRResponseBytes).
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIRResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream body: %w", err)
	}
	if int64(len(body)) > maxIRResponseBytes {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", maxIRResponseBytes)
	}

	if buf != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		buf.SetBody(body)
	}

	// Non-2xx: the upstream error body is passed through (bypassing IR); the
	// caller routes billing decisions by status code alone.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Preserve upstream Content-Type for error bodies so clients can parse them.
		// This is an explicit single-header copy, not subject to the whitelist policy
		// which targets success responses where the relay sets its own Content-Type.
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Inject(http.Header{"Content-Type": {ct}})
		}
		w.Inject(UpstreamHeadersToCopy(resp.Header))
		if cerr := w.Commit(resp.StatusCode); cerr != nil {
			return nil, fmt.Errorf("%w: commit error body to client: %w", ErrClientWrite, cerr)
		}
		if _, werr := w.Write(body); werr != nil {
			return nil, fmt.Errorf("%w: write error body to client: %w", ErrClientWrite, werr)
		}
		// A small error body can land entirely inside net/http's internal
		// buffer: Write returns nil without the bytes actually reaching the
		// socket, and the real delivery error only surfaces on Flush.
		// Without this check a client that disconnects right after a
		// buffered Write is still recorded as having received the error body.
		if ferr := w.Flush(); ferr != nil {
			return nil, fmt.Errorf("%w: flush error body to client: %w", ErrClientWrite, ferr)
		}
		return nil, nil
	}

	// 2xx: must be validated through the IR decoder.
	irResp, decErr := decoder.DecodeResponse(json.RawMessage(body))
	if decErr != nil {
		// **Write nothing to the client** here: let the caller take the
		// "client-protocol-native 502 error" path. The raw body has already
		// been saved to buf (via buf.SetBody above), so operators can still
		// debug it. Even on failure, the decoder may return a partial
		// IRResponse (preserving wire.Usage), so provider cost / dispatch
		// analysis doesn't lose the information of how many tokens the
		// upstream consumed before failing.
		var partialUsage *IRUsage
		if irResp != nil {
			partialUsage = &irResp.Usage
		}
		return partialUsage, decErr
	}

	encoded := encoder.EncodeResponse(irResp)
	if buf != nil {
		// Caller-facing (post-IR-encode) bytes, mirroring SetBody's capture
		// of the raw pre-decode upstream body above.
		buf.SetResponseBody(encoded)
	}
	// The relay encodes the success body as JSON; set Content-Type explicitly because
	// Content-Type is excluded from the upstream header allowlist.
	// The body was already fully decoded against the upstream response
	// (irResp.Usage reflects real upstream consumption), so partial usage is
	// still returned for correct billing below even if the commit/write/flush
	// that follows fails — only the delivery outcome is a failure.
	var partialUsage *IRUsage
	if irResp != nil {
		partialUsage = &irResp.Usage
	}
	w.Inject(http.Header{"Content-Type": {"application/json"}})
	w.Inject(UpstreamHeadersToCopy(resp.Header))
	if cerr := w.Commit(resp.StatusCode); cerr != nil {
		return partialUsage, fmt.Errorf("%w: commit response to client: %w", ErrClientWrite, cerr)
	}
	if _, werr := w.Write(encoded); werr != nil {
		// The error is wrapped in ErrClientWrite so the caller (which also
		// reaches this same return slot via decErr above) can tell a
		// downstream write failure apart from an IR decode failure and
		// classify it as a client write timeout instead of a 2xx success.
		return partialUsage, fmt.Errorf("%w: write response to client: %w", ErrClientWrite, werr)
	}
	// A non-streaming JSON body is often small enough to land entirely
	// inside net/http's internal buffer: Write returns nil without the
	// bytes actually reaching the socket, and the real delivery error only
	// surfaces on Flush. Without this check, a client that disconnects
	// right after a buffered Write is still recorded as a delivered 2xx —
	// the classification this whole function exists to get right.
	if ferr := w.Flush(); ferr != nil {
		return partialUsage, fmt.Errorf("%w: flush response to client: %w", ErrClientWrite, ferr)
	}

	if irResp != nil {
		// Non-streaming success path: extract the finish_reason signal from
		// irResp. produced also accounts for ReasoningContent: a reasoning
		// model may only produce thinking with Content empty, and without
		// this check a normally completed thinking-only response would be
		// misclassified as empty.
		if onFinish != nil {
			sawToolCall := len(irResp.ToolCalls) > 0
			produced := irResp.Content != "" || irResp.ReasoningContent != "" || len(irResp.ToolCalls) > 0
			onFinish(irResp.StopReason, sawToolCall, produced)
		}
		return &irResp.Usage, nil
	}
	return nil, nil
}

// IRStreamRelayJSONLines proxies a Gemini-style JSON Lines stream through IR.
// Consistent with IRStreamRelay: an explicit in-stream upstream failure
// (decoder.Finish returns an error) or a non-EOF read error is propagated as
// an error, letting the caller rewrite the status code / skip
// RecordSuccess.
//
// Like IRStreamRelay, the client-facing SSE response headers are DEFERRED
// until the first encoded event is actually about to be written — see
// IRStreamRelay's doc comment for the full rationale (pre-first-event
// failover).
//
// onFirstChunk fires once, at that same first-event point (pass nil to
// skip); used for TTFT measurement.
// onFinish is called when the stream ends normally, with the raw finish
// reason, whether a tool call was seen, and whether any content was produced
// (pass nil to skip); used for finish_reason collection.
func IRStreamRelayJSONLines(
	w ClientWriter,
	resp *http.Response,
	decoder StreamDecoder,
	encoder StreamEncoder,
	buf UpstreamBuffer,
	onFirstChunk func(),
	onFinish func(rawReason string, sawToolCall, produced bool),
) (*IRUsage, error) {
	defer func() { _ = resp.Body.Close() }()
	// Watching the caller's connection is the caller's own job: it owns that
	// connection, and doing it here would need the framework's request object
	// back. See WatchClientClose.
	//
	// A slow-reading client is bounded by the writer, which slides a deadline
	// forward on every write and flush, rather than by this loop doing it per
	// batch — so every path through this interface gets the same protection.

	buf2 := make([]byte, 4096)
	var lineBuf []byte
	var rawReadErr error
	var sig FinishSignals

	// The emitter owns the deferred-header rule; see newSSEEmitter. It must be
	// the only thing that touches the writer before the first event goes out.
	emit := newSSEEmitter(w, buf, onFirstChunk)

	for {
		n, err := resp.Body.Read(buf2)
		if n > 0 {
			lineBuf = append(lineBuf, buf2[:n]...)
			for {
				idx := bytes.IndexByte(lineBuf, '\n')
				if idx < 0 {
					break
				}
				line := string(lineBuf[:idx])
				lineBuf = lineBuf[idx+1:]

				if buf != nil {
					buf.AppendUpstream(append([]byte(line), '\n'))
				}

				deltas, decErr := decoder.DecodeChunk(line + "\n")
				if decErr != nil {
					continue
				}

				sig.Accumulate(deltas)
				if werr := emit(encoder.EncodeDeltas(deltas)); werr != nil {
					// Return immediately — do NOT fall through to the
					// leftover-lineBuf / decoder.Finish blocks below.
					// Those blocks assume a still-writable client: on a
					// dead connection they would emit() again onto a
					// broken writer and feed the still-unconsumed
					// lineBuf tail (which may itself contain an embedded
					// '\n', i.e. more than one line) to DecodeChunk as if
					// it were a single line, corrupting decoder state for
					// no benefit since nothing more can reach the client
					// anyway. Mirrors IRStreamRelay's identical emit-failure
					// handling in its own scan loop.
					usage := encoder.Usage()
					return &usage, fmt.Errorf("stream write failed: %w", werr)
				}
			}
			// The loop above drains every complete line out of lineBuf;
			// whatever remains is an incomplete tail still waiting for a
			// newline. Cap it so an upstream that sends bytes without ever
			// emitting '\n' can't grow lineBuf without bound.
			if len(lineBuf) > maxJSONLineBytes {
				rawReadErr = fmt.Errorf("upstream JSON-lines line exceeds %d bytes", maxJSONLineBytes)
				break
			}
		}
		if err != nil {
			rawReadErr = err
			break
		}
	}

	// Leftover lineBuf: the upstream's last line may lack a trailing newline
	// (observed with some Gemini streams that EOF directly); it must be
	// decoded first so sig.SawDone can pick up finishReason before deciding
	// whether the read error triggers a failure settlement.
	if len(lineBuf) > 0 {
		if buf != nil {
			buf.AppendUpstream(lineBuf)
		}
		deltas, _ := decoder.DecodeChunk(string(lineBuf) + "\n")
		sig.Accumulate(deltas)
		if werr := emit(encoder.EncodeDeltas(deltas)); werr != nil {
			usage := encoder.Usage()
			return &usage, fmt.Errorf("stream write failed: %w", werr)
		}
	}

	// Finish is called before judging the read error: the decoder may still
	// have an internal buffer holding a termination signal.
	deltas, finishErr := decoder.Finish()
	sig.Accumulate(deltas)
	if len(deltas) > 0 {
		if werr := emit(encoder.EncodeDeltas(deltas)); werr != nil {
			usage := encoder.Usage()
			return &usage, fmt.Errorf("stream write failed: %w", werr)
		}
	}

	// EOF is a normal end. When the client closes the connection only after
	// the upstream SSE has finished, resp.Body.Read gets
	// ctx.Err()=context.Canceled instead of io.EOF; as long as sawDone=true,
	// the upstream has already finished emitting finish_reason normally, so
	// this should settle as success. Any other non-EOF error is a genuine
	// upstream interruption. The loop only breaks when Read returns an
	// error (including io.EOF), so rawReadErr must be non-nil here. The
	// exemption condition mirrors IRStreamRelay: sawDone + usage fully
	// collected + error in the benign-trailing family.
	var readErr error
	//nolint:staticcheck // QF1001: kept as a positive "all three exemption
	// conditions hold" grouping to match the doc comment above; a De Morgan'd
	// form would obscure the exemption logic being described.
	if !errors.Is(rawReadErr, io.EOF) && !(sig.SawDone && hasMeaningfulUsage(encoder.Usage()) && IsBenignPostDoneReadErr(rawReadErr)) {
		readErr = fmt.Errorf("upstream JSON-lines read error: %w", rawReadErr)
	}

	// Mirrors IRStreamRelay: finishErr or readErr means the stream failed,
	// so EncodeDone must not be called — otherwise the client would see a
	// seemingly-complete termination frame, inconsistent with the server
	// settling this as a 502. sig.SawDone is also required: a clean EOF
	// before any DeltaDone must not be synthesized into a success
	// terminator either — see errStreamTruncated's doc comment.
	var terminalErr error
	switch {
	case readErr == nil && finishErr == nil && sig.SawDone:
		if werr := emit(encoder.EncodeDone()); werr != nil {
			usage := encoder.Usage()
			return &usage, fmt.Errorf("stream write failed: %w", werr)
		}
		// Only notify the collection point when the stream ended normally
		// (no finishErr / readErr); a failed stream leaves finish_reason
		// unset.
		if onFinish != nil {
			onFinish(sig.Raw, sig.SawToolCall, sig.Produced)
		}
	case readErr != nil:
		terminalErr = readErr
	case finishErr != nil:
		terminalErr = finishErr
	default:
		// readErr == nil && finishErr == nil && !sig.SawDone
		terminalErr = errStreamTruncated
	}

	usage := encoder.Usage()
	return &usage, terminalErr
}

// ClientWriter is everything the relay helpers need in order to answer the
// caller. It is declared here, by the package that needs it, rather than taken
// as a *gin.Context: a relay that holds the framework's request object can do
// anything to it, and what these functions actually do is stage headers, commit
// a status, write bytes and flush them.
//
// Declaring the need rather than importing the provider is also what lets the
// gateway hand in its own response object — the one that knows when bytes have
// really left — without this package having to know that type exists.
type ClientWriter interface {
	// Inject stages response headers, REPLACING any already staged under the
	// same name and keeping every value given for it. Replacing is what the
	// callers mean: a relay setting Content-Type is stating what the body is,
	// not adding a second opinion, and two Content-Type values on one response
	// is a malformed response rather than a richer one.
	//
	// Headers may or may not reach the caller immediately; what is guaranteed
	// is that they are in place by the time Commit returns.
	Inject(h http.Header)
	// Commit fixes the status. Whether it is on the wire at that moment is the
	// implementation's business — a buffering writer may hold it until the
	// first body write — but nothing may be written before it, and no later
	// call may change it.
	Commit(status int) error
	Write(p []byte) (int, error)
	// Flush pushes buffered bytes onto the socket and reports the failure a
	// buffered Write can hide.
	Flush() error
}

// UpstreamHeadersToCopy is the subset of an upstream's response headers that
// may be passed on to the caller.
//
// A provider's headers can name the provider, the account, or the model behind
// an alias, so the allowlist is the whole point. It is a plain filter, separate
// from the writing, so it can be checked on its own — before this split the
// only way to test the policy was to run a whole relay through it.
func UpstreamHeadersToCopy(header http.Header) http.Header {
	var out http.Header
	for k, vv := range header {
		if !allowedUpstreamHeaders[k] {
			continue
		}
		if out == nil {
			out = http.Header{}
		}
		for _, v := range vv {
			out.Add(k, v)
		}
	}
	return out
}
