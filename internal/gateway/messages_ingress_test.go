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

// This file is the end-to-end proof of the Anthropic-ingress deliverable:
// a Claude-format client hitting POST /v1/messages, routed through the
// cross-protocol IR path to an OpenAI-compatible upstream (provider_type
// "openai"), getting Claude-native responses and errors back. It mirrors
// crossproto_test.go's harness (createProvider/createModelAndCandidate/
// createAPIKey/newCtxPath/svc.Handle) with ingress and egress swapped:
// crossproto_test.go drives an OpenAI-ingress client against an
// anthropic-type provider; these tests drive a Claude-ingress client (path
// /v1/messages) against an openai-type provider — the other direction of the
// same IR round trip, and the one that matches how a real Claude Code / Claude
// SDK caller reaches an OpenAI-compatible upstream through this gateway.

// TestMessagesIngressNonStreamSuccess covers the non-stream success path. It
// also stands in for "valid X-Api-Key succeeds": svc.Handle receives exactly
// the *model.APIKey middleware.APIKeyAuth would have resolved from a caller's
// X-Api-Key header (see internal/middleware/api_key_auth.go's resolveAPIKey)
// — Handle itself does not re-read auth headers, so driving it with an
// already-resolved key IS the post-auth behavior a valid X-Api-Key produces.
func TestMessagesIngressNonStreamSuccess(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var (
		sawPath, sawAuthHeader, sawSystemMessage bool
		sawUpstreamModel                         string
		upstreamBody                             []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = strings.HasSuffix(r.URL.Path, "/v1/chat/completions")
		sawAuthHeader = strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")

		body, _ := io.ReadAll(r.Body)
		upstreamBody = body
		var parsed struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &parsed)
		sawUpstreamModel = parsed.Model
		for _, m := range parsed.Messages {
			if m.Role == "system" && m.Content == "You are a helpful assistant." {
				sawSystemMessage = true
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":5,"total_tokens":20}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "openai-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-openai-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"system":"You are a helpful assistant.","messages":[{"role":"user","content":"What is 2+2?"}]}`)
	c, w := newCtxPath("/v1/messages", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// --- upstream received an OpenAI Chat-shaped request ---
	if !sawPath {
		t.Error("upstream did not receive a request ending in /v1/chat/completions")
	}
	if !sawAuthHeader {
		t.Error("upstream did not receive an Authorization: Bearer header")
	}
	if sawUpstreamModel != "gpt-4o-real" {
		t.Errorf("upstream body model = %q, want the provider model name %q", sawUpstreamModel, "gpt-4o-real")
	}
	if !sawSystemMessage {
		t.Errorf("upstream body = %s, want a role:system message carrying the Claude top-level system field", upstreamBody)
	}

	// --- client received a Claude Messages-shaped response ---
	var resp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client response not valid JSON: %v; body=%s", err, w.Body.String())
	}
	if resp.Type != "message" {
		t.Errorf(`top-level "type" = %q, want "message"`, resp.Type)
	}
	if resp.Role != "assistant" {
		t.Errorf(`"role" = %q, want "assistant"`, resp.Role)
	}
	if resp.Model != "claude-3-5-sonnet" {
		t.Errorf("client response model = %q, want the external name %q (not the openai provider model name)", resp.Model, "claude-3-5-sonnet")
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "4" {
		t.Fatalf("client response content = %+v, want one text block with %q", resp.Content, "4")
	}
	if resp.Usage.InputTokens != 15 || resp.Usage.OutputTokens != 5 {
		t.Errorf("client usage = %+v, want input_tokens=15 output_tokens=5 (mapped from openai prompt/completion tokens)", resp.Usage)
	}
}

// TestMessagesIngressStreamSuccess covers the streaming success path: the
// caller sends a Claude request with stream:true, the upstream speaks OpenAI
// SSE (chat.completion.chunk shaped, split finish_reason/usage chunks — a
// common real-world provider shape), and the client must receive Claude SSE
// (message_start / content_block_start / content_block_delta / message_delta
// / message_stop) with no OpenAI [DONE] terminator anywhere.
func TestMessagesIngressStreamSuccess(t *testing.T) {
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
		write(`data: {"id":"chatcmpl-2","model":"gpt-4o-real","choices":[{"delta":{"role":"assistant","content":"Hello"}}]}` + "\n\n")
		write(`data: {"id":"chatcmpl-2","choices":[{"delta":{"content":" world"}}]}` + "\n\n")
		write(`data: {"id":"chatcmpl-2","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n")
		write(`data: {"id":"chatcmpl-2","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\n")
		write("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "openai-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-openai-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	reqBody := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c, w := newCtxPath("/v1/messages", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	for _, want := range []string{
		`"type":"message_start"`,
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Claude ingress must NOT forward the OpenAI [DONE] terminator: %s", body)
	}
	if strings.Contains(body, "gpt-4o-real") {
		t.Errorf("openai provider model name leaked into the client stream: %s", body)
	}

	// Reconstruct the concatenated text from every content_block_delta's
	// text_delta.text — proving the two OpenAI content deltas were correctly
	// decoded and re-encoded as Claude text deltas, not dropped or merged.
	var text strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var evt struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &evt) != nil {
			continue
		}
		if evt.Type == "content_block_delta" && evt.Delta.Type == "text_delta" {
			text.WriteString(evt.Delta.Text)
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("concatenated client stream text = %q, want %q", text.String(), "Hello world")
	}

	// The Claude wire format only carries output_tokens in message_delta
	// (input_tokens is fixed at message_start time, before the upstream's
	// late-arriving usage frame is even decoded) — so the caller-visible
	// message_delta.usage.output_tokens is the one wire field this fixture
	// can assert on directly.
	if !strings.Contains(body, `"output_tokens":2`) {
		t.Errorf("client stream message_delta missing mapped output_tokens=2: %s", body)
	}
	// The full prompt+completion usage (including the late-arriving OpenAI
	// prompt_tokens the Claude wire format has no slot for after
	// message_start) must still land correctly in the billing/audit record.
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if billed := requireBilled(t, captured); billed.Prompt != 10 || billed.Completion != 2 {
		t.Errorf("billed usage = %+v, want prompt=10 completion=2 (mapped from openai prompt_tokens/completion_tokens)", billed)
	}
}

// TestMessagesIngressAuth_MissingKeyEnvelope confirms the shape of the 401 a
// caller without an X-Api-Key header receives on /v1/messages: top-level
// type:"error", top-level request_id, nested error.type:"authentication_error"
// — exactly what middleware.APIKeyAuth sends (internal/middleware/api_key_auth.go's
// missing-raw-key branch calls gateway.WriteIngressError with these same
// arguments). This package cannot import internal/middleware directly to
// drive that call site itself: middleware already imports internal/gateway,
// so a gateway test importing middleware back would be an import cycle. The
// real X-Api-Key header parsing (Bearer vs X-Api-Key precedence, conflict
// detection, DB lookup) is exercised end-to-end against the production
// middleware in internal/middleware/api_key_auth_test.go's
// TestAPIKeyAuth_MissingKey_IngressAwareEnvelope and
// internal/middleware/api_key_auth_test.go's TestAPIKeyAuth_XAPIKeyHeader (valid
// key), plus internal/router/router_test.go's
// TestMessagesRouteReachesGatewayWithValidKey (valid key reaches the real
// registered route). This test pins the exact envelope contract those call
// sites depend on.
func TestMessagesIngressAuth_MissingKeyEnvelope(t *testing.T) {
	c, w := newCtxPath("/v1/messages", nil)

	WriteIngressError(c, IngressProtocol(c.Request.URL.Path), http.StatusUnauthorized, errTypeAuthentication, "missing API key", "req_missing_key")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body did not unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if body["type"] != "error" {
		t.Errorf(`top-level "type" = %v, want "error"`, body["type"])
	}
	if body["request_id"] != "req_missing_key" {
		t.Errorf(`top-level "request_id" = %v, want "req_missing_key"`, body["request_id"])
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf(`"error" field is not an object: %v`, body["error"])
	}
	if errObj["type"] != "authentication_error" {
		t.Errorf("error.type = %v, want authentication_error", errObj["type"])
	}
}

// TestMessagesIngressMalformedBodyRejected covers three malformed-body
// shapes: a message content that is an object (neither a string nor a
// content-block array), a missing messages array, and max_tokens<=0. All
// three must be rejected as a 400 Claude error envelope BEFORE any
// candidate/upstream is ever tried.
func TestMessagesIngressMalformedBodyRejected(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "object_content",
			body: []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"messages":[{"role":"user","content":{"foo":"bar"}}]}`),
		},
		{
			name: "max_tokens_zero",
			body: []byte(`{"model":"claude-3-5-sonnet","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`),
		},
		{
			name: "missing_messages",
			body: []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			upstreamHit := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHit = true
			}))
			defer upstream.Close()

			svc := newSvc(t, db)
			p := createProvider(t, db, "openai-provider", upstream.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-openai-upstream", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "gpt-4o-real", true, true, 1)
			apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

			c, w := newCtxPath("/v1/messages", tc.body)
			svc.Handle(c, apiKey)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if upstreamHit {
				t.Error("upstream must not be called for a malformed Claude request body")
			}

			var respBody map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &respBody); err != nil {
				t.Fatalf("response body did not unmarshal: %v (body=%s)", err, w.Body.String())
			}
			if respBody["type"] != "error" {
				t.Errorf(`top-level "type" = %v, want "error" (Anthropic-native envelope)`, respBody["type"])
			}
			if _, hasRequestID := respBody["request_id"]; !hasRequestID {
				t.Errorf("response body missing top-level request_id: %v", respBody)
			}
			errObj, ok := respBody["error"].(map[string]any)
			if !ok {
				t.Fatalf(`"error" field is not an object: %v`, respBody["error"])
			}
			if errObj["type"] != "invalid_request_error" {
				t.Errorf("error.type = %v, want invalid_request_error", errObj["type"])
			}
		})
	}
}

