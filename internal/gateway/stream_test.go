package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/gateway/capture"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

func TestIsDataLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{`data: {"x":1}` + "\n", true},
		{`data:{"x":1}` + "\n", true}, // no space after colon — SSE spec
		{"data: [DONE]\n", true},
		{"data:\n", true},
		{": keepalive\n", false},
		{"event: ping\n", false},
		{"\n", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isDataLine([]byte(tt.line)); got != tt.want {
			t.Errorf("isDataLine(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestRewriteStreamChunkRewritesModel(t *testing.T) {
	payload := []byte(`{"model":"provider-x","choices":[]}`)
	out, usage := rewriteStreamChunk(payload, "external", true)
	if !bytes.Contains(out, []byte(`"model":"external"`)) {
		t.Errorf("model not rewritten back to external: %s", out)
	}
	if usage != nil {
		t.Errorf("expected nil usage for chunk without usage field, got %+v", usage)
	}
}

// TestRewriteStreamChunkNoModelNotInjected: a chunk that never carried a
// model field (usage-only / ping) must NOT have one injected — otherwise the
// gateway silently changes the upstream's wire shape.
func TestRewriteStreamChunkNoModelNotInjected(t *testing.T) {
	payload := []byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	out, usage := rewriteStreamChunk(payload, "external", true)
	if bytes.Contains(out, []byte(`"model"`)) {
		t.Errorf("model injected into a model-less chunk: %s", out)
	}
	if usage == nil || usage.PromptTokens != 5 || usage.CompletionTokens != 3 || usage.TotalTokens != 8 {
		t.Errorf("usage not extracted from model-less chunk: %+v", usage)
	}
}

// TestRewriteStreamChunkStripsUsageWhenNotKept: when the caller did not
// request stream_options.include_usage, the usage field is stripped from the
// forwarded payload (injected usage is internal-only). The usage
// is still extracted and returned for the gateway's own cost accounting.
func TestRewriteStreamChunkStripsUsageWhenNotKept(t *testing.T) {
	payload := []byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	out, usage := rewriteStreamChunk(payload, "external", false)
	if bytes.Contains(out, []byte(`"usage"`)) {
		t.Errorf("usage not stripped from forwarded frame: %s", out)
	}
	if usage == nil || usage.PromptTokens != 5 {
		t.Errorf("usage must still be extracted for internal accounting: %+v", usage)
	}
}

// TestRewriteStreamChunkNullPayloadNoPanic: a `data: null` frame must not
// panic on a nil-map write (regression guard for the parallel of
// rewriteModelField's nil guard).
func TestRewriteStreamChunkNullPayloadNoPanic(t *testing.T) {
	out, usage := rewriteStreamChunk([]byte(`null`), "external", true)
	if !bytes.Equal(out, []byte(`null`)) {
		t.Errorf("null payload should pass through unchanged, got %s", out)
	}
	if usage != nil {
		t.Errorf("expected nil usage for null payload, got %+v", usage)
	}
}

func TestRewriteStreamChunkInvalidJSONPassthrough(t *testing.T) {
	out, _ := rewriteStreamChunk([]byte(`{bad`), "external", true)
	if !bytes.Equal(out, []byte(`{bad`)) {
		t.Errorf("invalid JSON should pass through unchanged, got %s", out)
	}
}

func TestUsageFromRawMap(t *testing.T) {
	m := map[string]json.RawMessage{
		"usage": json.RawMessage(`{"prompt_tokens":2,"completion_tokens":4,"total_tokens":6}`),
	}
	u := usageFromRawMap(m)
	if u == nil || u.PromptTokens != 2 || u.CompletionTokens != 4 || u.TotalTokens != 6 {
		t.Errorf("usage wrong: %+v", u)
	}
	if usageFromRawMap(map[string]json.RawMessage{}) != nil {
		t.Error("expected nil for map without usage key")
	}
	if usageFromRawMap(map[string]json.RawMessage{"usage": json.RawMessage(`null`)}) != nil {
		t.Error("expected nil for literal-null usage")
	}
	// An empty usage object {} must NOT be treated as known-zero —
	// it has no prompt/completion counts, so it's "unknown".
	if usageFromRawMap(map[string]json.RawMessage{"usage": json.RawMessage(`{}`)}) != nil {
		t.Error("expected nil for empty usage object {}")
	}
	// A partial usage object missing completion_tokens is also unknown.
	if usageFromRawMap(map[string]json.RawMessage{"usage": json.RawMessage(`{"prompt_tokens":5}`)}) != nil {
		t.Error("expected nil for partial usage missing completion_tokens")
	}
}

func TestWriteStreamLineDataChunk(t *testing.T) {
	var buf bytes.Buffer
	wrote, usage, done, _, writeErr := writeStreamLine(&buf, []byte(`data: {"model":"p","choices":[]}`+"\n"), "ext", true)
	if writeErr != nil {
		t.Fatalf("unexpected write error: %v", writeErr)
	}
	if !wrote {
		t.Error("expected wroteData=true for a data line")
	}
	if done {
		t.Error("expected done=false for a regular data line")
	}
	if usage != nil {
		t.Errorf("expected nil usage, got %+v", usage)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"model":"ext"`)) {
		t.Errorf("model not rewritten in forwarded line: %s", buf.Bytes())
	}
}

func TestWriteStreamLineDone(t *testing.T) {
	var buf bytes.Buffer
	wrote, _, done, _, writeErr := writeStreamLine(&buf, []byte("data: [DONE]\n"), "ext", true)
	if writeErr != nil {
		t.Fatalf("unexpected write error: %v", writeErr)
	}
	if !wrote {
		t.Error("[DONE] should count as a data line")
	}
	if !done {
		t.Error("[DONE] should report done=true so the pump detects truncation when absent")
	}
	if !bytes.Equal(buf.Bytes(), []byte("data: [DONE]\n")) {
		t.Errorf("[DONE] not forwarded verbatim: %s", buf.Bytes())
	}
}

func TestWriteStreamLineNonDataPassthrough(t *testing.T) {
	var buf bytes.Buffer
	wrote, _, done, _, writeErr := writeStreamLine(&buf, []byte(": keepalive\n"), "ext", true)
	if writeErr != nil {
		t.Fatalf("unexpected write error: %v", writeErr)
	}
	if wrote {
		t.Error("non-data line should not count as data")
	}
	if done {
		t.Error("non-data line should not set done")
	}
	if !bytes.Equal(buf.Bytes(), []byte(": keepalive\n")) {
		t.Errorf("non-data line not forwarded verbatim: %s", buf.Bytes())
	}
}

// streamCaller is the request the streams below answer. includeUsage sets the
// one field that decides whether the upstream's usage frame is passed on to the
// caller or read and dropped.
func streamCaller(includeUsage bool) string {
	if includeUsage {
		return `{"model":"ext","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	}
	return `{"model":"ext","stream":true,"messages":[{"role":"user","content":"hi"}]}`
}

// pumpStream drives the same-protocol pump the way a delivery does: through the
// toolbox the kernel hands it, which is what opens the capture file and closes
// it again afterwards.
func pumpStream(t *testing.T, c *gin.Context, rc *Exchange, upstreamBody string, wantsUsage bool) (*protocols.IRUsage, error) {
	t.Helper()
	adm := admitFor(t, protocols.ProtocolOpenAI, "/v1/chat/completions", streamCaller(wantsUsage), Candidate{
		ProviderModelName: "real", EgressProtocol: protocols.ProtocolOpenAI, Passthrough: true,
	})
	tools, release := (&Service{}).newDeliveryTools(c, rc, TransferLimits{}, true)
	defer release()
	return adm.payload.(*textPayload).pumpSameProtocol(tools, &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		Header:     make(http.Header),
	})
}

// runStreamPump is pumpStream for the tests that care only about what it
// reported, not about what the caller received.
func runStreamPump(t *testing.T, upstreamBody string, wantsUsage bool) (*protocols.IRUsage, error) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return pumpStream(t, c, &Exchange{ingress: protocols.ProtocolOpenAI, isStream: true}, upstreamBody, wantsUsage)
}

// TestStreamUpstreamWithDoneSucceeds: a stream that emits a data frame and
// then `data: [DONE]` terminates cleanly with no error.
func TestStreamUpstreamWithDoneSucceeds(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	if _, err := runStreamPump(t, body, true); err != nil {
		t.Fatalf("expected nil error for stream with [DONE], got %v", err)
	}
}

// TestStreamUpstreamNoDoneReturnsTruncationError: a stream that emits at
// least one data frame but closes WITHOUT `data: [DONE]` must report a
// truncation so the delivery is recorded as partial rather than a clean success
// — the client already received bytes, so it is a silent truncation, not a
// clean completion.
//
// Which truncation it is depends on whether anything else vouched for the
// completion; the pair below is what says so.
func TestStreamUpstreamNoDoneReturnsTruncationError(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	_, err := runStreamPump(t, body, true)
	if !errors.Is(err, errStreamEndedUnannounced) {
		t.Fatalf("expected errStreamEndedUnannounced, got %v", err)
	}
}

// TestAStreamMissingOnlyItsTerminatorIsToldFromOneCutShort pins the split that
// decides whether an operator hears about a stream.
//
// Both bodies end without `data: [DONE]`, and until they were told apart both
// produced the same reason code. One of them is a provider quirk that costs
// nothing — several upstreams never send the sentinel — and warning on it means
// a warning per SUCCESSFUL request, which is how a warning stops being read.
// The other is a completion that stopped in the middle: billed 200, committed
// 200, recorded as delivered, indistinguishable from a success in every column
// that gets persisted. Collapse the two and one of those outcomes is guaranteed
// to be wrong.
//
// The usage frame is what separates them, and it is available because this
// gateway asks every upstream for usage whether or not the caller wanted it. A
// stream that produced its final usage got to its end; one that produced
// neither usage nor terminator said nothing about having finished at all.
func TestAStreamMissingOnlyItsTerminatorIsToldFromOneCutShort(t *testing.T) {
	const delta = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	const usageFrame = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n"

	t.Run("the provider that never sends [DONE]", func(t *testing.T) {
		usage, err := runStreamPump(t, delta+usageFrame, true)
		if !errors.Is(err, errStreamNoDoneTerminator) {
			t.Fatalf("err = %v, want errStreamNoDoneTerminator: the final usage arrived, "+
				"so the only thing missing is a sentinel this provider does not send", err)
		}
		if usage == nil {
			t.Fatal("no usage reported; then the two cases were not actually told apart here")
		}
	})

	t.Run("the completion that stopped in the middle", func(t *testing.T) {
		_, err := runStreamPump(t, delta, true)
		if !errors.Is(err, errStreamEndedUnannounced) {
			t.Fatalf("err = %v, want errStreamEndedUnannounced: nothing in this stream "+
				"said it had finished", err)
		}
	})
}

// cancelAfterReader delivers all of its bytes on the first Read, then cancels
// the request context and returns context.Canceled on the next Read — modeling
// a caller that closes the connection right after receiving the full response
// (including [DONE]), so the following upstream body read fails with a
// ctx-canceled error.
type cancelAfterReader struct {
	data   []byte
	off    int
	cancel context.CancelFunc
}

func (r *cancelAfterReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	r.cancel()
	return 0, context.Canceled
}

// TestStreamUpstreamPostDoneDisconnectSucceeds: a stream that emits its data
// frames and `data: [DONE]`, after which the caller disconnects (surfacing as
// a ctx-canceled body read), must finish as SUCCESS — not
// errClientDisconnected. OpenAI SDKs close the connection the moment they read
// [DONE], before the pump reaches the trailing blank line, so the common case
// for a fully-delivered stream is a post-[DONE] disconnect; labeling it 499
// would mislabel the vast majority of successful streams (regression guard).
func TestStreamUpstreamPostDoneDisconnectSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(&cancelAfterReader{data: []byte(body), cancel: cancel}),
		Header:     make(http.Header),
	}
	rc := &Exchange{ingress: protocols.ProtocolOpenAI, isStream: true}
	adm := admitFor(t, protocols.ProtocolOpenAI, "/v1/chat/completions", streamCaller(true), Candidate{
		ProviderModelName: "real", EgressProtocol: protocols.ProtocolOpenAI, Passthrough: true,
	})
	tools, release := (&Service{}).newDeliveryTools(c, rc, TransferLimits{}, true)
	defer release()
	_, err := adm.payload.(*textPayload).pumpSameProtocol(tools, resp)
	if err != nil {
		t.Fatalf("post-[DONE] disconnect must succeed, got %v", err)
	}
}

