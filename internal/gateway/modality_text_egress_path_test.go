package gateway

import (
	"context"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// The openai-family passthrough forwards the caller's own path, so a legacy
// /v1/completions caller reaches the upstream's completions endpoint instead
// of being retitled into the chat one. Everything else — chat callers (whose
// path is the canonical one anyway), cross-protocol IR round-trips, and the
// gemini family (whose egress path is model-parameterized, not the caller's)
// keeps the encoder's canonical path.
func TestPassthroughKeepsTheCallersPathInTheUpstreamCall(t *testing.T) {
	admit := func(path, body string) Payload {
		t.Helper()
		payload, rej := NewTextModality().Admit(context.Background(), Ingress{
			Protocol: protocols.ProtocolOpenAI, Path: path, Body: []byte(body),
		})
		if rej != nil {
			t.Fatalf("Admit refused %s: %+v", path, rej)
		}
		return payload
	}

	openaiPassthrough := Candidate{ProviderModelName: "pm", EgressProtocol: protocols.ProtocolOpenAI, Passthrough: true}
	chatBody := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	promptBody := `{"model":"m","prompt":"hi"}`

	for _, tc := range []struct {
		name string
		path string
		body string
		cand Candidate
		want string
	}{
		{"legacy completions keeps its path", "/v1/completions", promptBody, openaiPassthrough, "/v1/completions"},
		{"chat path is already canonical", "/v1/chat/completions", chatBody, openaiPassthrough, "/v1/chat/completions"},
		{"trailing slash trimmed", "/v1/completions/", promptBody, openaiPassthrough, "/v1/completions"},
		{"empty path falls back to canonical", "", chatBody, openaiPassthrough, "/v1/chat/completions"},
		{"cross-protocol IR ignores the caller path", "/v1/completions", promptBody,
			Candidate{ProviderModelName: "pm", EgressProtocol: protocols.ProtocolClaude}, "/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, err := admit(tc.path, tc.body).PrepareUpstream(tc.cand)
			if err != nil {
				t.Fatalf("PrepareUpstream: %v", err)
			}
			if call.Path != tc.want {
				t.Fatalf("Path = %q, want %q", call.Path, tc.want)
			}
		})
	}
}

// A gemini passthrough must NOT take the caller's path: the caller's path
// carries the /v1beta prefix that JoinUpstreamURL re-attaches from the
// provider base, and the encoder's canonical path is model-parameterized.
func TestGeminiPassthroughKeepsTheCanonicalEgressPath(t *testing.T) {
	const callerPath = "/v1beta/models/gemini-2.0-flash:generateContent"
	payload, rej := NewTextModality().Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolGemini, Path: callerPath,
		Body: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	})
	if rej != nil {
		t.Fatalf("Admit refused: %+v", rej)
	}
	call, err := payload.PrepareUpstream(Candidate{
		ProviderModelName: "gemini-2.0-flash", EgressProtocol: protocols.ProtocolGemini, Passthrough: true,
	})
	if err != nil {
		t.Fatalf("PrepareUpstream: %v", err)
	}
	if call.Path != "/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("Path = %q, want the canonical gemini egress path", call.Path)
	}
}
