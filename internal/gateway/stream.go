package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter-ce/pkg/logger"
	"github.com/yolorouter/yolorouter-ce/pkg/redact"
)

// errClientDisconnected is returned by the stream pump when the caller's
// request context is cancelled (GATE-20). It is not a real upstream failure
// — the relay loop records it as a distinct outcome so the request log shows
// "caller cancelled", not "upstream failed".
var errClientDisconnected = errors.New("client disconnected")

// errStreamNoDoneTerminator is returned when the upstream sent at least one
// data frame but closed the stream without the `data: [DONE]` terminator.
// The completion is silently truncated — the caller already received bytes
// (so the HTTP status stays 200), but handleStream logs a partial row,
// not clean success (stream-integrity fix).
var errStreamNoDoneTerminator = errors.New("upstream stream ended without [DONE] terminator")

// maxPreambleBytes caps the pre-first-data-frame preamble buffer in
// StreamUpstreamToClient — a malicious/buggy upstream could otherwise grow
// it without bound (the response body has no bodylimit guard the way the
// request body does).
const maxPreambleBytes = 64 * 1024

// maxStreamLineBytes caps a single SSE line. bufio.Reader.ReadBytes doesn't
// bound line length itself — without this a malicious/buggy upstream sending
// a very long line without a newline could grow the in-memory buffer without
// limit (the response body has no bodylimit guard the way the request does).
const maxStreamLineBytes = 1 * 1024 * 1024 // 1 MiB

// maxStreamBodyFileBytes is a HARD anti-OOM disk backstop, NOT a content
// truncation (PRD §6.8.6/LOG-08: the stream capture must record every SENT
// SSE fragment, never cut short for length). A normal chat stream never
// approaches this; only a hostile/buggy upstream maxing bandwidth within the
// 120s upstream timeout could. Hitting it sets stream_body_truncated=true
// (marked, never silent) and stops appending — the caller's own stream is
// unaffected either way. A package var (not const) so tests can inject a
// small value instead of actually writing 1GiB.
var maxStreamBodyFileBytes int64 = 1 << 30 // 1 GiB

