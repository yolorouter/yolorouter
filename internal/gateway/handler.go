package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/internal/model"
)

// gatewayAPIKeyKey is the gin.Context key under which APIKeyAuth stores the
// authenticated APIKey. PostChatCompletions reads it via c.MustGet.
const gatewayAPIKeyKey = "gateway_api_key"

// SetGatewayAuth stores the authenticated API key on the context — called
// by middleware.APIKeyAuth on a successful credential resolution.
func SetGatewayAuth(c *gin.Context, apiKey *model.APIKey) {
	c.Set(gatewayAPIKeyKey, apiKey)
}

// PostChatCompletions is the gin handler for POST /v1/chat/completions. It
// pulls the APIKey the middleware already resolved and hands the request to
// RelayService.Handle, which runs the full gateway pipeline.
//
// The middleware-only-resolves / handler-enforces split is deliberate (see
// middleware.APIKeyAuth): state/expiry/budget/concurrency/RPM rejections
// need to land in the request log and map to specific OpenAI error types,
// which the handler is in a position to do and the middleware is not.
func PostChatCompletions(svc *RelayService) gin.HandlerFunc {
	return func(c *gin.Context) {
		v, ok := c.Get(gatewayAPIKeyKey)
		if !ok {
			WriteOpenAIError(c, http.StatusInternalServerError, errTypeServer, "missing gateway auth context")
			return
		}
		apiKey, ok := v.(*model.APIKey)
		if !ok {
			WriteOpenAIError(c, http.StatusInternalServerError, errTypeServer, "invalid gateway auth context")
			return
		}
		svc.Handle(c, apiKey)
	}
}
