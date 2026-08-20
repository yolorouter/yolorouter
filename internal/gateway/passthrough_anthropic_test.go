package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// This file is the regression coverage for a passthrough egress that does not
// speak OpenAI. Two OpenAI-shaped assumptions are easy to make and wrong here:
// RewriteNonStreamResponse's extractUsage only recognizes
// prompt_tokens/completion_tokens, and OpenAI completion detection only
// recognizes the literal `data: [DONE]` line.
// Neither ever appears on an Anthropic wire response, so a same-protocol
// Claude ingress -> Claude egress passthrough (an Anthropic-type provider,
// createAnthropicProvider from crossproto_test.go, hit via a
// /v1/messages request so ingress==egress==claude and Negotiate reports
// Passthrough=true) silently lost billing (no usage was billed) and, on
// stream, always finalized as "stream_no_done" even for a stream that
// completed cleanly with message_stop. These tests drive that exact
// same-protocol Anthropic passthrough shape and assert both bugs are fixed.

// TestPassthroughAnthropicNonStream_UsageAndModelRewrite is the non-stream
// counterpart. The Claude upstream response is byte-forwarded to the client
// (content/id/stop_reason preserved verbatim) with only the top-level
// "model" field rewritten back to the external name, and the billed usage must be
// populated from the Anthropic input_tokens/output_tokens fields.
func TestPassthroughAnthropicNonStream_UsageAndModelRewrite(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_01abc","type":"message","role":"assistant","content":[{"type":"text","text":"4"}],"model":"claude-3-5-sonnet-real","stop_reason":"end_turn","usage":{"input_tokens":15,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	reqBody := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"messages":[{"role":"user","content":"What is 2+2?"}]}`)
	c, w := newCtxPath("/v1/messages", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// --- client received the byte-forwarded Claude response with the model
	// field rewritten to the external name ---
	var resp struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client response not valid JSON: %v; body=%s", err, w.Body.String())
	}
	if resp.Model != "claude-3-5-sonnet" {
		t.Errorf("client response model = %q, want external name %q (not the provider model name)", resp.Model, "claude-3-5-sonnet")
	}
	if resp.ID != "msg_01abc" || resp.Type != "message" || resp.Role != "assistant" || resp.StopReason != "end_turn" {
		t.Errorf("client response = %+v, want the upstream body byte-forwarded verbatim apart from model", resp)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "4" {
		t.Fatalf("client response content = %+v, want one text block with %q", resp.Content, "4")
	}
	if strings.Contains(w.Body.String(), "claude-3-5-sonnet-real") {
		t.Errorf("provider model name leaked into the client response: %s", w.Body.String())
	}

	// --- the billed usage must be populated from the Anthropic usage object (the
	// bug: RewriteNonStreamResponse's OpenAI-shaped extractUsage never
	// matches input_tokens/output_tokens, so no usage was billed) ---
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	billed := requireBilled(t, captured)
	if billed.Prompt != 15 || billed.Completion != 5 {
		t.Errorf("billed usage = %+v, want prompt=15 completion=5 (mapped from claude input_tokens/output_tokens)", billed)
	}
}

// TestPassthroughAnthropicStream_UsageAndCleanCompletion is the streaming
// counterpart. The Claude upstream sends a normally-completed SSE stream
// (message_start / content_block_delta / message_delta / message_stop, no
// OpenAI [DONE] terminator — Claude has no such convention). A completion
// check that recognized only the literal `data: [DONE]` line would finalize
// this exact fixture as "stream_no_done" with nothing billed despite it
// completing cleanly.
func TestPassthroughAnthropicStream_UsageAndCleanCompletion(t *testing.T) {
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
		write(`data: {"type":"message_start","message":{"id":"msg_01xyz","type":"message","role":"assistant","model":"claude-3-5-sonnet-real","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n")
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
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-real", true, true, 1)
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
		"event: content_block_delta",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream missing byte-forwarded %q: %s", want, body)
		}
	}
	if !strings.Contains(body, `"text":"Hello"`) || !strings.Contains(body, `"text":" world"`) {
		t.Errorf("client stream missing byte-forwarded text deltas: %s", body)
	}

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}

	// --- the billed usage must be populated from the Anthropic
	// input_tokens/output_tokens accumulated across message_start +
	// message_delta ---
	billed := requireBilled(t, captured)
	if billed.Prompt != 10 || billed.Completion != 2 {
		t.Errorf("billed usage = %+v, want prompt=10 completion=2 (mapped from claude input_tokens/output_tokens)", billed)
	}

	// --- the request must NOT be finalized as stream_no_done: exactly one
	// attempt, and it must be AttemptSuccess with no fail_reason ---
	if len(captured.attempts) != 1 {
		t.Fatalf("Attempts = %+v, want exactly 1", captured.attempts)
	}
	if captured.attempts[0].Outcome != AttemptSuccess {
		t.Errorf("attempt outcome = %q, want %q (a clean message_stop completion must not be misreported as stream_no_done)", captured.attempts[0].Outcome, AttemptSuccess)
	}
	if captured.attempts[0].FailReason != "" {
		t.Errorf("attempt fail_reason = %q, want empty", captured.attempts[0].FailReason)
	}
}

// TestPassthroughAnthropicStream_MessageStartModelRewrite is the regression
// coverage for the stream-side model-name leak: OpenAI's per-chunk model
// rewrite only understands that protocol's top-level "model" field,
// so a Claude passthrough stream's message_start frame (which nests the
// model under "message.model") used to be forwarded byte-for-byte, leaking
// the provider's internal model name to the client. This drives that exact
// shape end to end and asserts the client sees the external name instead.
func TestPassthroughAnthropicStream_MessageStartModelRewrite(t *testing.T) {
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
		write(`data: {"type":"message_start","message":{"id":"msg_01xyz","type":"message","role":"assistant","model":"claude-3-5-sonnet-real","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n")
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
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-real", true, true, 1)
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

	// Locate the message_start data line and confirm message.model was
	// rewritten to the external name, not the provider's internal name.
	// Find the data: line carrying message_start by scanning whole lines
	// (not a raw substring search) — map-keyed re-marshaling inside
	// rewriteClaudeMessageStartModel sorts JSON object keys alphabetically,
	// so "type":"message_start" no longer necessarily appears near the start
	// of the line the way the raw upstream fixture wrote it.
	var messageStartLine string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "data: ") && strings.Contains(l, `"type":"message_start"`) {
			messageStartLine = l
			break
		}
	}
	if messageStartLine == "" {
		t.Fatalf("client stream missing a data: line for message_start: %s", body)
	}

	var envelope struct {
		Type    string `json:"type"`
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	payload := strings.TrimPrefix(messageStartLine, "data: ")
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		t.Fatalf("message_start data line not valid JSON: %v; line=%s", err, messageStartLine)
	}
	if envelope.Message.Model != "claude-3-5-sonnet" {
		t.Errorf("client message_start message.model = %q, want external name %q", envelope.Message.Model, "claude-3-5-sonnet")
	}
	if strings.Contains(body, "claude-3-5-sonnet-real") {
		t.Errorf("provider model name leaked into the client stream: %s", body)
	}

	// The rest of the stream (deltas, message_stop) must still be
	// byte-forwarded untouched.
	for _, want := range []string{
		"event: content_block_delta",
		`"text":"hi"`,
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream missing byte-forwarded %q: %s", want, body)
		}
	}

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	billed := requireBilled(t, captured)
	if billed.Prompt != 10 || billed.Completion != 2 {
		t.Errorf("billed usage = %+v, want prompt=10 completion=2", billed)
	}
	if len(captured.attempts) != 1 || captured.attempts[0].Outcome != AttemptSuccess {
		t.Errorf("attempts = %+v, want exactly one AttemptSuccess (model rewrite must not break clean-done classification)", captured.attempts)
	}
	if captured.attempts[0].FailReason != "" {
		t.Errorf("attempt fail_reason = %q, want empty", captured.attempts[0].FailReason)
	}
}

