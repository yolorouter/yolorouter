package gateway

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/images"
	"github.com/yolorouter/yolorouter/internal/providerproto"
)

// geminiIngressPathPrefix is the fixed prefix of a native Gemini ingress
// path; the remainder is the {model}:{action} segment parseGeminiPath
// splits apart. Shared with ingress.go's parseGeminiPath so the prefix the
// two functions match against can't drift apart.
const geminiIngressPathPrefix = "/v1beta/models/"

// IngressProtocol maps the client-facing request path to the wire protocol
// the caller is speaking. Structured as a switch so later versions can add
// more ingress routes without reshaping the call sites; everything unmapped
// falls back to ProtocolOpenAI.
//
// Gemini's native path is parameterized by model
// (/v1beta/models/{model}:generateContent or :streamGenerateContent), so it
// can't be matched by an exact-string switch case like the other ingress
// routes -- it's checked separately, before the switch, by prefix + one of
// the two recognized action suffixes.
func IngressProtocol(requestPath string) protocols.ProtocolID {
	if strings.HasPrefix(requestPath, geminiIngressPathPrefix) &&
		(strings.HasSuffix(requestPath, ":generateContent") || strings.HasSuffix(requestPath, ":streamGenerateContent")) {
		return protocols.ProtocolGemini
	}
	switch requestPath {
	case "/v1/messages":
		return protocols.ProtocolClaude
	case "/v1/chat/completions":
		return protocols.ProtocolOpenAI
	case "/v1/responses":
		return protocols.ProtocolResponses
	case "/v1/images/generations", images.EditPath:
		return protocols.ProtocolImages
	default:
		return protocols.ProtocolOpenAI
	}
}

// isModelsDiscoveryPath reports whether path is the OpenAI/Anthropic-shared
// model discovery route (GET /v1/models or GET /v1/models/:model). These are
// the only ingress routes that serve two wire protocols from one path, so
// they are the only place protocol selection must look beyond the path.
func isModelsDiscoveryPath(path string) bool {
	return path == "/v1/models" || strings.HasPrefix(path, "/v1/models/")
}

// IngressProtocolForRequest is IngressProtocol with header awareness for the
// model-discovery routes. IngressProtocol sees only the path and maps every
// /v1/models request to ProtocolOpenAI, but the Anthropic SDK calls the same
// route and identifies itself with the anthropic-version header. For those
// routes only, a present anthropic-version header selects the Anthropic
// (Claude) envelope. Every other path keeps its path-determined protocol, so
// routes that already resolve correctly by path — /v1/messages (Claude),
// /v1/chat/completions (OpenAI), the Gemini path — are unaffected.
// HasAnthropicVersion is passed in rather than read from a *gin.Context so
// this stays a pure function with no gin dependency, mirroring IngressProtocol.
func IngressProtocolForRequest(path string, hasAnthropicVersion bool) protocols.ProtocolID {
	p := IngressProtocol(path)
	if p == protocols.ProtocolOpenAI && isModelsDiscoveryPath(path) && hasAnthropicVersion {
		return protocols.ProtocolClaude
	}
	return p
}

// IngressProtocolForContext is the gin-aware entry point over
// IngressProtocolForRequest: it reads the path and the anthropic-version
// header from the request. Middleware (APIKeyAuth, WriteNamespacedError) and
// the model-discovery handlers all go through here, so the protocol-detection
// signals live in one place rather than inlined at each call site.
func IngressProtocolForContext(c *gin.Context) protocols.ProtocolID {
	return IngressProtocolForRequest(c.Request.URL.Path, c.GetHeader("anthropic-version") != "")
}

// IsChatEndpoint reports whether an ingress path carries a system-prompt
// surface that the gateway can inject a custom system prompt into: chat
// completions, messages, responses, and Gemini's generateContent /
// streamGenerateContent actions. It is the path-level counterpart of
// IngressProtocol -- the same set of routes IngressProtocol maps to a
// chat-shaped protocol is also the set into which a custom system prompt can
// be injected -- but stricter: IngressProtocol falls back to ProtocolOpenAI
// for unrecognized paths (so /v1/embeddings would otherwise look "openai"),
// so the explicit path allowlist is required to keep non-chat OpenAI routes
// out of the injection path.
//
// Gemini's route is a wildcard {model}:{action}, so non-chat actions on the
// same prefix (countTokens / embedContent / unknown) are excluded: only the
// two generateContent suffixes carry a systemInstruction field.
func IsChatEndpoint(requestPath string) bool {
	switch requestPath {
	case "/v1/chat/completions", "/v1/messages", "/v1/responses":
		return true
	}
	if strings.HasPrefix(requestPath, geminiIngressPathPrefix) {
		return strings.HasSuffix(requestPath, ":generateContent") ||
			strings.HasSuffix(requestPath, ":streamGenerateContent")
	}
	return false
}

// providerSupportedProtocols returns the set of wire protocols a provider
// accepts on egress — providerproto owns the reading of provider_type and
// protocol_endpoints, this is just the model.Provider adapter.
func providerSupportedProtocols(p *model.Provider) map[protocols.ProtocolID]bool {
	return providerproto.SupportedSet(p.ProviderType, p.ProtocolEndpoints)
}

// egressBaseURL returns the base URL to use when speaking proto to provider
// p: the per-protocol URL from protocol_endpoints if one is declared and
// non-empty, otherwise the provider's default base_url. This lets a provider
// expose independent upstream URLs per supported protocol (e.g. an
// OpenAI-compatible gateway that proxies Claude requests to a different
// host).
func egressBaseURL(p *model.Provider, proto protocols.ProtocolID) string {
	return providerproto.ResolveURL(providerproto.ParseEndpoints(p.ProtocolEndpoints), proto, p.BaseURL)
}

// EgressDecision is the outcome of negotiating between the ingress protocol
// and a provider's supported protocols: which wire protocol to speak to the
// provider, and where to send it.
type EgressDecision struct {
	Protocol protocols.ProtocolID
	BaseURL  string
	// Passthrough is true when Protocol equals the ingress protocol (the
	// caller's body is forwarded with only the model field rewritten, no IR
	// round-trip), and false when the request falls back to the provider's
	// primary protocol (IR decode/encode required).
	Passthrough bool
}

// Negotiate picks the egress protocol for a request against a provider. If
// the provider supports the ingress protocol directly, the request passes
// through unchanged (no IR round-trip). Otherwise it falls back to the
// provider's primary protocol, which is always in its supported set, so
// this never fails to find a route.
func Negotiate(ingress protocols.ProtocolID, p *model.Provider) (*EgressDecision, error) {
	if p == nil {
		return nil, errors.New("gateway: negotiate requires a non-nil provider")
	}

	if providerSupportedProtocols(p)[ingress] {
		return &EgressDecision{Protocol: ingress, BaseURL: egressBaseURL(p, ingress), Passthrough: true}, nil
	}
	primary := providerproto.TypeOf(p.ProviderType)
	return &EgressDecision{Protocol: primary, BaseURL: egressBaseURL(p, primary), Passthrough: false}, nil
}