// StreamUpstreamToClient pipes an SSE stream from upstream to the client,
// rewriting the model field in every `data: {json}` chunk back to the
// external name (PRD §6.5.5). Returns the usage from the final usage chunk
// if the upstream sent one, and an error only for transport-level failures.
//
// Header is deferred until the first data frame: if the upstream returns 2xx
// but EOFs (or errors) before emitting any data, nothing has been written to
// the client yet and the relay loop can still failover to the next candidate
// (PRD §6.5.5 lifecycle table — "received upstream response, first chunk not
// yet sent to caller → failover allowed"). Once a data frame is forwarded,
// rc.FirstByteSent flips true and no more switching is allowed (GATE-19).
//
// Leading non-data lines before the first data frame (commentary / SSE
// preamble) are skipped — OpenAI chat streams open with `data:` directly, so
// this only matters for unusual upstreams, and skipping keeps the failover
// window intact.
//
// The [DONE] terminator is tracked: an upstream that sends data frames and
// then closes cleanly WITHOUT `data: [DONE]` has truncated the completion —
// the pump returns errStreamNoDoneTerminator so handleStream records a
// partial row instead of clean success.
//
// When the caller did NOT request stream_options.include_usage but the
// gateway injected it upstream (EnsureStreamUsageInjection), the usage field
// is stripped from forwarded frames (PRD §1114: injected usage is for the
// gateway's internal cost accounting only).
func StreamUpstreamToClient(c *gin.Context, resp *http.Response, rc *RelayContext) (*Usage, error) {
	defer func() { _ = resp.Body.Close() }()

	// M6.2 (Codex #4): append every SENT SSE line (post-rewrite, post-usage-
	// strip = caller-facing, satisfying PRD §6.8.4 "sent fragments") to a
	// per-request file under data/bodies/. Memory stays bounded (one line at
	// a time); the full stream is persisted without truncation. The 1GiB
	// backstop only sets a truncation flag (never silent) for a hostile
	// upstream.
	//
	// The file is opened here but deliberately NOT closed before this
	// function returns (code-review finding): a mid-stream failure after the
	// first byte causes handleStream to call writeStreamErrorEvent, which
	// writes one more inline SSE error frame + a synthetic [DONE] straight to
	// the client — those bytes must also land in the capture file, so the
	// file has to stay open across that call. handleStream closes it via
	// closeStreamBodyFile once it has finished writing everything it's going
	// to write for this attempt.
	openStreamBodyFile(c, rc)

	flusher, _ := c.Writer.(http.Flusher)
	headerWritten := false
	doneSeen := false // tracks whether upstream emitted the `data: [DONE]` terminator
	var usage *Usage
	// preamble buffers any SSE lines that arrive before the first data
	// frame (commentary heartbeats, event:/id:/retry: directives, blank
	// separators). They're flushed in order once the first data frame
	// commits the headers — kept intact rather than dropped, so an upstream
	// that opens with a preamble doesn't lose framing, while the failover
	// window still only closes once actual data has reached the client.
	var preamble []byte
	// bufio.Scanner (not Reader.ReadBytes) bounds a single line's memory at
	// maxStreamLineBytes — ReadBytes allocates the whole line before any
	// length check could fire, so the cap was decorative. Scanner.Buffer's
	// second arg is the max token size; exceeding it surfaces as
	// bufio.ErrTooLong from Err().
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLineBytes)
	for scanner.Scan() {
		// GATE-20: caller disconnect -> stop reading upstream and release
		// the concurrency slot. Checked before each forwarded line so a
		// disconnect between chunks is caught promptly.
		select {
		case <-c.Request.Context().Done():
			return usage, errClientDisconnected
		default:
		}
		// ScanLines strips the trailing newline; re-add it so writeStreamLine
		// sees the same shape ReadBytes would have produced.
		line := append(scanner.Bytes(), '\n')
		switch {
		case headerWritten:
			// Flush to the client BEFORE the capture-file append (code-review
			// efficiency finding): the append does a redact pass (regexes +
			// a conditional JSON decode/marshal) plus a disk write, neither
			// of which the caller should wait on before seeing bytes it
			// already received — capture is best-effort persistence, not
			// part of the client-visible streaming path.
			sent := forwardStreamLine(c, rc, line, &usage, &doneSeen)
			if flusher != nil {
				flusher.Flush()
			}
			appendStreamBodyLine(rc, sent)
		case isDataLine(line):
			// First data frame — commit the SSE headers, flush any buffered
			// preamble in order, then forward the data line.
			writeSSEHeader(c)
			hadPreamble := len(preamble) > 0
			if hadPreamble {
				_, _ = c.Writer.Write(preamble)
			}
			headerWritten = true
			sent := forwardStreamLine(c, rc, line, &usage, &doneSeen)
			if flusher != nil {
				flusher.Flush()
			}
			if hadPreamble {
				// The preamble bytes were just sent to the caller — capture
				// them too, or the persisted stream body silently starts
				// after whatever heartbeat/event:/id:/retry: lines the
				// upstream opened with (code-review finding).
				appendStreamBodyLine(rc, preamble)
				preamble = nil
			}
			appendStreamBodyLine(rc, sent)
		default:
			// Preamble line before the first data frame — buffer it, but
			// cap the buffer so a malicious/buggy upstream can't grow it
			// without bound (the response body has no bodylimit guard the
			// way the request body does).
			if len(preamble)+len(line) <= maxPreambleBytes {
				preamble = append(preamble, line...)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// A caller disconnect surfaces as a body-read error (the transport
		// cancels on ctx.Done); recognize it so handleStream logs 499
		// instead of an upstream stream fault.
		if errors.Is(c.Request.Context().Err(), context.Canceled) {
			return usage, errClientDisconnected
		}
		if errors.Is(err, bufio.ErrTooLong) {
			return usage, fmt.Errorf("upstream stream line too long (max %d bytes)", maxStreamLineBytes)
		}
		return usage, fmt.Errorf("read upstream stream: %w", err)
	}
	// scanner.Scan() returned false with nil err = clean EOF.
	if !headerWritten {
		return usage, errors.New("upstream stream ended before any data chunk")
	}
	if !doneSeen {
		// Upstream emitted at least one data frame but closed without the
		// [DONE] terminator — the completion is silently truncated. Report
		// it so handleStream logs a partial row instead of clean success.
		// Bytes already went to the client, so the HTTP status stays 200.
		return usage, errStreamNoDoneTerminator
	}
	return usage, nil
}

// forwardStreamLine writes one SSE line and folds its outcome back onto rc
// (first-byte marker), the running usage pointer (final-frame tokens), and
// the [DONE] terminator flag. It returns the exact bytes written to the
// client (post model-rewrite, post usage-strip) so the caller can append
// the same caller-facing bytes to the stream capture file (Task 5,
// PRD §6.8.4 "sent fragments") — never the raw pre-rewrite upstream line.
func forwardStreamLine(c *gin.Context, rc *RelayContext, line []byte, usage **Usage, doneSeen *bool) []byte {
	wroteData, u, done, sent := writeStreamLine(c.Writer, line, rc.OriginalModel, rc.WantsStreamUsage)
	if wroteData {
		rc.MarkFirstByteSent()
	}
	if u != nil {
		*usage = u
	}
	if done {
		*doneSeen = true
	}
	return sent
}

func writeSSEHeader(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Disable proxy buffering (nginx X-Accel-Buffering et al) so SSE chunks
	// reach the client token-by-token instead of in buffered batches.
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
}

func isDataLine(line []byte) bool {
	// SSE allows "data:" with or without a space after the colon — accept
	// both so a provider that omits the space isn't misclassified as a
	// preamble line.
	return bytes.HasPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:"))
}

// writeStreamLine writes one SSE line to the client, rewriting the model
// field when the line is a `data: {json}` chunk. Returns wroteData=true if
// the line was a data line (counts toward the first-byte decision), the
// usage extracted from this chunk (the final usage chunk carries
// prompt/completion tokens), done=true if the line was the [DONE]
// terminator, and sent = the exact bytes written to w (caller-facing,
// post-rewrite/post-usage-strip) — the stream body capture (Task 5) appends
// this, never the raw pre-rewrite input line.
func writeStreamLine(w io.Writer, line []byte, externalModel string, keepUsage bool) (wroteData bool, usage *Usage, done bool, sent []byte) {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		// Non-data line (blank separator, event:/id:/retry: headers) —
		// forward verbatim so the SSE framing stays intact.
		_, _ = w.Write(line)
		return false, nil, false, line
	}
	// SSE allows "data:" or "data: " — the optional single space after the
	// colon is framing, not part of the value.
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 {
		_, _ = w.Write(line)
		return true, nil, false, line
	}
	if string(payload) == "[DONE]" {
		out := []byte("data: [DONE]\n")
		_, _ = w.Write(out)
		return true, nil, true, out
	}
	rewritten, u := rewriteStreamChunk(payload, externalModel, keepUsage)
	out := make([]byte, 0, len(rewritten)+len("data: ")+1)
	out = append(out, "data: "...)
	out = append(out, rewritten...)
	out = append(out, '\n')
	_, _ = w.Write(out)
	return true, u, false, out
}