// TestStreamUpstreamStripsInjectedUsage: with WantsStreamUsage=false, the
// final usage frame the gateway requested upstream must NOT be forwarded to
// the caller. The pump still extracts usage internally — verified
// here by checking the recorder's body has no usage field.
func TestStreamUpstreamStripsInjectedUsage(t *testing.T) {
	body := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rc := &Exchange{ingress: protocols.ProtocolOpenAI, isStream: true}
	usage, err := pumpStream(t, c, rc, body, false)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if usage == nil || usage.PromptTokens != 5 {
		t.Errorf("internal usage must still be extracted: %+v", usage)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"usage"`)) {
		t.Errorf("injected usage leaked to caller (WantsStreamUsage=false): %s", rec.Body.Bytes())
	}
}

// runStreamPumpCapture is runStreamPump plus the BodiesDirContextKey value
// and a RequestID (stream body capture: internal/router/router.go
// stashes the absolute bodies dir on every request's gin.Context; here the
// test wires it directly instead of going through the real middleware). It
// returns the Exchange so callers can inspect StreamCaptured()/
// StreamTruncated() and the recorder so callers can check the bytes the
// caller actually received.
func runStreamPumpCapture(t *testing.T, upstreamBody, requestID, bodiesDir string) (*Exchange, *httptest.ResponseRecorder, *protocols.IRUsage, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(BodiesDirContextKey, bodiesDir)
	rc := &Exchange{requestID: requestID, ingress: protocols.ProtocolOpenAI, isStream: true}
	usage, err := pumpStream(t, c, rc, upstreamBody, true)
	return rc, rec, usage, err
}

// sseDataFrames builds n SSE `data:` frames, each carrying a `content` field
// padded to at least payloadBytes so the caller can control the aggregate
// stream size precisely, followed by the `data: [DONE]` terminator.
func sseDataFrames(n, payloadBytes int) string {
	pad := strings.Repeat("A", payloadBytes)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"content":"`)
		b.WriteString(pad)
		b.WriteString(`"}}]}` + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestStreamCaptureNoTruncation: a >2MB SSE stream is
// captured in full under data/bodies/<request_id>.stream — no truncation
// below the 1GiB backstop — while the caller's own stream is unaffected.
func TestStreamCaptureNoTruncation(t *testing.T) {
	dir := t.TempDir()
	body := sseDataFrames(3000, 1000) // ~3MB of data frames, well over 2MB
	rc, rec, _, err := runStreamPumpCapture(t, body, "req-no-trunc", dir)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("data: [DONE]")) {
		t.Fatalf("caller stream did not complete: %s", rec.Body.String()[:200])
	}
	if rc.bodies.StreamTruncated() {
		t.Error("expected streamBodyTruncated=false (well under the 1GiB backstop)")
	}
	captured, err := os.ReadFile(filepath.Join(dir, "req-no-trunc.stream"))
	if err != nil {
		t.Fatalf("read captured stream file: %v", err)
	}
	if len(captured) < 2<<20 {
		t.Fatalf("captured file too small: %d bytes, want > 2MB", len(captured))
	}
	// The captured bytes must be the caller-facing (post-rewrite) content,
	// not just any bytes — every content frame's padding must be present.
	if bytes.Count(captured, []byte(strings.Repeat("A", 1000))) != 3000 {
		t.Errorf("captured file is missing data frames: found %d of 3000", bytes.Count(captured, []byte(strings.Repeat("A", 1000))))
	}
	if !bytes.Contains(captured, []byte("data: [DONE]")) {
		t.Error("captured file missing the [DONE] terminator line")
	}
}

// TestStreamCaptureBackstopMarked: once the (test-shrunk)
// capture.MaxStreamFileBytes cap is hit, the capture is marked truncated, the
// file stops growing past the cap, and the caller's own stream still
// completes normally — the backstop only stops the disk audit copy, never
// the client-facing stream.
func TestStreamCaptureBackstopMarked(t *testing.T) {
	orig := capture.MaxStreamFileBytes
	capture.MaxStreamFileBytes = 500 // test-only small cap; avoids writing a real 1GiB
	defer func() { capture.MaxStreamFileBytes = orig }()

	dir := t.TempDir()
	body := sseDataFrames(50, 200) // far larger than the 500-byte test cap
	rc, rec, _, err := runStreamPumpCapture(t, body, "req-backstop", dir)
	if err != nil {
		t.Fatalf("expected nil error (backstop must not break the caller's stream), got %v", err)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("data: [DONE]")) {
		t.Fatalf("caller stream did not complete despite the backstop: %s", rec.Body.String()[:200])
	}
	if !rc.bodies.StreamTruncated() {
		t.Error("expected streamBodyTruncated=true once the test cap was exceeded")
	}
	info, err := os.Stat(filepath.Join(dir, "req-backstop.stream"))
	if err != nil {
		t.Fatalf("stat captured stream file: %v", err)
	}
	if info.Size() > capture.MaxStreamFileBytes {
		t.Errorf("captured file grew past the backstop cap: %d bytes, cap %d", info.Size(), capture.MaxStreamFileBytes)
	}
}

// TestStreamCaptureVerbatim: v0.1 does NOT scrub body content, so an
// SSE data line is persisted to the stream capture file exactly as it was
// forwarded to the caller (the gateway only rewrites model/usage fields, never
// arbitrary content).
func TestStreamCaptureVerbatim(t *testing.T) {
	dir := t.TempDir()
	content := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	body := `data: {"choices":[{"delta":{"content":"` + content + `"}}]}` + "\n\n" + "data: [DONE]\n\n"
	rc, _, _, err := runStreamPumpCapture(t, body, "req-verbatim", dir)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	captured, err := os.ReadFile(filepath.Join(dir, "req-verbatim.stream"))
	if err != nil {
		t.Fatalf("read captured stream file: %v", err)
	}
	if !bytes.Contains(captured, []byte(content)) {
		t.Errorf("expected content preserved verbatim in the captured stream file: %s", captured)
	}
	if bytes.Contains(captured, []byte("[REDACTED]")) {
		t.Errorf("v0.1 must not redact stream body content: %s", captured)
	}
	if !rc.bodies.StreamCaptured() {
		t.Error("expected streamBodyCaptured to be true")
	}
}

// TestWriteStreamErrorEventOpenAIIngress: a mid-stream failure on the
// OpenAI ingress (/v1/chat/completions) must keep producing the original
// OpenAI wire shape — an inline `data: {"error":...}` frame followed by
// `data: [DONE]` so OpenAI SDKs blocked on [DONE] unblock promptly.
func TestWriteStreamErrorEventOpenAIIngress(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rc := &Exchange{requestID: "req-openai-mid", ingress: protocols.ProtocolOpenAI}

	_ = writeStreamErrorEvent(committedStreamClient(t, c, rc), rc.ingress, rc.requestID)

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Errorf("expected OpenAI upstream_error frame, got %q", body)
	}
	if !strings.Contains(body, "req-openai-mid") {
		t.Errorf("expected the request id quoted in the message, got %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("OpenAI ingress must still get the [DONE] terminator, got %q", body)
	}
	if strings.Contains(body, "event: error") {
		t.Errorf("OpenAI ingress must not get the Claude event: error framing, got %q", body)
	}
}

// TestWriteStreamErrorEventClaudeIngress: a mid-stream failure on the Claude
// ingress (/v1/messages) must emit the Anthropic streaming error shape
// (event: error + a top-level "type":"error" envelope) and must NOT emit the
// OpenAI [DONE] terminator — Claude has no such convention, and sending it
// would be a protocol violation the Anthropic SDK does not expect mid-stream.
func TestWriteStreamErrorEventClaudeIngress(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rc := &Exchange{requestID: "req-claude-mid", ingress: protocols.ProtocolClaude}

	_ = writeStreamErrorEvent(committedStreamClient(t, c, rc), rc.ingress, rc.requestID)

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("expected a Claude event: error frame, got %q", body)
	}
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("expected the top-level Anthropic \"type\":\"error\" discriminator, got %q", body)
	}
	if !strings.Contains(body, `"type":"api_error"`) {
		t.Errorf("expected the nested error.type=api_error, got %q", body)
	}
	if !strings.Contains(body, "req-claude-mid") {
		t.Errorf("expected the request id quoted in the message, got %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Claude ingress must NOT emit the OpenAI [DONE] terminator, got %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("expected the SSE event to end with the blank-line terminator, got %q", body)
	}
}

