package gateway

import (
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

func TestIngressProtocol_ChatCompletions(t *testing.T) {
	got := IngressProtocol("/v1/chat/completions")
	if got != protocols.ProtocolOpenAI {
		t.Fatalf("IngressProtocol(/v1/chat/completions) = %v, want %v", got, protocols.ProtocolOpenAI)
	}
}

func TestIngressProtocol_UnknownPathFallsBackToOpenAI(t *testing.T) {
	got := IngressProtocol("/v1/unknown")
	if got != protocols.ProtocolOpenAI {
		t.Fatalf("IngressProtocol(/v1/unknown) = %v, want %v", got, protocols.ProtocolOpenAI)
	}
}

func TestIngressProtocol_Responses(t *testing.T) {
	got := IngressProtocol("/v1/responses")
	if got != protocols.ProtocolResponses {
		t.Fatalf("IngressProtocol(/v1/responses) = %v, want %v", got, protocols.ProtocolResponses)
	}
}

func TestIngressProtocol_Messages(t *testing.T) {
	got := IngressProtocol("/v1/messages")
	if got != protocols.ProtocolClaude {
		t.Fatalf("IngressProtocol(/v1/messages) = %v, want %v", got, protocols.ProtocolClaude)
	}
}

func TestIngressProtocolForRequest(t *testing.T) {
	cases := []struct {
		name                string
		path                string
		hasAnthropicVersion bool
		want                protocols.ProtocolID
	}{
		{"models list, anthropic caller", "/v1/models", true, protocols.ProtocolClaude},
		{"models list, openai caller", "/v1/models", false, protocols.ProtocolOpenAI},
		{"models retrieve, anthropic caller", "/v1/models/gpt-4o", true, protocols.ProtocolClaude},
		{"models retrieve, openai caller", "/v1/models/gpt-4o", false, protocols.ProtocolOpenAI},
		// Non-discovery paths ignore the header entirely.
		{"chat completions ignores header", "/v1/chat/completions", true, protocols.ProtocolOpenAI},
		{"messages stays claude by path", "/v1/messages", false, protocols.ProtocolClaude},
		{"messages stays claude even with header", "/v1/messages", true, protocols.ProtocolClaude},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IngressProtocolForRequest(tc.path, tc.hasAnthropicVersion); got != tc.want {
				t.Fatalf("IngressProtocolForRequest(%q, %v) = %v, want %v", tc.path, tc.hasAnthropicVersion, got, tc.want)
			}
		})
	}
}

func TestIngressProtocol_GeminiGenerateContent(t *testing.T) {
	got := IngressProtocol("/v1beta/models/gemini-2.0-flash:generateContent")
	if got != protocols.ProtocolGemini {
		t.Fatalf("IngressProtocol(gemini generateContent) = %v, want %v", got, protocols.ProtocolGemini)
	}
}

func TestIngressProtocol_GeminiStreamGenerateContent(t *testing.T) {
	got := IngressProtocol("/v1beta/models/gemini-1.5-pro:streamGenerateContent")
	if got != protocols.ProtocolGemini {
		t.Fatalf("IngressProtocol(gemini streamGenerateContent) = %v, want %v", got, protocols.ProtocolGemini)
	}
}

// TestIngressProtocol_GeminiPrefixWithoutRecognizedActionFallsBackToOpenAI
// guards that the Gemini match requires one of the two known action
// suffixes -- an unrelated action under the same /v1beta/models/ prefix
// must not be misclassified as Gemini.
func TestIngressProtocol_GeminiPrefixWithoutRecognizedActionFallsBackToOpenAI(t *testing.T) {
	got := IngressProtocol("/v1beta/models/gemini-2.0-flash:countTokens")
	if got != protocols.ProtocolOpenAI {
		t.Fatalf("IngressProtocol(gemini unrecognized action) = %v, want %v", got, protocols.ProtocolOpenAI)
	}
}