// rewriteStreamChunk rewrites the model field in one SSE data payload. If
// the payload isn't valid JSON it's forwarded unchanged — breaking the
// stream over one malformed chunk would punish the caller for an upstream
// quirk. usage is pulled out of the SAME already-decoded map (not via a
// second json.Unmarshal of the whole payload), so the streaming hot path
// parses each frame once for the rewrite plus one tiny sub-decode for usage.
//
// When keepUsage is false (caller did not request stream_options.include_usage
// but the gateway injected it upstream), the usage field is stripped from the
// forwarded payload — the gateway still returns the extracted usage for its
// own cost accounting, but does not forward it to the caller (PRD §1114).
func rewriteStreamChunk(payload []byte, externalModel string, keepUsage bool) ([]byte, *Usage) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return payload, nil
	}
	if m == nil {
		// payload was literal "null" — json.Unmarshal returns nil error but
		// leaves m nil, and writing m["model"] would panic on a nil map.
		// Forward unchanged (mirrors rewriteModelField's guard).
		return payload, nil
	}
	// Only rewrite model when the chunk actually carries one — don't inject
	// it into usage-only / ping frames that never had a model field.
	if _, ok := m["model"]; ok {
		if modelJSON, err := json.Marshal(externalModel); err == nil {
			m["model"] = modelJSON
		}
	}
	usage := usageFromRawMap(m)
	// Strip the usage field from forwarded frames unless the caller asked
	// for it (PRD §1114: usage the gateway injected for its own cost
	// accounting is internal-only and must not be forwarded to a caller
	// that did not request stream_options.include_usage). The extracted
	// usage above is still returned for internal cost/budget accounting.
	if !keepUsage {
		delete(m, "usage")
	}
	rewritten, err := json.Marshal(m)
	if err != nil {
		return payload, nil
	}
	return rewritten, usage
}

