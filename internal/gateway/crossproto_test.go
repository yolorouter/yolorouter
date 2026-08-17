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
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// createAnthropicProvider seeds a provider whose provider_type is
// "anthropic" — the negotiate.go egress switch maps that to
// protocols.ProtocolClaude, so a request against this provider takes the
// cross-protocol IR round trip instead of the same-protocol byte passthrough
// every other gateway test
// exercises. Mirrors createProvider (relay_test.go) with the type field
// overridden.
func createAnthropicProvider(t *testing.T, db *gorm.DB, name, baseURL string) *model.Provider {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{
		Name: name, ProviderType: "anthropic", BaseURL: baseURL,
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed anthropic provider: %v", err)
	}
	return p
}

// createGeminiProvider is createAnthropicProvider's Gemini counterpart:
// provider_type="gemini" maps (providerproto.TypeOf) to
// protocols.ProtocolGemini, so a request against this provider from a native
// Gemini ingress caller (/v1beta/...) is same-protocol passthrough — no IR
// round trip — exercising the decoding pump's Gemini branch and, on a
// mid-stream upstream failure, writeStreamErrorEvent's Gemini branch.
func createGeminiProvider(t *testing.T, db *gorm.DB, name, baseURL string) *model.Provider {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{
		Name: name, ProviderType: "gemini", BaseURL: baseURL,
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed gemini provider: %v", err)
	}
	return p
}

// createResponsesProvider is createGeminiProvider's Responses counterpart:
// provider_type="responses" maps (providerproto.TypeOf) to
// protocols.ProtocolResponses, so a request against this provider from a
// native Responses ingress caller (/v1/responses) is same-protocol
// passthrough — no IR round trip.
func createResponsesProvider(t *testing.T, db *gorm.DB, name, baseURL string) *model.Provider {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{
		Name: name, ProviderType: "responses", BaseURL: baseURL,
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed responses provider: %v", err)
	}
	return p
}

// TestCrossProtocolOpenAIToAnthropicNonStream drives the first
// production-shaped exercise of the cross-protocol IR path: an OpenAI Chat
// Completions request against a provider_type="anthropic" provider. It
// asserts both ends of the round trip — the upstream receives a
// Claude-shaped request (path, auth headers, top-level "system" carrying the
// normalized OpenAI system message, "messages", "max_tokens"), and the
// client receives an OpenAI-shaped response (external model name, usage
// mapped from Claude's input_tokens/output_tokens).
func TestCrossProtocolOpenAIToAnthropicNonStream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var (
		sawPath, sawAPIKeyHeader, sawVersionHeader bool
		sawMessages, sawMaxTokens                  bool
		sawSystem                                  string
		upstreamBody                               []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = strings.HasSuffix(r.URL.Path, "/v1/messages")
		sawAPIKeyHeader = r.Header.Get("x-api-key") == "sk-claude-upstream"
		sawVersionHeader = r.Header.Get("anthropic-version") != ""

		body, _ := io.ReadAll(r.Body)
		upstreamBody = body
		var parsed struct {
			Messages  json.RawMessage `json:"messages"`
			MaxTokens *int            `json:"max_tokens"`
			System    string          `json:"system"`
		}
		_ = json.Unmarshal(body, &parsed)
		sawMessages = len(parsed.Messages) > 0
		sawMaxTokens = parsed.MaxTokens != nil
		sawSystem = parsed.System

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_01abc","type":"message","role":"assistant","content":[{"type":"text","text":"4"}],"model":"claude-3-5-sonnet-20241022","stop_reason":"end_turn","usage":{"input_tokens":15,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"What is 2+2?"}]}`)
	c, w := newCtx(reqBody)
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
	if !sawVersionHeader {
		t.Error("upstream did not receive an anthropic-version header")
	}
	if !sawMessages {
		t.Errorf("upstream body missing non-empty messages: %s", upstreamBody)
	}
	if !sawMaxTokens {
		t.Errorf("upstream body missing max_tokens: %s", upstreamBody)
	}
	if sawSystem != "You are a helpful assistant." {
		t.Errorf("upstream top-level system = %q, want the OpenAI system message normalized into it", sawSystem)
	}

	// --- client received an OpenAI-shaped response ---
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client response not valid JSON: %v; body=%s", err, w.Body.String())
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("client response model = %q, want external name %q (not the claude provider model name)", resp.Model, "gpt-4o")
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "4" {
		t.Fatalf("client response choices = %+v, want one choice with content %q", resp.Choices, "4")
	}
	if resp.Usage.PromptTokens != 15 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 20 {
		t.Errorf("client usage = %+v, want prompt=15 completion=5 total=20 (mapped from claude input_tokens/output_tokens)", resp.Usage)
	}
}