// TestRewriteClaudeMessageStartModel is the focused unit test for the helper
// itself: a message_start line has its nested message.model field rewritten
// while every other field is preserved; any other frame shape is returned
// unchanged with ok=false so the caller forwards it byte-for-byte.
func TestRewriteClaudeMessageStartModel(t *testing.T) {
	t.Run("rewrites message_start model", func(t *testing.T) {
		line := []byte(`data: {"type":"message_start","message":{"id":"msg_01xyz","type":"message","role":"assistant","model":"claude-3-5-sonnet-real","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n")
		newLine, ok := rewriteClaudeMessageStartModel(line, "claude-3-5-sonnet")
		if !ok {
			t.Fatalf("rewriteClaudeMessageStartModel returned ok=false for a valid message_start line: %s", line)
		}
		if !bytes.HasPrefix(newLine, []byte("data: ")) {
			t.Fatalf("rewritten line lost the data: prefix: %s", newLine)
		}
		if !bytes.HasSuffix(newLine, []byte("\n")) {
			t.Fatalf("rewritten line lost its trailing newline: %q", newLine)
		}

		var envelope struct {
			Type    string `json:"type"`
			Message struct {
				ID         string          `json:"id"`
				Type       string          `json:"type"`
				Role       string          `json:"role"`
				Model      string          `json:"model"`
				Content    []any           `json:"content"`
				StopReason json.RawMessage `json:"stop_reason"`
				Usage      struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		payload := bytes.TrimPrefix(bytes.TrimRight(newLine, "\n"), []byte("data: "))
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatalf("rewritten payload not valid JSON: %v; payload=%s", err, payload)
		}
		if envelope.Type != "message_start" {
			t.Errorf("envelope.Type = %q, want message_start (must be preserved)", envelope.Type)
		}
		if envelope.Message.Model != "claude-3-5-sonnet" {
			t.Errorf("message.model = %q, want claude-3-5-sonnet", envelope.Message.Model)
		}
		if envelope.Message.ID != "msg_01xyz" || envelope.Message.Type != "message" || envelope.Message.Role != "assistant" {
			t.Errorf("message identity fields not preserved: %+v", envelope.Message)
		}
		if len(envelope.Message.Content) != 0 {
			t.Errorf("message.content = %+v, want empty (preserved verbatim)", envelope.Message.Content)
		}
		if string(envelope.Message.StopReason) != "null" {
			t.Errorf("message.stop_reason = %s, want null (preserved verbatim)", envelope.Message.StopReason)
		}
		if envelope.Message.Usage.InputTokens != 10 || envelope.Message.Usage.OutputTokens != 1 {
			t.Errorf("message.usage = %+v, want input=10 output=1 (preserved verbatim)", envelope.Message.Usage)
		}
	})

	t.Run("preserves trailing CRLF", func(t *testing.T) {
		line := []byte(`data: {"type":"message_start","message":{"model":"internal-name"}}` + "\r\n")
		newLine, ok := rewriteClaudeMessageStartModel(line, "external-name")
		if !ok {
			t.Fatalf("rewriteClaudeMessageStartModel returned ok=false: %s", line)
		}
		if !bytes.HasSuffix(newLine, []byte("\r\n")) {
			t.Errorf("rewritten line lost the \\r\\n suffix: %q", newLine)
		}
	})

	t.Run("non-message_start data line is returned unchanged", func(t *testing.T) {
		line := []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n")
		newLine, ok := rewriteClaudeMessageStartModel(line, "external-name")
		if ok {
			t.Errorf("rewriteClaudeMessageStartModel returned ok=true for a content_block_delta line: %s", newLine)
		}
		if !bytes.Equal(newLine, line) {
			t.Errorf("returned line = %q, want the original line unchanged", newLine)
		}
	})

	t.Run("non-data line is returned unchanged", func(t *testing.T) {
		line := []byte("event: message_start\n")
		newLine, ok := rewriteClaudeMessageStartModel(line, "external-name")
		if ok {
			t.Errorf("rewriteClaudeMessageStartModel returned ok=true for a non-data line: %s", newLine)
		}
		if !bytes.Equal(newLine, line) {
			t.Errorf("returned line = %q, want the original line unchanged", newLine)
		}
	})

	t.Run("unparseable message_start-ish line is returned unchanged", func(t *testing.T) {
		line := []byte(`data: not json but contains message_start` + "\n")
		newLine, ok := rewriteClaudeMessageStartModel(line, "external-name")
		if ok {
			t.Errorf("rewriteClaudeMessageStartModel returned ok=true for unparseable JSON: %s", newLine)
		}
		if !bytes.Equal(newLine, line) {
			t.Errorf("returned line = %q, want the original line unchanged", newLine)
		}
	})
}