// usageFromRawMap decodes just the "usage" sub-value out of an already-parsed
// SSE/JSON object map. Returns nil when there's no usage field — the relay
// loop treats nil as "unknown", never zero (GATE-21).
func usageFromRawMap(m map[string]json.RawMessage) *Usage {
	raw, ok := m["usage"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var w wireUsage
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil
	}
	// toUsage returns nil when prompt/completion counts are missing — a
	// partial usage frame must NOT be treated as known-zero (GATE-21).
	return w.toUsage()
}

// writeStreamErrorEvent writes one SSE data frame carrying an error, used
// when the upstream stream breaks AFTER the first byte has already gone to
// the client (GATE-19: can't switch, can't change status — only emit an
// inline error event and close). The caller has already verified the
// response is mid-stream.
//
// rc's stream capture file is still open at this point (StreamUpstreamToClient
// deliberately leaves it open past its own return, see openStreamBodyFile's
// call site) — both frames written here are also appended to it, or the
// persisted "sent stream chunks" capture would end one frame short of what
// the real client actually received (code-review finding). The caller
// (handleStream) closes the file once this function returns.
func writeStreamErrorEvent(c *gin.Context, rc *RelayContext) {
	requestID := rc.RequestID
	msg := "upstream stream interrupted"
	if requestID != "" {
		msg = msg + " (request: " + requestID + ")" // GATE-08: caller can quote the id
	}
	evt := fmt.Sprintf(`data: {"error":{"message":%q,"type":"upstream_error"}}`+"\n\n", msg)
	evtBytes := []byte(evt)
	_, _ = c.Writer.Write(evtBytes)
	appendStreamBodyLine(rc, evtBytes)
	// Terminate the stream so OpenAI SDK clients that block on [DONE] to
	// finalize their completion unblock promptly instead of hanging until
	// their own read timeout.
	doneBytes := []byte("data: [DONE]\n\n")
	_, _ = c.Writer.Write(doneBytes)
	appendStreamBodyLine(rc, doneBytes)
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// openStreamBodyFile opens (create+append) the per-request stream capture
// file under the configured bodies directory (data/bodies/<request_id>.stream,
// PRD §6.8.4/§6.8.6). Failure to resolve the directory or open the file is
// NOT fatal to the request — the caller's own SSE stream is completely
// unaffected either way; only the audit capture is skipped (and, for a real
// OS-level open error, logged so an unwritable data/bodies dir is visible
// in ops logs instead of silently dropping every stream body forever).
func openStreamBodyFile(c *gin.Context, rc *RelayContext) {
	if rc.RequestID == "" {
		return
	}
	dir := streamBodiesDir(c)
	if dir == "" {
		return
	}
	path := filepath.Join(dir, rc.RequestID+".stream")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logger.Warn("gateway: open stream body file failed", zap.String("request_id", rc.RequestID), zap.Error(err))
		return
	}
	rc.streamBodyFile = f
	rc.streamBodyCaptured = true
	// A pre-first-byte failover can re-enter this function for the SAME
	// RequestID (same file, O_APPEND) after an earlier attempt already wrote
	// some preamble bytes to it — one Stat() here (not per appended line,
	// see appendStreamBodyLine) seeds the in-memory backstop counter with
	// the file's real starting size so it never drifts out of sync with what
	// prior attempts already wrote.
	if info, statErr := f.Stat(); statErr == nil {
		rc.streamBodyBytesWritten = info.Size()
	}
}