func TestNegotiate_OpenAIIngressOnOpenAIProvider_Passthrough(t *testing.T) {
	p := &Provider{ProviderType: "openai", BaseURL: "https://api.openai.com"}

	decision, err := Negotiate(protocols.ProtocolOpenAI, p)
	if err != nil {
		t.Fatalf("Negotiate returned error: %v", err)
	}
	if decision.Protocol != protocols.ProtocolOpenAI {
		t.Errorf("Protocol = %v, want %v", decision.Protocol, protocols.ProtocolOpenAI)
	}
	if decision.BaseURL != p.BaseURL {
		t.Errorf("BaseURL = %q, want %q", decision.BaseURL, p.BaseURL)
	}
}

func TestNegotiate_OpenAIIngressOnAnthropicProvider_FallsBackToPrimary(t *testing.T) {
	p := &Provider{ProviderType: "anthropic", BaseURL: "https://api.anthropic.com"}

	decision, err := Negotiate(protocols.ProtocolOpenAI, p)
	if err != nil {
		t.Fatalf("Negotiate returned error: %v", err)
	}
	if decision.Protocol != protocols.ProtocolClaude {
		t.Errorf("Protocol = %v, want %v", decision.Protocol, protocols.ProtocolClaude)
	}
	if decision.BaseURL != p.BaseURL {
		t.Errorf("BaseURL = %q, want %q", decision.BaseURL, p.BaseURL)
	}
}

func TestNegotiate_EmptyProviderTypeTreatedAsOpenAI(t *testing.T) {
	p := &Provider{ProviderType: "", BaseURL: "https://example.com"}

	decision, err := Negotiate(protocols.ProtocolOpenAI, p)
	if err != nil {
		t.Fatalf("Negotiate returned error: %v", err)
	}
	if decision.Protocol != protocols.ProtocolOpenAI {
		t.Errorf("Protocol = %v, want %v", decision.Protocol, protocols.ProtocolOpenAI)
	}
	if decision.BaseURL != p.BaseURL {
		t.Errorf("BaseURL = %q, want %q", decision.BaseURL, p.BaseURL)
	}
}

func TestNegotiate_NilProviderReturnsError(t *testing.T) {
	_, err := Negotiate(protocols.ProtocolOpenAI, nil)
	if err == nil {
		t.Fatal("Negotiate(nil provider) expected error, got nil")
	}
}

func TestNegotiate_MultiProtocolProvider_BothIngressesPassthroughWithProviderBaseURL(t *testing.T) {
	p := &Provider{
		ProviderType:      "openai",
		BaseURL:           "https://api.openai.com",
		ProtocolEndpoints: `{"anthropic":""}`,
	}

	openaiDecision, err := Negotiate(protocols.ProtocolOpenAI, p)
	if err != nil {
		t.Fatalf("Negotiate(openai) returned error: %v", err)
	}
	if !openaiDecision.Passthrough || openaiDecision.Protocol != protocols.ProtocolOpenAI {
		t.Errorf("openai ingress: got %+v, want passthrough openai", openaiDecision)
	}
	if openaiDecision.BaseURL != p.BaseURL {
		t.Errorf("openai ingress BaseURL = %q, want %q", openaiDecision.BaseURL, p.BaseURL)
	}

	claudeDecision, err := Negotiate(protocols.ProtocolClaude, p)
	if err != nil {
		t.Fatalf("Negotiate(claude) returned error: %v", err)
	}
	if !claudeDecision.Passthrough || claudeDecision.Protocol != protocols.ProtocolClaude {
		t.Errorf("claude ingress: got %+v, want passthrough claude", claudeDecision)
	}
	// Empty per-protocol value in protocol_endpoints falls back to base_url.
	if claudeDecision.BaseURL != p.BaseURL {
		t.Errorf("claude ingress BaseURL = %q, want %q", claudeDecision.BaseURL, p.BaseURL)
	}
}

