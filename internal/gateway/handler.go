package gateway

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// gatewayAPIKeyKey is the gin.Context key under which APIKeyAuth stores the
// authenticated APIKey. PostChatCompletions reads it via c.MustGet.
const gatewayAPIKeyKey = "gateway_api_key"

// SetGatewayAuth stores the authenticated API key on the context — called
// by middleware.APIKeyAuth on a successful credential resolution.
func SetGatewayAuth(c *gin.Context, apiKey *APIKey) {
	c.Set(gatewayAPIKeyKey, apiKey)
}

// gatewayCredentialKey is the gin.Context key under which APIKeyAuth stores
// the caller's presented plaintext credential. Only Handle reads it, and only
// so a capability's loopback self-call can act as the same caller; the value
// never reaches the sanitized header capture or any log.
const gatewayCredentialKey = "gateway_api_credential"

// SetGatewayCredential stores the caller's presented credential — called by
// middleware.APIKeyAuth alongside SetGatewayAuth.
func SetGatewayCredential(c *gin.Context, credential string) {
	c.Set(gatewayCredentialKey, credential)
}

// GatewayCredential returns the stored credential, or "" when the middleware
// didn't record one.
func GatewayCredential(c *gin.Context) string {
	if v, ok := c.Get(gatewayCredentialKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// PostChatCompletions is the gin handler for both POST /v1/chat/completions
// and POST /v1/messages (see router.go — both routes are bound to this same
// handler). It pulls the APIKey the middleware already resolved and hands
// the request to Service.Handle, which runs the full gateway pipeline
// and dispatches by ingress protocol itself.
//
// The middleware-only-resolves / handler-enforces split is deliberate (see
// middleware.APIKeyAuth): state/expiry/budget/concurrency/RPM rejections
// need to land in the request log and map to specific OpenAI error types,
// which the handler is in a position to do and the middleware is not.
func PostChatCompletions(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// This guard should be unreachable in practice — APIKeyAuth always
		// sets gatewayAPIKeyKey before c.Next() on a success path, and this
		// handler is only ever mounted behind that middleware — but if it
		// somehow is (a future route mounted without APIKeyAuth, a context
		// key typo), the response must still match the caller's actual wire
		// protocol: a /v1/messages caller expects the Anthropic envelope,
		// not the OpenAI shape.
		ingress := IngressProtocol(c.Request.URL.Path)
		requestID := requestIDFor(c)
		// The gemini route is a wildcard over the whole {model}:{action}
		// segment, so a typo'd action name still matches it and would
		// otherwise fall through to the OpenAI protocol default below —
		// answering a chat-shaped reply from a gemini URL. Give the
		// unrecognized action the same 404 the router's NoRoute handler
		// gives paths it cannot match at all. Known actions classify as
		// Gemini and never reach this branch.
		if ingress != protocols.ProtocolGemini && strings.HasPrefix(c.Request.URL.Path, geminiIngressPathPrefix) {
			WriteUnknownRouteError(c, ingress, requestID)
			return
		}
		v, ok := c.Get(gatewayAPIKeyKey)
		if !ok {
			WriteIngressError(c, ingress, http.StatusInternalServerError, errTypeServer, "missing gateway auth context", requestID)
			return
		}
		apiKey, ok := v.(*APIKey)
		if !ok {
			WriteIngressError(c, ingress, http.StatusInternalServerError, errTypeServer, "invalid gateway auth context", requestID)
			return
		}
		svc.Handle(c, apiKey)
	}
}
