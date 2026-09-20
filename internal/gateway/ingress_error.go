package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// requestIDHeader is the response header name the RequestID middleware
// already sets on every response (see internal/middleware/request_id.go).
// Ingress error writers reuse this exact name instead of introducing a
// second id header, and set it defensively so it is present even on paths
// that bypass that middleware (unit tests, or a handler mounted without it).
const requestIDHeader = "X-Request-Id"

// setRequestIDHeader puts requestID on the response under requestIDHeader.
// Idempotent with the RequestID middleware (same header, same value), so
// calling it here does not produce a second/duplicate id header — it only
// guarantees the header is present even when that middleware never ran.
func setRequestIDHeader(c *gin.Context, requestID string) {
	if requestID == "" {
		return
	}
	c.Writer.Header().Set(requestIDHeader, requestID)
}

// LocalIngressErrorBody returns the wire bytes for a locally-generated error
// on the given ingress, from the protocol registry's own envelope builder.
// The builder is authoritative for the whole dialect, request-id handling
// included (inside the message, a structural field, or header-only), so
// callers hand over the RAW message and the id and get back exactly the
// bytes WriteIngressError would send — the audit row and the wire cannot
// differ. Exported so other packages generating a local error ahead of
// gateway.Handle (e.g. middleware.APIKeyAuth) store the same bytes.
func LocalIngressErrorBody(ingress protocols.ProtocolID, status int, errType, message, requestID string) []byte {
	return codecsFor(ingress).ErrorBody(status, errType, message, requestID)
}

// WriteIngressError sends a locally-generated error in the wire envelope its
// ingress protocol expects, stashes the same bytes for the audit row when an
// Exchange is on the context, and aborts the chain. Which envelope — and how
// the request id travels in it — is the registry entry's knowledge; every
// branch here is protocol-agnostic. The request id is always also set on the
// X-Request-Id header. Exported so other packages generating a local error
// ahead of gateway.Handle (e.g. middleware.APIKeyAuth's own 401s) can pick
// the caller's actual wire envelope instead of always writing the OpenAI
// shape.
func WriteIngressError(c *gin.Context, ingress protocols.ProtocolID, status int, errType, message, requestID string) {
	setRequestIDHeader(c, requestID)
	body := LocalIngressErrorBody(ingress, status, errType, message, requestID)
	if rc := relayContextFrom(c); rc != nil {
		rc.bodies.SetResponse(body)
	}
	c.Data(status, "application/json; charset=utf-8", body)
	c.Abort()
}

// unknownRouteError mirrors middleware's gatewayError/gatewayErrorBody field
// order so the serialized bytes stay identical to the NoRoute handler's
// OpenAI-surface envelope — encoding/json preserves struct field order,
// while a gin.H map would sort the keys alphabetically.
type unknownRouteError struct {
	Error unknownRouteErrorBody `json:"error"`
}

type unknownRouteErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// WriteUnknownRouteError emits the 404 an unrouted gateway path receives
// from the router's NoRoute dispatcher: the plain gateway error body for the
// OpenAI-compatible surface, the caller's protocol envelope for Claude and
// Gemini. It re-implements that dispatch instead of importing the middleware
// helper because middleware imports this package; the branch structure and
// payloads mirror WriteNamespacedError's gateway half so a wildcard-matched
// path this serves stays byte-identical with the paths the router itself
// rejects.
func WriteUnknownRouteError(c *gin.Context, ingress protocols.ProtocolID, requestID string) {
	if ingress == protocols.ProtocolClaude || ingress == protocols.ProtocolGemini {
		WriteIngressError(c, ingress, http.StatusNotFound, "invalid_request_error", "route not found", requestID)
		return
	}
	c.JSON(http.StatusNotFound, unknownRouteError{Error: unknownRouteErrorBody{
		Message: "route not found",
		Type:    "invalid_request_error",
		Code:    "route_not_found",
	}})
	c.Abort()
}