// TestCrossProtocolOpenAIToAnthropicStream is the streaming counterpart:
// the upstream speaks Claude SSE (message_start / content_block_delta /
// message_delta / message_stop), and the client — having sent an OpenAI
// chat request with stream:true — must receive OpenAI-shaped SSE chunks
// (chat.completion.chunk objects, external model name, text deltas
// concatenating to the upstream's text) terminated by "data: [DONE]".
func TestCrossProtocolOpenAIToAnthropicStream(t *testing.T) {
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
		write(`data: {"type":"message_start","message":{"id":"msg_01xyz","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n")
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
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c, w := newCtx(reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("client stream missing OpenAI chat.completion.chunk frames: %s", body)
	}
	if !strings.Contains(body, `"model":"gpt-4o"`) {
		t.Errorf("client stream model not rewritten to the external name: %s", body)
	}
	if strings.Contains(body, "claude-3-5-sonnet") {
		t.Errorf("claude provider model name leaked into the client stream: %s", body)
	}

	// Reconstruct the concatenated text from every SSE data frame's delta.content.
	var text strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("concatenated client stream text = %q, want %q", text.String(), "Hello world")
	}

	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("client stream missing the OpenAI [DONE] terminator: %s", body)
	}
}

// TestCrossProtocolOpenAIToAnthropicStream_IncludeUsage is a regression test
// for the cross-protocol fix: when the OpenAI-ingress caller sets
// stream_options.include_usage=true, the client must receive a final
// usage-bearing chat.completion.chunk before [DONE] — previously the
// cross-protocol chat.StreamEncoder merged DeltaUsage internally but never
// emitted it as an SSE frame, so include_usage callers got no usage on a
// cross-protocol stream (unlike the same-protocol passthrough path).
func TestCrossProtocolOpenAIToAnthropicStream_IncludeUsage(t *testing.T) {
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
		write(`data: {"type":"message_start","message":{"id":"msg_01xyz","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n")
		write("event: content_block_delta\n")
		write(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n")
		write("event: message_delta\n")
		write(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n")
		write("event: message_stop\n")
		write(`data: {"type":"message_stop"}` + "\n\n")
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	c, w := newCtx(reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	doneIdx := strings.Index(body, "data: [DONE]")
	if doneIdx < 0 {
		t.Fatalf("client stream missing the OpenAI [DONE] terminator: %s", body)
	}

	var sawUsageChunk bool
	for _, line := range strings.Split(body[:doneIdx], "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var chunk struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || chunk.Usage == nil {
			continue
		}
		sawUsageChunk = true
		if len(chunk.Choices) != 0 {
			t.Errorf("usage chunk choices = %+v, want empty (matching OpenAI's own final usage frame)", chunk.Choices)
		}
		if chunk.Usage.PromptTokens != 10 || chunk.Usage.CompletionTokens != 2 || chunk.Usage.TotalTokens != 12 {
			t.Errorf("usage chunk = %+v, want prompt=10 completion=2 total=12 (mapped from claude input_tokens/output_tokens)", chunk.Usage)
		}
	}
	if !sawUsageChunk {
		t.Errorf("client stream with stream_options.include_usage=true missing a usage chunk before [DONE]: %s", body)
	}
}

// TestCrossProtocolOpenAIToAnthropicNonStream_CapturesResponseBody is a
// regression test for the cross-protocol fix: on a successful
// cross-protocol non-stream
// request, IRNonStreamRelay wrote the encoded client response directly and
// only the raw upstream body was recorded, leaving rc.ResponseBody() (the
// caller-facing audit body persisted to request_log_bodies.response_body)
// empty — inconsistent with the same-protocol passthrough path, which always
// populates both. After the fix, rc.ResponseBody() must be non-empty and equal
// to the OpenAI-encoded bytes actually written to the client.
func TestCrossProtocolOpenAIToAnthropicNonStream_CapturesResponseBody(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_01abc","type":"message","role":"assistant","content":[{"type":"text","text":"4"}],"model":"claude-3-5-sonnet-20241022","stop_reason":"end_turn","usage":{"input_tokens":15,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"What is 2+2?"}]}`)
	c, w := newCtx(reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if len(captured.ResponseBody()) == 0 {
		t.Fatal("rc.ResponseBody() is empty; the cross-protocol non-stream path must capture the caller-facing encoded response")
	}
	if !bytes.Equal(captured.ResponseBody(), w.Body.Bytes()) {
		t.Errorf("ResponseBody() = %s, want it to equal the bytes actually written to the client: %s", captured.ResponseBody(), w.Body.Bytes())
	}
	if !bytes.Contains(captured.ResponseBody(), []byte(`"model":"gpt-4o"`)) {
		t.Errorf("ResponseBody() = %s, want the OpenAI-encoded client body carrying the external model name", captured.ResponseBody())
	}
}

// TestCrossProtocolStreamPreFirstEventFailover is a regression test for the
// fix that lets a cross-protocol stream fail over to the next candidate when
// the first candidate's upstream never emits a single event. Before the fix,
// protocols.IRStreamRelay committed the SSE response headers (200 +
// text/event-stream) unconditionally before reading any upstream bytes, and
// the dispatch layer called rc.markFirstByteSent() before invoking the relay at
// all — so a pre-first-event failure (here, an upstream that returns 200
// with a completely empty body) was indistinguishable from a genuine
// mid-stream failure and could never fail over, unlike the same-protocol
// passthrough path. The first candidate's upstream returns 200 with no SSE
// events at all; the gateway must fail over to the second, healthy
// candidate and the client must receive that candidate's content.
func TestCrossProtocolStreamPreFirstEventFailover(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	emptyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Deliberately write nothing: a 200 response with zero SSE events,
		// then the connection closes (clean EOF).
	}))
	defer emptyUpstream.Close()

	goodUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		write(`data: {"type":"message_start","message":{"id":"msg_02xyz","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n")
		write("event: content_block_delta\n")
		write(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n")
		write("event: message_delta\n")
		write(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n")
		write("event: message_stop\n")
		write(`data: {"type":"message_stop"}` + "\n\n")
	}))
	defer goodUpstream.Close()

	svc := newSvc(t, db)
	p1 := createAnthropicProvider(t, db, "claude-empty", emptyUpstream.URL)
	createProviderKey(t, db, svc.masterKey, p1.ID, "sk-claude-empty", "k1", 1, true)
	p2 := createAnthropicProvider(t, db, "claude-good", goodUpstream.URL)
	createProviderKey(t, db, svc.masterKey, p2.ID, "sk-claude-good", "k1", 1, true)

	// Both candidates back the same external model, in sort_order so the
	// empty candidate is tried first.
	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	for i, p := range []*model.Provider{p1, p2} {
		if err := db.Create(&model.ModelCandidate{
			ModelID: m.ID, ProviderID: p.ID, ProviderModelName: "claude-3-5-sonnet-20241022",
			InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
			SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
			ManagementStatus:   model.ModelCandidateStatusEnabled,
			SortOrder:          i + 1,
			VerificationStatus: model.ModelVerificationStatusPassed,
			CreatedAt:          now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatalf("seed candidate %d: %v", i, err)
		}
	}
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	reqBody := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c, w := newCtx(reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover to the second candidate; body = %s", w.Code, w.Body.String())
	}

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if len(captured.attempts) != 2 {
		t.Fatalf("Attempts = %+v, want exactly 2 (the empty first candidate, then the successful second one)", captured.attempts)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("client stream missing OpenAI chat.completion.chunk frames after failover: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("client stream missing the OpenAI [DONE] terminator after failover: %s", body)
	}

	// Reconstruct the concatenated text to confirm it's the SECOND
	// (healthy) candidate's content that actually reached the client.
	var text strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	if text.String() != "ok" {
		t.Errorf("concatenated client stream text = %q, want %q (from the second, healthy candidate)", text.String(), "ok")
	}
}

// TestCrossProtocolStreamCaptureMatchesClientBytes is the streaming
// counterpart to TestCrossProtocolOpenAIToAnthropicNonStream_CapturesResponseBody:
// after a successful cross-protocol IR stream (OpenAI ingress, Claude
// upstream — same fixture as TestCrossProtocolOpenAIToAnthropicStream), the
// per-request <request_id>.stream capture file must be byte-for-byte equal
// to what the client actually received, not merely "contain" the expected
// content. Before the capture-routing fix, IRStreamRelay recorded the RAW
// upstream lines (Claude SSE: message_start/content_block_delta/... shaped)
// instead of the caller-facing (post-re-encode) OpenAI
// chat.completion.chunk bytes actually written to c.Writer — the two shapes
// differ completely, so this regression test fails without the fix.
func TestCrossProtocolStreamCaptureMatchesClientBytes(t *testing.T) {
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
		write(`data: {"type":"message_start","message":{"id":"msg_01xyz","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n")
		write("event: content_block_delta\n")
		write(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n")
		write("event: content_block_delta\n")
		write(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}` + "\n\n")
		write("event: message_delta\n")
		write(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n")
		write("event: message_stop\n")
		write(`data: {"type":"message_stop"}` + "\n\n")
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	dir := t.TempDir()
	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	reqBody := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c, w := newCtx(reqBody)
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
}