// TestMessagesIngressMidStreamFailure covers the cross-protocol counterpart
// of stream_error_ingress_test.go's TestRelayStreamMidFailureClaudeIngress:
// there, the Claude-ingress caller hits an anthropic-type provider
// (same-protocol passthrough). Here it hits an openai-type provider (the
// cross-protocol IR path — protocols.IRStreamRelay, not the byte
// passthrough pump). The upstream
// sends one OpenAI SSE frame (enough to reach the first Claude-encoded event,
// committing the response) then dies via a raw connection hijack/close
// (hijackAfterFirstFrame, defined in stream_error_ingress_test.go) — a
// genuine transport-level read error, not a clean EOF. The client must still
// receive the Anthropic streaming error shape and no OpenAI [DONE].
func TestMessagesIngressMidStreamFailure(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(hijackAfterFirstFrame(
		`data: {"id":"chatcmpl-3","model":"gpt-4o-real","choices":[{"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n",
	))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "openai-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-openai-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c, w := newCtxPath("/v1/messages", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already committed before the mid-stream failure); body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("expected a Claude event: error frame, got %q", body)
	}
	if !strings.Contains(body, `"type":"api_error"`) {
		t.Errorf("expected the nested error.type=api_error, got %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Claude ingress must NOT emit the OpenAI [DONE] terminator, got %q", body)
	}
}

// TestMessagesIngressStreamAuditMatchesClientBytes is the cross-protocol
// counterpart to crossproto_test.go's
// TestCrossProtocolStreamCaptureMatchesClientBytes (which pins the OpenAI
// ingress -> Claude egress direction): after a successful Claude-ingress ->
// OpenAI-egress stream, the per-request <request_id>.stream capture file must
// be byte-for-byte equal to what the client actually received (the
// Claude-encoded SSE), not the raw OpenAI upstream lines and not merely
// "contain" the expected content.
func TestMessagesIngressStreamAuditMatchesClientBytes(t *testing.T) {
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
		write(`data: {"id":"chatcmpl-4","model":"gpt-4o-real","choices":[{"delta":{"role":"assistant","content":"Hello"}}]}` + "\n\n")
		write(`data: {"id":"chatcmpl-4","choices":[{"delta":{"content":" world"},"finish_reason":"stop"}]}` + "\n\n")
		write(`data: {"id":"chatcmpl-4","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\n")
		write("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "openai-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-openai-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	dir := t.TempDir()
	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	reqBody := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c, w := newCtxPath("/v1/messages", reqBody)
	c.Set(BodiesDirContextKey, dir)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if captured.requestID == "" {
		t.Fatal("expected a non-empty request id")
	}

	capturedBytes, err := os.ReadFile(filepath.Join(dir, captured.requestID+".stream"))
	if err != nil {
		t.Fatalf("read captured stream file: %v", err)
	}
	if !bytes.Equal(capturedBytes, w.Body.Bytes()) {
		t.Errorf("captured stream file is not byte-for-byte equal to the client bytes.\ncaptured=%q\nclient=  %q", capturedBytes, w.Body.Bytes())
	}
	// Sanity: make sure this isn't a trivial empty==empty pass.
	if !bytes.Contains(capturedBytes, []byte("message_start")) || !bytes.Contains(capturedBytes, []byte("message_stop")) {
		t.Errorf("captured stream file missing expected Claude SSE events: %s", capturedBytes)
	}
}
