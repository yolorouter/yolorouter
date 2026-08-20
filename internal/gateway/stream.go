package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// errClientDisconnected is returned by the stream pump when the caller's
// request context is cancelled. It is not a real upstream failure
// — the relay loop records it as a distinct outcome so the request log shows
// "caller cancelled", not "upstream failed".
var errClientDisconnected = errors.New("client disconnected")

// clientDisconnectOutcome maps a caller disconnect detected inside the stream
// pump to a pump result, keeping the post-[DONE] completion semantics in ONE
// place (both ctx-cancel exit paths — the in-loop select and the post-scan
// scanner.Err branch — call this, so they can never drift apart).
//
// A disconnect AFTER the `data: [DONE]` terminator was forwarded is NOT a real
// cancellation: the caller received the complete response and closed the
// connection. OpenAI SDKs close the moment they read [DONE], before the pump
// reaches the trailing blank line, so the very next ctx check fires — reporting
// 499 there would mislabel a fully-delivered stream as client_disconnected (the
// common case: most streams end this way). Only a disconnect BEFORE [DONE] is a
// genuine caller cancel (-> 499).
func clientDisconnectOutcome(usage *protocols.IRUsage, doneSeen bool) (*protocols.IRUsage, error) {
	if doneSeen {
		return usage, nil
	}
	return usage, errClientDisconnected
}

// errStreamNoDoneTerminator is returned when a stream closed without its
// `data: [DONE]` terminator but did deliver what only a finished stream
// delivers — the final usage frame, which this gateway asks every upstream for
// whether or not the caller wanted it.
//
// So the caller most likely holds the whole completion and the provider simply
// never sends the sentinel, which several of them do not. It is still not
// recorded as complete, because the end was never announced; what it is not is
// something to wake anybody over.
var errStreamNoDoneTerminator = errors.New("upstream stream ended without [DONE] terminator")

// errStreamEndedUnannounced is the other half of that: the body ended cleanly
// with nothing at all saying the completion had finished — no terminator, and
// none of the trailing matter a finished stream carries.
//
// Told apart from the case above because they deserve opposite treatment and
// used to share one code. A caller was handed a 200 and part of an answer,
// which every column that gets persisted records as a success, so the one
// warning this produces is all there is to go on. No error frame is injected
// into the response: a decoder that failed to recognise a terminal event would
// land here too, and breaking a completion that turned out to be whole is worse
// than logging one that was not.
var errStreamEndedUnannounced = errors.New("upstream stream ended without announcing completion")

// errClientCommitRefused is returned by a stream pump when the response it was
// about to send has already been committed by something else.
//
// It is its own sentinel because of what the alternative costs. Nothing of ours
// reached the caller, so every generic "nothing was sent" path treats this as
// pre-first-byte and offers the chain another candidate — which would send a
// second provider's stream into a response that already carries somebody's
// status. The caller is served either way; the only question left is whether we
// also spend an upstream call discovering that again.
var errClientCommitRefused = errors.New("response was already committed elsewhere")

// errAlreadyCommitted is what a refused commit says on top of the sentinel.
var errAlreadyCommitted = errors.New("gateway: response is already committed")

// maxPreambleBytes caps the pre-first-data-frame preamble buffer in the
// passthrough stream pump — a malicious/buggy upstream could otherwise grow
// it without bound (the response body has no bodylimit guard the way the
// request body does).
const maxPreambleBytes = 64 * 1024

// maxStreamLineBytes caps a single SSE line. bufio.Reader.ReadBytes doesn't
// bound line length itself — without this a malicious/buggy upstream sending
// a very long line without a newline could grow the in-memory buffer without
// limit (the response body has no bodylimit guard the way the request does).
const maxStreamLineBytes = 1 * 1024 * 1024 // 1 MiB

func writeSSEHeader(w protocols.ClientWriter) error {
	w.Inject(http.Header{
		"Content-Type":  {"text/event-stream"},
		"Cache-Control": {"no-cache"},
		"Connection":    {"keep-alive"},
		// Disable proxy buffering (nginx X-Accel-Buffering et al) so SSE chunks
		// reach the client token-by-token instead of in buffered batches.
		"X-Accel-Buffering": {"no"},
	})
	return w.Commit(http.StatusOK)
}

func isDataLine(line []byte) bool {
	// The prefix rule is shared with the decoders, so what counts as a data
	// line cannot drift between this forwarder and the parsing side. Like
	// every byte-preserving reader it does not trim leading whitespace off
	// the line — that is the helper's documented split with the parse-side
	// reading.
	_, ok := protocols.SSEDataPayloadStart(bytes.TrimRight(line, "\r\n"))
	return ok
}

