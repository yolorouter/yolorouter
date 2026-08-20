package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestResponsesIngressToAnthropicUpstream_NonStream is an end-to-end proof
// that a native Responses ingress request (/v1/responses) routed to an
// anthropic-type provider takes the cross-protocol IR round trip: the
// upstream must see a Claude Messages request (path, x-api-key auth), and
// the caller must receive a Responses-shaped response (object=response,
// output[].content[].text, usage.input_tokens/output_tokens), with the
// external model name and never the upstream's own claude model name.
func TestResponsesIngressToAnthropicUpstream_NonStream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var sawPath, sawAPIKeyHeader bool
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = strings.HasSuffix(r.URL.Path, "/v1/messages")
		sawAPIKeyHeader = r.Header.Get("x-api-key") == "sk-claude-upstream"
		upstreamBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_01abc","type":"message","role":"assistant","content":[{"type":"text","text":"hello from claude"}],"model":"claude-3-5-sonnet-20241022","stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":6}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"gpt-4o","input":"hi"}`)
	c, w := newCtxPath("/v1/responses", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// --- upstream received a Claude-shaped request ---
	if !sawPath {
		t.Error("upstream did not receive a request ending in /v1/messages")
	}
	if !sawAPIKeyHeader {
		t.Error("upstream did not receive x-api-key header carrying the provider key")
	}
	if !strings.Contains(string(upstreamBody), `"messages"`) {
		t.Errorf("upstream body missing Claude-shaped messages field: %s", upstreamBody)
	}

	// --- client received a Responses-shaped response ---
	var resp struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client response not valid Responses JSON: %v; body=%s", err, w.Body.String())
	}
	if resp.Object != "response" {
		t.Errorf("client response object = %q, want %q", resp.Object, "response")
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("client response model = %q, want external name %q (not the claude provider model name)", resp.Model, "gpt-4o")
	}
	var text string
	for _, item := range resp.Output {
		if item.Type == "message" {
			for _, c := range item.Content {
				text += c.Text
			}
		}
	}
	if text != "hello from claude" {
		t.Errorf("client response output text = %q, want %q", text, "hello from claude")
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 6 || resp.Usage.TotalTokens != 18 {
		t.Errorf("client usage = %+v, want input=12 output=6 total=18 (mapped from claude input_tokens/output_tokens)", resp.Usage)
	}
	if strings.Contains(w.Body.String(), "claude-3-5-sonnet-20241022") {
		t.Errorf("claude provider model name leaked into the client response: %s", w.Body.String())
	}
}

// TestResponsesIngressToAnthropicUpstream_Stream is the streaming
// counterpart: the upstream speaks Claude SSE (message_start/
// content_block_delta/message_delta/message_stop), and the client — having
// sent a Responses request with stream:true — must receive Responses SSE
// events (typed response.* frames, no OpenAI [DONE] terminator, no Claude
// "event: " framing), with the concatenated output text and final usage
// correct, and the claude provider model name must not leak.
func TestResponsesIngressToAnthropicUpstream_Stream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if flusher != nil {
				flusher.Flush()
			}
		}
		write("event: message_start\n")
		write(`data: {"type":"message_start","message":{"id":"msg_01xyz","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":9,"output_tokens":0}}}` + "\n\n")
		write("event: content_block_start\n")
		write(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
		write("event: content_block_delta\n")
		write(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n")
		write("event: content_block_delta\n")
		write(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}` + "\n\n")
		write("event: content_block_stop\n")
		write(`data: {"type":"content_block_stop","index":0}` + "\n\n")
		write("event: message_delta\n")
		write(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n")
		write("event: message_stop\n")
		write(`data: {"type":"message_stop"}` + "\n\n")
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"gpt-4o","input":"hi","stream":true}`)
	c, w := newCtxPath("/v1/responses", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Responses ingress must not emit the OpenAI [DONE] terminator: %s", body)
	}
	if strings.Contains(body, "event: content_block_delta") {
		t.Errorf("Responses ingress must not get the Claude event: framing: %s", body)
	}
	if strings.Contains(body, "claude-3-5-sonnet-20241022") {
		t.Errorf("claude provider model name leaked into the client stream: %s", body)
	}

	var text strings.Builder
	var sawCompleted bool
	var inputTokens, outputTokens, totalTokens int
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var evt struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response *struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(data), &evt) != nil {
			continue
		}
		if evt.Type == "response.output_text.delta" {
			text.WriteString(evt.Delta)
		}
		if evt.Type == "response.completed" && evt.Response != nil {
			sawCompleted = true
			inputTokens = evt.Response.Usage.InputTokens
			outputTokens = evt.Response.Usage.OutputTokens
			totalTokens = evt.Response.Usage.TotalTokens
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("concatenated client stream text = %q, want %q", text.String(), "Hello world")
	}
	if !sawCompleted {
		t.Fatalf("client stream missing a response.completed event: %s", body)
	}
	if inputTokens != 9 || outputTokens != 2 || totalTokens != 11 {
		t.Errorf("response.completed usage = input=%d output=%d total=%d, want input=9 output=2 total=11", inputTokens, outputTokens, totalTokens)
	}
}