// TestWriteStreamErrorEventCapturesToStreamFile: whatever bytes
// writeStreamErrorEvent sends to the client must also land in the
// per-request stream capture file, for both ingress protocols — the capture
// file's contract is "exactly what the client received".
func TestWriteStreamErrorEventCapturesToStreamFile(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
	}{
		{"openai", "/v1/chat/completions"},
		{"claude", "/v1/messages"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, nil)
			c.Set(BodiesDirContextKey, dir)
			rc := &Exchange{requestID: "req-" + tt.name + "-capture", ingress: IngressProtocol(tt.path)}
			openStreamBodyFile(c, rc)
			defer rc.bodies.CloseStream()

			_ = writeStreamErrorEvent(committedStreamClient(t, c, rc), rc.ingress, rc.requestID)

			captured, err := os.ReadFile(filepath.Join(dir, rc.requestID+".stream"))
			if err != nil {
				t.Fatalf("read captured stream file: %v", err)
			}
			if !bytes.Equal(captured, rec.Body.Bytes()) {
				t.Errorf("captured stream file = %q, want it byte-for-byte equal to the client bytes %q", captured, rec.Body.Bytes())
			}
		})
	}
}

// TestAWriterThatCannotTakeADeadlineIsReportedOnce guards the warn-once gate on
// the sliding write deadline: a writer that cannot honor SetWriteDeadline fails
// identically on every forwarded chunk, so the warning must be logged exactly
// once per request. Production *http.response always supports it; the warning
// exists so a wrapper that forgets to unwrap cannot silently disable the
// slow-client protection.
func TestAWriterThatCannotTakeADeadlineIsReportedOnce(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "test.log")
	logger.Init(logger.Config{Level: "warn", Filename: logFile, Console: false})
	t.Cleanup(func() {
		_ = logger.Sync()
		logger.Init(logger.Config{Filename: os.DevNull})
	})

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder() // does not support SetWriteDeadline
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/x", nil)

	client := &ginClientResponse{c: c, rc: &Exchange{requestID: "req-warn-once"}, window: time.Second}
	for range 5 {
		client.slideDeadline()
	}
	_ = logger.Sync()

	data, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatalf("read log file: %v", readErr)
	}
	count := strings.Count(string(data), "gateway: apply write deadline failed")
	if count != 1 {
		t.Errorf("expected exactly 1 warning log line despite 5 failing calls, got %d: %s", count, data)
	}
	if !strings.Contains(string(data), "req-warn-once") {
		t.Error("warning log must carry the request ID")
	}
}