// writeStreamLine writes one SSE line to the client, rewriting the model
// field when the line is a `data: {json}` chunk. Returns wroteData=true if
// the line was a data line (counts toward the first-byte decision), the
// usage extracted from this chunk (the final usage chunk carries
// prompt/completion tokens), done=true if the line was the [DONE]
// terminator, sent = the exact bytes written to w (caller-facing,
// post-rewrite/post-usage-strip) — the stream body capture appends
// this, never the raw pre-rewrite input line — and writeErr, the error (if
// any) from the underlying Write. Every branch below still returns wroteData
// unconditionally (matching the pre-error-propagation behavior of this
// function): whether a data line's write actually landed on the wire is
// captured in writeErr, not in wroteData.
func writeStreamLine(w io.Writer, line []byte, externalModel string, keepUsage bool) (wroteData bool, usage *protocols.IRUsage, done bool, sent []byte, writeErr error) {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		// Non-data line (blank separator, event:/id:/retry: headers) —
		// forward verbatim so the SSE framing stays intact.
		_, err := w.Write(line)
		return false, nil, false, line, err
	}
	// SSE allows "data:" or "data: " — the optional single space after the
	// colon is framing, not part of the value.
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 {
		_, err := w.Write(line)
		return true, nil, false, line, err
	}
	if string(payload) == "[DONE]" {
		out := []byte("data: [DONE]\n")
		_, err := w.Write(out)
		return true, nil, true, out, err
	}
	rewritten, u := rewriteStreamChunk(payload, externalModel, keepUsage)
	out := make([]byte, 0, len(rewritten)+len("data: ")+1)
	out = append(out, "data: "...)
	out = append(out, rewritten...)
	out = append(out, '\n')
	_, err := w.Write(out)
	return true, u, false, out, err
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
// own cost accounting, but does not forward it to the caller.
func rewriteStreamChunk(payload []byte, externalModel string, keepUsage bool) ([]byte, *protocols.IRUsage) {
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
	// for it (usage the gateway injected for its own cost
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
// loop treats nil as "unknown", never zero.
func usageFromRawMap(m map[string]json.RawMessage) *protocols.IRUsage {
	raw, ok := m["usage"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var w wireUsage
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil
	}
	// toIRUsage returns nil when prompt/completion counts are missing — a
	// partial usage frame must NOT be treated as known-zero.
	return w.toIRUsage()
}

// writeStreamErrorEvent writes the ingress protocol's mid-stream error
// frames, used when the upstream stream breaks AFTER the first byte has
// already gone to the client (can't switch, can't change status — only emit
// an inline error event and close). The caller has already verified the
// response is mid-stream.
//
// The frame shape and the terminator convention are the registry entry's
// knowledge (each protocol package documents its own); this function only
// sends what the entry built, stopping at the first write error so a
// terminator is never written to a client the error frame already failed to
// reach.
//
// The stream capture file is still open at this point — the pumps deliberately
// leave it open past their own return — so these frames land in it alongside
// everything else the caller received, rather than the record ending one frame
// short of the real response. The write deadline slides on each write, so a
// body idle timeout longer than the write window cannot leave the deadline
// already expired by the time this last frame goes out.
//
// Returns the first write error so callers can react; callers already
// committed to finalizing may discard it.
func writeStreamErrorEvent(w ClientResponse, ingress protocols.ProtocolID, requestID string) error {
	for _, frame := range codecsFor(ingress).StreamErrorFrames(streamErrorMessage(requestID)) {
		if err := sendSSEFrame(w, frame); err != nil {
			return err
		}
	}
	return nil
}

// streamErrorMessage builds the generic mid-stream failure message shared by
// every ingress protocol — no upstream detail is leaked, only the request id
// so the caller can quote it to support.
func streamErrorMessage(requestID string) string {
	return protocols.AppendRequestID("upstream stream interrupted", requestID)
}

// sendSSEFrame writes one SSE frame to the client and makes sure it actually
// left before the caller treats it as sent.
//
// The flush is not optional bookkeeping: net/http can take a small Write into
// its own buffer and return nil, so a frame reported as written may never have
// reached the socket. It is also the moment the response object records what
// went out, and it records only after the flush succeeded — a frame the caller
// never received must not appear in the account of what they got. Either
// failure comes back so the caller can stop rather than write the frames that
// would follow it.
func sendSSEFrame(w ClientResponse, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}

// flushAndCheckError flushes the response writer and returns any deferred
// write error. Thin wrapper around protocols.FlushAndCheckError: the
// canonical unwrap-and-flush implementation lives in internal/protocols
// (this package already depends on it, and protocols cannot depend back on
// this package), so the IR streaming relay loops and this package's
// same-protocol passthrough stream pumps share one implementation instead of
// drifting apart.
func flushAndCheckError(c *gin.Context) error {
	return protocols.FlushAndCheckError(c)
}

// isClientWriteError reports whether err represents a downstream (client-side)
// write failure rather than an upstream server fault. The streaming relay
// loops (IRStreamRelay / IRStreamRelayJSONLines) and the same-protocol
// passthrough pumps wrap every downstream Write/Flush error with
// protocols.ErrClientWrite; this classifier matches ONLY that sentinel, so a
// slow or gone client is not misclassified as AttemptServerError.
//
// Only the dedicated sentinel is used for classification — neither a broad
// net.Error.Timeout() check nor the bare syscalls (EPIPE / ECONNRESET /
// io.ErrClosedPipe) are matched here. Both are ambiguous on the READ side:
// an upstream RST while reading the response body surfaces through the exact
// same syscalls, and context.DeadlineExceeded (upstream attempt timeout)
// also satisfies net.Error.Timeout(). Only a write-site error explicitly
// wrapped in ErrClientWrite is unambiguous.
//
// The error reaches here wrapped (e.g. "stream write failed: ErrClientWrite:
// write to client: <root>") via fmt.Errorf("...: %w", ...) chains; errors.As
// / errors.Is unwrap through all layers.
func isClientWriteError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, protocols.ErrClientWrite)
}