func TestNegotiate_PerProtocolBaseURL_UsedWhenNonEmpty(t *testing.T) {
	p := &Provider{
		ProviderType:      "openai",
		BaseURL:           "https://api.openai.com",
		ProtocolEndpoints: `{"anthropic":"https://claude.gw/v1"}`,
	}

	claudeDecision, err := Negotiate(protocols.ProtocolClaude, p)
	if err != nil {
		t.Fatalf("Negotiate(claude) returned error: %v", err)
	}
	if !claudeDecision.Passthrough || claudeDecision.Protocol != protocols.ProtocolClaude {
		t.Errorf("claude ingress: got %+v, want passthrough claude", claudeDecision)
	}
	if claudeDecision.BaseURL != "https://claude.gw/v1" {
		t.Errorf("claude ingress BaseURL = %q, want %q", claudeDecision.BaseURL, "https://claude.gw/v1")
	}

	openaiDecision, err := Negotiate(protocols.ProtocolOpenAI, p)
	if err != nil {
		t.Fatalf("Negotiate(openai) returned error: %v", err)
	}
	if !openaiDecision.Passthrough || openaiDecision.Protocol != protocols.ProtocolOpenAI {
		t.Errorf("openai ingress: got %+v, want passthrough openai", openaiDecision)
	}
	if openaiDecision.BaseURL != p.BaseURL {
		t.Errorf("openai ingress BaseURL = %q, want %q", openaiDecision.BaseURL, p.BaseURL)
	}
}

func TestNegotiate_MalformedProtocolEndpoints_DegradesToPrimaryOnly(t *testing.T) {
	p := &Provider{
		ProviderType:      "openai",
		BaseURL:           "https://api.openai.com",
		ProtocolEndpoints: "{bad json",
	}

	// Malformed protocol_endpoints means only the primary (openai) protocol
	// is supported, so a claude ingress must fall back to the primary
	// protocol rather than passing through.
	decision, err := Negotiate(protocols.ProtocolClaude, p)
	if err != nil {
		t.Fatalf("Negotiate(claude) returned error: %v", err)
	}
	if decision.Passthrough {
		t.Errorf("expected fallback (no passthrough) for malformed protocol_endpoints, got %+v", decision)
	}
	if decision.Protocol != protocols.ProtocolOpenAI {
		t.Errorf("Protocol = %v, want %v", decision.Protocol, protocols.ProtocolOpenAI)
	}
	if decision.BaseURL != p.BaseURL {
		t.Errorf("BaseURL = %q, want %q", decision.BaseURL, p.BaseURL)
	}
}

func TestNegotiate_AnthropicPrimaryProvider_OpenAIIngressFallsBackToClaude(t *testing.T) {
	p := &Provider{ProviderType: "anthropic", BaseURL: "https://api.anthropic.com"}

	decision, err := Negotiate(protocols.ProtocolOpenAI, p)
	if err != nil {
		t.Fatalf("Negotiate(openai) returned error: %v", err)
	}
	if decision.Passthrough {
		t.Errorf("expected fallback (no passthrough), got %+v", decision)
	}
	if decision.Protocol != protocols.ProtocolClaude {
		t.Errorf("Protocol = %v, want %v", decision.Protocol, protocols.ProtocolClaude)
	}
	if decision.BaseURL != p.BaseURL {
		t.Errorf("BaseURL = %q, want %q", decision.BaseURL, p.BaseURL)
	}
}

func TestNegotiate_AnthropicProviderWithOpenAIEndpoint_MixedBaseURLs(t *testing.T) {
	p := &Provider{
		ProviderType:      "anthropic",
		BaseURL:           "https://api.anthropic.com",
		ProtocolEndpoints: `{"openai":"https://oai.gw/v1"}`,
	}

	openaiDecision, err := Negotiate(protocols.ProtocolOpenAI, p)
	if err != nil {
		t.Fatalf("Negotiate(openai) returned error: %v", err)
	}
	if !openaiDecision.Passthrough || openaiDecision.Protocol != protocols.ProtocolOpenAI {
		t.Errorf("openai ingress: got %+v, want passthrough openai", openaiDecision)
	}
	if openaiDecision.BaseURL != "https://oai.gw/v1" {
		t.Errorf("openai ingress BaseURL = %q, want %q", openaiDecision.BaseURL, "https://oai.gw/v1")
	}

	claudeDecision, err := Negotiate(protocols.ProtocolClaude, p)
	if err != nil {
		t.Fatalf("Negotiate(claude) returned error: %v", err)
	}
	if !claudeDecision.Passthrough || claudeDecision.Protocol != protocols.ProtocolClaude {
		t.Errorf("claude ingress: got %+v, want passthrough claude", claudeDecision)
	}
	if claudeDecision.BaseURL != p.BaseURL {
		t.Errorf("claude ingress BaseURL = %q, want %q", claudeDecision.BaseURL, p.BaseURL)
	}
}