// The descriptor-identity half of the double-open guarantee (two callers both
// believe they open this file; the second open must keep the first
// descriptor, or it leaks) lives with the capture package now — the handle is
// its private state. What stays here is the gateway wiring: a second
// openStreamBodyFile call is harmless and the capture keeps working.
func TestOpeningTheCaptureFileTwiceKeepsCapturing(t *testing.T) {
	dir := t.TempDir()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(BodiesDirContextKey, dir)
	rc := &Exchange{requestID: "twice-opened"}

	openStreamBodyFile(c, rc)
	defer rc.bodies.CloseStream()
	if !rc.bodies.StreamCaptured() {
		t.Fatal("first open produced no capture file")
	}

	openStreamBodyFile(c, rc)
	appendStreamBodyLine(rc, []byte("data: once\n\n"))
	rc.bodies.CloseStream()
	captured, err := os.ReadFile(filepath.Join(dir, "twice-opened.stream"))
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	if strings.Count(string(captured), "data: once") != 1 {
		t.Fatalf("capture after a double open holds %q, want the line exactly once", captured)
	}
}

// TestANewAttemptReopensTheCaptureFileAndAppends pins the case the guard above
// must not break.
//
// A pre-first-byte failover closes the file on its way out and opens it again
// for the next attempt, against the same request ID. That second open has to
// happen — and has to keep what the first attempt wrote, since the capture is
// the whole exchange, not the last try at it.
func TestANewAttemptReopensTheCaptureFileAndAppends(t *testing.T) {
	dir := t.TempDir()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(BodiesDirContextKey, dir)
	rc := &Exchange{requestID: "reopened"}

	openStreamBodyFile(c, rc)
	appendStreamBodyLine(rc, []byte("data: first\n\n"))
	rc.bodies.CloseStream()

	openStreamBodyFile(c, rc)
	appendStreamBodyLine(rc, []byte("data: second\n\n"))
	rc.bodies.CloseStream()

	captured, err := os.ReadFile(filepath.Join(dir, rc.requestID+".stream"))
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	if want := "data: first\n\ndata: second\n\n"; string(captured) != want {
		t.Errorf("capture is %q, want %q", captured, want)
	}
}