// TestResponsesIngressToResponsesProvider_Passthrough confirms a native
// Responses ingress request against a provider_type="responses" provider is
// forwarded as same-protocol byte passthrough (no IR round trip): the
// upstream must receive the caller's own Responses-shaped body (input
// intact) with a Bearer-authenticated /v1/responses request, and the client
// must receive the upstream's Responses output/usage verbatim with only the
// model field rewritten back to the external name.
func TestResponsesIngressToResponsesProvider_Passthrough(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var sawPath, sawAuth, sawInput bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = strings.HasSuffix(r.URL.Path, "/v1/responses")
		sawAuth = r.Header.Get("Authorization") == "Bearer sk-responses-upstream"
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Input json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(body, &parsed)
		sawInput = len(parsed.Input) > 0

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","model":"gpt-4o-real","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"responses passthrough ok"}],"status":"completed"}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createResponsesProvider(t, db, "responses-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-responses-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"gpt-4o","input":"hi"}`)
	c, w := newCtxPath("/v1/responses", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// --- upstream received the native Responses request ---
	if !sawPath {
		t.Error("upstream did not receive a request ending in /v1/responses")
	}
	if !sawAuth {
		t.Error("upstream did not receive Authorization: Bearer sk-responses-upstream")
	}
	if !sawInput {
		t.Error("upstream body missing non-empty input (caller body was not forwarded verbatim)")
	}

	// --- client received the upstream's Responses response, model rewritten ---
	var resp struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client response not valid Responses JSON: %v; body=%s", err, w.Body.String())
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("client response model = %q, want external name %q (passthrough must still rewrite the model field)", resp.Model, "gpt-4o")
	}
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "responses passthrough ok" {
		t.Fatalf("client output = %+v, want one message with text %q", resp.Output, "responses passthrough ok")
	}
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 3 || resp.Usage.TotalTokens != 8 {
		t.Errorf("client usage = %+v, want input=5 output=3 total=8 (passthrough, unchanged)", resp.Usage)
	}
	if strings.Contains(w.Body.String(), "gpt-4o-real") {
		t.Errorf("provider model name leaked into the client response: %s", w.Body.String())
	}
}

// TestResponsesIngressToResponsesProvider_PassthroughStream is the streaming
// counterpart of TestResponsesIngressToResponsesProvider_Passthrough: a
// native Responses streaming request against a provider_type="responses"
// provider is forwarded as same-protocol byte passthrough, and the
// "response.model" field nested in the response.created/response.completed
// envelope events — which the upstream stamps with its own
// provider_model_name — must be rewritten to the external name before it
// reaches the client or the per-request stream capture file.
func TestResponsesIngressToResponsesProvider_PassthroughStream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(`data: {"type":"response.created","response":{"id":"resp_1","object":"response","model":"gpt-4o-real","status":"in_progress","output":[]}}` + "\n\n")
		write(`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}` + "\n\n")
		write(`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" world"}` + "\n\n")
		write(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-4o-real","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello world"}],"status":"completed"}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}` + "\n\n")
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createResponsesProvider(t, db, "responses-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-responses-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	dir := t.TempDir()
	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	reqBody := []byte(`{"model":"gpt-4o","input":"hi","stream":true}`)
	c, w := newCtxPath("/v1/responses", reqBody)
	c.Set(BodiesDirContextKey, dir)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if strings.Contains(body, "gpt-4o-real") {
		t.Errorf("provider model name leaked into the client stream: %s", body)
	}
	if !strings.Contains(body, `"model":"gpt-4o"`) {
		t.Errorf("client stream missing response.model rewritten to the external name: %s", body)
	}

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	capturedBytes, err := os.ReadFile(filepath.Join(dir, captured.requestID+".stream"))
	if err != nil {
		t.Fatalf("read captured stream file: %v", err)
	}
	if strings.Contains(string(capturedBytes), "gpt-4o-real") {
		t.Errorf("provider model name leaked into the stream capture file: %s", capturedBytes)
	}
	if !bytes.Equal(capturedBytes, w.Body.Bytes()) {
		t.Errorf("captured stream file is not byte-for-byte equal to the client bytes.\ncaptured=%q\nclient=  %q", capturedBytes, w.Body.Bytes())
	}
}
