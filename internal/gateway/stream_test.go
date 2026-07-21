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

	"github.com/gin-gonic/gin"
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
// forwarded payload (PRD §1114: injected usage is internal-only). The usage
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
	// GATE-21: an empty usage object {} must NOT be treated as known-zero —
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
	wrote, usage, done, _ := writeStreamLine(&buf, []byte(`data: {"model":"p","choices":[]}`+"\n"), "ext", true)
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
	wrote, _, done, _ := writeStreamLine(&buf, []byte("data: [DONE]\n"), "ext", true)
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
	wrote, _, done, _ := writeStreamLine(&buf, []byte(": keepalive\n"), "ext", true)
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

// runStreamPump wires a minimal gin context + http.Response around
// StreamUpstreamToClient so the truncation/usage tests don't repeat the
// boilerplate.
func runStreamPump(t *testing.T, upstreamBody string, wantsUsage bool) (*Usage, error) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		Header:     make(http.Header),
	}
	rc := &RelayContext{OriginalModel: "ext", IsStream: true, WantsStreamUsage: wantsUsage}
	return StreamUpstreamToClient(c, resp, rc)
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
// least one data frame but closes WITHOUT `data: [DONE]` must report
// errStreamNoDoneTerminator so handleStream logs a partial row instead of
// clean success (the client already received bytes, so it's a silent
// truncation, not a clean completion).
func TestStreamUpstreamNoDoneReturnsTruncationError(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	_, err := runStreamPump(t, body, true)
	if !errors.Is(err, errStreamNoDoneTerminator) {
		t.Fatalf("expected errStreamNoDoneTerminator, got %v", err)
	}
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
	rc := &RelayContext{OriginalModel: "ext", IsStream: true, WantsStreamUsage: true}
	_, err := StreamUpstreamToClient(c, resp, rc)
	if err != nil {
		t.Fatalf("post-[DONE] disconnect must succeed, got %v", err)
	}
}

// TestStreamUpstreamStripsInjectedUsage: with WantsStreamUsage=false, the
// final usage frame the gateway requested upstream must NOT be forwarded to
// the caller (PRD §1114). The pump still extracts usage internally — verified
// here by checking the recorder's body has no usage field.
func TestStreamUpstreamStripsInjectedUsage(t *testing.T) {
	body := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
	rc := &RelayContext{OriginalModel: "ext", IsStream: true, WantsStreamUsage: false}
	usage, err := StreamUpstreamToClient(c, resp, rc)
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
// and a RequestID (Task 5's stream body capture: internal/router/router.go
// stashes the absolute bodies dir on every request's gin.Context; here the
// test wires it directly instead of going through the real middleware). It
// returns the RelayContext so callers can inspect streamBodyCaptured/
// streamBodyTruncated and the recorder so callers can check the bytes the
// caller actually received.
func runStreamPumpCapture(t *testing.T, upstreamBody, requestID, bodiesDir string) (*RelayContext, *httptest.ResponseRecorder, *Usage, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(BodiesDirContextKey, bodiesDir)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		Header:     make(http.Header),
	}
	rc := &RelayContext{RequestID: requestID, OriginalModel: "ext", IsStream: true, WantsStreamUsage: true}
	usage, err := StreamUpstreamToClient(c, resp, rc)
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

// TestStreamCaptureNoTruncation (Task 5, Codex #4): a >2MB SSE stream is
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
	if rc.streamBodyTruncated {
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

// TestStreamCaptureBackstopMarked (Task 5, Codex #4): once the (test-shrunk)
// maxStreamBodyFileBytes cap is hit, streamBodyTruncated flips true, the
// file stops growing past the cap, and the caller's own stream still
// completes normally — the backstop only stops the disk audit copy, never
// the client-facing stream.
func TestStreamCaptureBackstopMarked(t *testing.T) {
	orig := maxStreamBodyFileBytes
	maxStreamBodyFileBytes = 500 // test-only small cap; avoids writing a real 1GiB
	defer func() { maxStreamBodyFileBytes = orig }()

	dir := t.TempDir()
	body := sseDataFrames(50, 200) // far larger than the 500-byte test cap
	rc, rec, _, err := runStreamPumpCapture(t, body, "req-backstop", dir)
	if err != nil {
		t.Fatalf("expected nil error (backstop must not break the caller's stream), got %v", err)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("data: [DONE]")) {
		t.Fatalf("caller stream did not complete despite the backstop: %s", rec.Body.String()[:200])
	}
	if !rc.streamBodyTruncated {
		t.Error("expected streamBodyTruncated=true once the test cap was exceeded")
	}
	info, err := os.Stat(filepath.Join(dir, "req-backstop.stream"))
	if err != nil {
		t.Fatalf("stat captured stream file: %v", err)
	}
	if info.Size() > maxStreamBodyFileBytes {
		t.Errorf("captured file grew past the backstop cap: %d bytes, cap %d", info.Size(), maxStreamBodyFileBytes)
	}
}

// TestStreamCaptureVerbatim (Task 5): v0.1 does NOT scrub body content, so an
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
	if !rc.streamBodyCaptured {
		t.Error("expected streamBodyCaptured to be true")
	}
}
