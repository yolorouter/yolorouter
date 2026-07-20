package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	wrote, usage, done := writeStreamLine(&buf, []byte(`data: {"model":"p","choices":[]}`+"\n"), "ext", true)
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
	wrote, _, done := writeStreamLine(&buf, []byte("data: [DONE]\n"), "ext", true)
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
	wrote, _, done := writeStreamLine(&buf, []byte(": keepalive\n"), "ext", true)
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