// openStreamBodyFile opens (create+append) the per-request stream capture
// file under the configured bodies directory (data/bodies/<request_id>.stream).
// Failure to resolve the directory or open the file is
// NOT fatal to the request — the caller's own SSE stream is completely
// unaffected either way; only the audit capture is skipped (and, for a real
// OS-level open error, logged so an unwritable data/bodies dir is visible
// in ops logs instead of silently dropping every stream body forever).
//
// Calling it twice for one attempt keeps the first file rather than opening a
// second one. Two callers is the state of a half-finished move of this
// decision, and the alternative — overwriting the capture handle — drops the
// only reference to a descriptor nothing will ever close. Re-entry BETWEEN
// attempts is different and still opens: the previous attempt's file was
// closed on its way out, and O_APPEND is what lets a failover keep writing
// into the bytes an earlier attempt already captured.
func openStreamBodyFile(c *gin.Context, rc *Exchange) {
	if rc.requestID == "" {
		return
	}
	dir := streamBodiesDir(c)
	if dir == "" {
		return
	}
	if err := rc.bodies.OpenStream(dir, rc.requestID); err != nil {
		logger.Warn("gateway: open stream body file failed", zap.String("request_id", rc.requestID), zap.Error(err))
	}
}

// appendStreamBodyLine appends one already-caller-facing SSE line to the
// request's stream capture file, verbatim (v0.1 does not scrub body content).
// A no-op once the file was never opened (bodies dir unresolved / open failed)
// or the anti-OOM backstop already fired for this request — the backstop
// itself, and why it marks rather than silently cuts, is documented on
// capture.MaxStreamFileBytes.
func appendStreamBodyLine(rc *Exchange, line []byte) {
	if err := rc.bodies.AppendStream(line); err != nil {
		logger.Warn("gateway: write stream body failed", zap.String("request_id", rc.requestID), zap.Error(err))
	}
}

// BodiesDirContextKey is the gin.Context key under which a router-level
// middleware stashes the absolute data/bodies/ directory for every gateway
// request. Exported and shared so the setter (internal/router/router.go) and
// this reader can't silently drift apart into two mismatched string literals
// that would disable stream capture with no error.
const BodiesDirContextKey = "bodies_dir"

// streamBodiesDir resolves the absolute data/bodies/ directory for this
// request. The gateway package has no direct access to app config (importing
// it would create a cycle back through cmd/config's dependents); instead
// serve.go resolves the absolute path once at boot, and a router-level
// middleware stashes it on every request's gin.Context under
// BodiesDirContextKey (internal/router/router.go). Empty means unresolved
// (e.g. a test gin.Context built without that middleware) — stream capture
// is silently skipped in that case; the caller's stream itself is never
// affected.
func streamBodiesDir(c *gin.Context) string {
	return c.GetString(BodiesDirContextKey)
}
