package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// hijackAfterFirstFrame builds an httptest.Server handler that writes and
// flushes one SSE frame, then hijacks the raw connection and closes it
// WITHOUT a clean chunked-transfer terminator. Unlike a handler that simply
// returns (which the http package tears down into a clean end-of-body for
// the client), yanking the raw connection out from under the client mid-body
// surfaces to the reading side as a genuine transport-level read error
// (io.ErrUnexpectedEOF from the chunked decoder, not io.EOF) — modeling a
// real "upstream connection died mid-stream" failure, so the passthrough
// delivery takes the mid-stream error branch (rc.firstByteSent=true,
// err != nil) that calls
// writeStreamErrorEvent, instead of the clean-EOF-without-[DONE] branch
// (errStreamNoDoneTerminator) which deliberately does NOT inject a
// synthetic error event.
func hijackAfterFirstFrame(firstFrame string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(firstFrame))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

// TestRelayStreamMidFailureOpenAIIngress: a same-protocol OpenAI stream that
// dies mid-flight (after the first data frame reached the client) must still
// produce the OpenAI wire shape: an inline `data: {"error":...}` frame
// followed by `data: [DONE]` — the existing behavior, pinned here as a
// regression guard now that writeStreamErrorEvent is ingress-aware.
func TestRelayStreamMidFailureOpenAIIngress(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(hijackAfterFirstFrame(
		"data: {\"model\":\"gpt-4o-real\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
	))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already committed before the mid-stream failure); body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Errorf("expected the OpenAI mid-stream error frame, got %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("OpenAI ingress must still terminate with [DONE], got %q", body)
	}
	if strings.Contains(body, "event: error") {
		t.Errorf("OpenAI ingress must not get the Claude event: error framing, got %q", body)
	}
}

// TestRelayStreamMidFailureClaudeIngress is the Claude-ingress counterpart:
// a /v1/messages request against an anthropic-type provider (same-protocol
// passthrough) whose upstream dies mid-flight must produce the Anthropic
// streaming error shape (event: error + a top-level "type":"error" JSON
// envelope) and must NOT emit the OpenAI [DONE] terminator.
func TestRelayStreamMidFailureClaudeIngress(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(hijackAfterFirstFrame(
		"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
	))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"claude-3-5-sonnet","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
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

// TestRelayStreamMidFailureGeminiIngress is the Gemini-ingress counterpart of
// TestRelayStreamMidFailureClaudeIngress: a native /v1beta/models/...
// :streamGenerateContent request against a provider_type="gemini" provider
// (same-protocol passthrough) whose upstream dies mid-flight must produce
// the Gemini wire shape — a bare `data: {"error":{code,message,status}}`
// frame — and must NOT emit the OpenAI [DONE] terminator (Gemini's SSE
// framing has no such convention at all).
func TestRelayStreamMidFailureGeminiIngress(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(hijackAfterFirstFrame(
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}` + "\n\n",
	))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createGeminiProvider(t, db, "gemini-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-gemini-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gemini-2.0-flash", "gemini-2.0-flash-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	c, w := newCtxPath("/v1beta/models/gemini-2.0-flash:streamGenerateContent", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already committed before the mid-stream failure); body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"error":{"code":500`) {
		t.Errorf("expected the Gemini mid-stream error frame with a nested code, got %q", body)
	}
	if !strings.Contains(body, `"status":"INTERNAL"`) {
		t.Errorf("expected the nested error.status=INTERNAL, got %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Gemini ingress must NOT emit the OpenAI [DONE] terminator, got %q", body)
	}
	if strings.Contains(body, "event: error") {
		t.Errorf("Gemini ingress must not get the Claude event: error framing, got %q", body)
	}
}

// TestRelayStreamMidFailureResponsesIngress is the Responses-ingress
// counterpart: a /v1/responses request against a provider_type="responses"
// provider (same-protocol passthrough) whose upstream dies mid-flight must
// produce the Responses-native mid-stream error frame — a bare
// `data: {"type":"error",...}` event — and must NOT emit the OpenAI [DONE]
// terminator (the Responses SSE stream is framed as typed response.* events,
// never OpenAI Chat's chat.completion.chunk + [DONE]).
func TestRelayStreamMidFailureResponsesIngress(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(hijackAfterFirstFrame(
		"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n",
	))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createResponsesProvider(t, db, "responses-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-responses-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"gpt-4o","stream":true,"input":"hi"}`)
	c, w := newCtxPath("/v1/responses", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already committed before the mid-stream failure); body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("expected the Responses-native mid-stream error event, got %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Responses ingress must NOT emit the OpenAI [DONE] terminator, got %q", body)
	}
	if strings.Contains(body, "event: error") {
		t.Errorf("Responses ingress must not get the Claude event: error framing, got %q", body)
	}
}
