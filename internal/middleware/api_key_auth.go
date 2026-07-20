// Package middleware additions for M5: gateway API key auth — the second
// auth path, independent of admin sessions. The caller presents a
// Yolorouter API key (Authorization: Bearer sk-yr-...); we hash it with the
// shared SHA-256 hex recipe (crypto.HashToken — the same one M4 stores and
// the session-token path uses), look up the row, and store it on the context
// for the gateway handler.
//
// Pre-call limit enforcement (state/expiry/budget/RPM/concurrency) runs in
// gateway.RelayService.Handle, NOT here: those rejections need to land in
// the request log and map to specific OpenAI error types, which only the
// handler is positioned to do. This middleware's job is purely credential
// resolution.
package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/gateway"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/pkg/crypto"
)

// APIKeyAuth resolves an Authorization: Bearer <key> credential to its
// APIKey row and stores it on the context via gateway.SetGatewayAuth. A
// missing, malformed, or unknown key returns an OpenAI-compatible 401 — the
// gateway namespace uses upstream's native error shape, NOT pkg/response
// (design doc §3).
func APIKeyAuth(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := extractBearerKey(c)
		if raw == "" {
			gateway.WriteOpenAIError(c, http.StatusUnauthorized, "authentication_error", "missing API key")
			return
		}
		key, err := repository.FindAPIKeyByHash(db, crypto.HashToken(raw))
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// "invalid" rather than "not found" — never confirm whether
				// a key exists, to avoid an enumeration oracle.
				gateway.WriteOpenAIError(c, http.StatusUnauthorized, "authentication_error", "invalid API key")
				return
			}
			gateway.WriteOpenAIError(c, http.StatusInternalServerError, "server_error", "internal error")
			return
		}
		gateway.SetGatewayAuth(c, key)
		c.Next()
	}
}

func extractBearerKey(c *gin.Context) string {
	auth := c.Request.Header.Get("Authorization")
	// RFC 7235: the auth scheme is case-insensitive. Accept "bearer"/"BEARER"/
	// etc; the scheme MUST be followed by whitespace or end-of-string, so a
	// typo like "bearerXYZ" is rejected as malformed rather than parsed as
	// token "XYZ". Trim leading spaces/tabs after the scheme.
	if len(auth) < 6 || !strings.EqualFold(auth[:6], "bearer") {
		return ""
	}
	rest := auth[6:]
	if len(rest) > 0 && rest[0] != ' ' && rest[0] != '\t' {
		return ""
	}
	return strings.TrimLeft(rest, " \t")
}