// closeStreamBodyFile closes the stream capture file opened by
// openStreamBodyFile, if any. Safe to call even when open never succeeded.
func closeStreamBodyFile(rc *RelayContext) {
	if rc.streamBodyFile != nil {
		_ = rc.streamBodyFile.Close()
		rc.streamBodyFile = nil
	}
}

// appendStreamBodyLine redacts and appends one already-caller-facing SSE
// line to the request's stream capture file. A no-op once the file was
// never opened (bodies dir unresolved / open failed) or the 1GiB backstop
// already fired for this request.
//
// maxStreamBodyFileBytes is a HARD anti-OOM disk backstop, NOT a content
// truncation (PRD §6.8.6/LOG-08: the capture must record every sent SSE
// fragment, never cut short for length) — a normal chat stream never comes
// close to it; only a hostile/buggy upstream maxing bandwidth within the
// 120s upstream timeout could. Hitting it sets rc.streamBodyTruncated=true
// (marked, never silent) and stops appending; the caller's own stream is
// entirely unaffected.
func appendStreamBodyLine(rc *RelayContext, line []byte) {
	if rc.streamBodyFile == nil || rc.streamBodyTruncated || len(line) == 0 {
		return
	}
	red := redact.RedactBytes(line, "")
	// rc.streamBodyBytesWritten tracks the file's size purely in memory
	// (initialized once from a real Stat() in openStreamBodyFile) — checking
	// the backstop this way, instead of Stat()-ing the file on every single
	// appended line, turns what could be hundreds of syscalls per stream
	// into zero (code-review efficiency finding).
	if rc.streamBodyBytesWritten+int64(len(red)) > maxStreamBodyFileBytes {
		rc.streamBodyTruncated = true
		return
	}
	n, err := rc.streamBodyFile.Write(red)
	rc.streamBodyBytesWritten += int64(n)
	if err != nil {
		logger.Warn("gateway: write stream body failed", zap.String("request_id", rc.RequestID), zap.Error(err))
	}
}

// removeEmptyStreamBodyFile deletes the stream capture file for rc if it
// ended up completely empty — the stream failed before any data ever
// reached the caller (a pre-first-byte candidate failover, or a caller
// disconnect before the first byte forwarded). Left in place, an empty
// stream_body_path would show as an empty, useless "stream body" link on
// the request-log detail page. A no-op when nothing was ever captured
// (streamBodyCaptured == false, e.g. bodies_dir was never resolved) or the
// file legitimately has content — streamBodyTruncated in particular guards
// against ever discarding a backstop-marked file, even though in practice
// that flag never fires on an empty file (it only fires after content was
// already appended).
func removeEmptyStreamBodyFile(c *gin.Context, rc *RelayContext) {
	if !rc.streamBodyCaptured || rc.streamBodyTruncated {
		return
	}
	dir := streamBodiesDir(c)
	if dir == "" {
		return
	}
	path := filepath.Join(dir, rc.RequestID+".stream")
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		return
	}
	_ = os.Remove(path)
	rc.streamBodyCaptured = false
}

// streamBodiesDir resolves the absolute data/bodies/ directory for this
// request. The gateway package has no direct access to app config (importing
// it would create a cycle back through cmd/config's dependents); instead
// serve.go resolves the absolute path once at boot, and a router-level
// middleware stashes it on every request's gin.Context under "bodies_dir"
// (internal/router/router.go, "bodies_dir" key). Empty means unresolved
// (e.g. a test gin.Context built without that middleware) — stream capture
// is silently skipped in that case; the caller's stream itself is never
// affected.
func streamBodiesDir(c *gin.Context) string {
	return c.GetString("bodies_dir")
}
