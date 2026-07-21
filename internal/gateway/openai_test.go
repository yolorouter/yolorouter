package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPeekRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	model, isStream, err := PeekRequest(body)
	if err != nil {
		t.Fatalf("PeekRequest error: %v", err)
	}
	if model != "gpt-4" || !isStream {
		t.Fatalf("got model=%q stream=%v, want gpt-4/true", model, isStream)
	}
}

func TestPeekRequestInvalidJSON(t *testing.T) {
	if _, _, err := PeekRequest([]byte(`{bad json`)); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestValidateRequest(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"valid", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false},
		{"missing messages", `{"model":"m"}`, true},
		{"empty messages", `{"model":"m","messages":[]}`, true},
		{"messages not array", `{"model":"m","messages":"x"}`, true},
		{"messages null", `{"model":"m","messages":null}`, true},
		{"non-function tool rejected", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"code_interpreter"}]}`, true},
		{"function tool ok", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`, false},
		{"unknown fields pass through", `{"model":"m","messages":[{"role":"user","content":"hi"}],"custom_field":42,"top_k":7}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRequest([]byte(tt.body))
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestHasTools(t *testing.T) {
	if HasTools([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)) {
		t.Fatal("expected false with no tools")
	}
	if HasTools([]byte(`{"tools":[]}`)) {
		t.Fatal("expected false with empty tools array")
	}
	if !HasTools([]byte(`{"tools":[{"type":"function","function":{"name":"f"}}]}`)) {
		t.Fatal("expected true with tools")
	}
}

// TestRewriteRequestModelPreservesUnknownFields verifies the model swap is
// surgical: an extended OpenAI param (top_k) the gateway doesn't understand
// must survive into the upstream body, since v0.1 is pure pass-through.
func TestRewriteRequestModelPreservesUnknownFields(t *testing.T) {
	body := []byte(`{"model":"external","messages":[{"role":"user","content":"hi"}],"temperature":0.5,"top_k":7}`)
	out, err := RewriteRequestModel(body, "provider-model")
	if err != nil {
		t.Fatalf("RewriteRequestModel: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if m["model"] != "provider-model" {
		t.Fatalf("model not rewritten: %v", m["model"])
	}
	if m["temperature"] != 0.5 {
		t.Fatalf("temperature not preserved: %v", m["temperature"])
	}
	if m["top_k"] != float64(7) {
		t.Fatalf("top_k not preserved: %v", m["top_k"])
	}
}

func TestRewriteNonStreamResponse(t *testing.T) {
	body := []byte(`{"model":"provider-x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	rewritten, usage, err := RewriteNonStreamResponse(body, "external")
	if err != nil {
		t.Fatalf("RewriteNonStreamResponse: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(rewritten, &m); err != nil {
		t.Fatalf("parse rewritten: %v", err)
	}
	if m["model"] != "external" {
		t.Fatalf("model not rewritten back to external: %v", m["model"])
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage wrong: %+v", usage)
	}
}

func TestExtractUsageMissing(t *testing.T) {
	// GATE-21: a response without a usage object yields nil, NOT a zero
	// Usage — the caller must treat nil as "unknown" so cost is never
	// recorded as 0 for a usage-less request.
	if got := extractUsage([]byte(`{"model":"x","choices":[]}`)); got != nil {
		t.Fatalf("expected nil usage, got %+v", got)
	}
}

func TestExtractUsageMalformed(t *testing.T) {
	if got := extractUsage([]byte(`{bad`)); got != nil {
		t.Fatalf("expected nil for malformed body, got %+v", got)
	}
}

// TestExtractUsageEmptyObjectIsNil: GATE-21 — an empty usage object {} (or a
// partial one missing completion_tokens) has no real counts, so it must NOT
// be treated as known-zero (which would let computeCost record
// cost_known=true cost_micros=0 and show the request as free).
func TestExtractUsageEmptyObjectIsNil(t *testing.T) {
	if got := extractUsage([]byte(`{"model":"x","usage":{}}`)); got != nil {
		t.Fatalf("expected nil for empty usage {}, got %+v", got)
	}
	if got := extractUsage([]byte(`{"model":"x","usage":{"prompt_tokens":5}}`)); got != nil {
		t.Fatalf("expected nil for partial usage missing completion_tokens, got %+v", got)
	}
}

// TestEnsureStreamUsageInjection: PRD §1114 — for a stream request where the
// caller did NOT set stream_options.include_usage, the gateway injects it
// upstream so final usage arrives for cost accounting. Non-stream and
// caller-already-wants-usage bodies are returned unchanged.
func TestEnsureStreamUsageInjection(t *testing.T) {
	// Non-stream: body unchanged.
	out, err := EnsureStreamUsageInjection([]byte(`{"model":"m","stream":false}`), false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != `{"model":"m","stream":false}` {
		t.Errorf("non-stream body should be unchanged: %s", out)
	}
	// Stream + caller already wants usage: body unchanged.
	body := []byte(`{"stream":true,"stream_options":{"include_usage":true}}`)
	out, err = EnsureStreamUsageInjection(body, true, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("body should be unchanged when caller wants usage: %s", out)
	}
	// Stream + caller does NOT want usage: inject stream_options.include_usage=true.
	out, err = EnsureStreamUsageInjection([]byte(`{"model":"m","stream":true,"messages":[]}`), true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	so, ok := m["stream_options"]
	if !ok {
		t.Fatal("stream_options not injected")
	}
	if !bytes.Contains(so, []byte(`"include_usage":true`)) {
		t.Errorf("include_usage=true not injected: %s", so)
	}
}

// TestParseRequestStreamOptions: WantsStreamUsage is set only when the
// caller explicitly sends stream_options.include_usage=true.
func TestParseRequestStreamOptions(t *testing.T) {
	p, err := parseRequest([]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatalf("parseRequest: %v", err)
	}
	if !p.WantsStreamUsage {
		t.Error("expected WantsStreamUsage=true when include_usage=true")
	}
	p, _ = parseRequest([]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":false}}`))
	if p.WantsStreamUsage {
		t.Error("expected WantsStreamUsage=false when include_usage=false")
	}
	p, _ = parseRequest([]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if p.WantsStreamUsage {
		t.Error("expected WantsStreamUsage=false when stream_options absent")
	}
}