// A commit-refusing writer stands in for the response object this pump will be
// handed once the kernel owns delivery: one that knows whether the response has
// already been committed, and says no when it has.
// TestARefusedCommitEndsTheRequestInsteadOfTryingAnotherProvider pins the one
// branch that has to exist before the writer that triggers it does.
//
// A refused commit means the caller already holds a response somebody else
// wrote. Nothing of ours reached them, which is also what a dead upstream looks
// like from here — and that reading sends the chain to the next provider. It
// must not: no second attempt can reach a caller who has already been answered,
// so the only thing another provider buys is one more upstream call and a
// second stream aimed at a committed response.
func TestARefusedCommitEndsTheRequestInsteadOfTryingAnotherProvider(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rc := &Exchange{requestID: "commit-refused", ingress: protocols.ProtocolOpenAI, isStream: true}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("data: {\"id\":\"x\"}\n\ndata: [DONE]\n\n")),
	}
	adm := admitFor(t, protocols.ProtocolOpenAI, "/v1/chat/completions", streamCaller(true), Candidate{
		ProviderModelName: "m", EgressProtocol: protocols.ProtocolOpenAI, Passthrough: true,
	})
	tools, release := (&Service{}).newDeliveryTools(c, rc, TransferLimits{}, true)
	defer release()
	// A response somebody else already committed: the stub refuses a second
	// commit exactly as the real one does.
	tools.Client = &stubClient{committed: true, status: http.StatusOK}

	got := adm.payload.Deliver(tools, resp)

	if got.Verdict != fact.VerdictSettled {
		t.Errorf("verdict is %v, want settled — another candidate cannot reach a caller who already has a response", got.Verdict)
	}
	if got.Fault != fact.FaultGateway {
		t.Errorf("fault is %v, want gateway — two writers on one response is ours, not the provider's", got.Fault)
	}
	if got.Complete {
		t.Error("delivery reports complete; nothing this pump had to send ever reached the caller")
	}
	if got.FailReason != "client_commit_refused" {
		t.Errorf("fail reason is %q, want client_commit_refused", got.FailReason)
	}
	if rc.firstByteSent {
		t.Error("first byte recorded as sent; the commit that would have sent it was refused")
	}
}
