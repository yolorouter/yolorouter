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
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/gateway"
	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/pkg/crypto"
	"github.com/yolorouter/yolorouter-ce/pkg/logger"
	"github.com/yolorouter/yolorouter-ce/pkg/redact"
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
			logAuthRejection(c, db, http.StatusUnauthorized, "missing API key", "authentication_error", "missing API key")
			gateway.WriteOpenAIError(c, http.StatusUnauthorized, "authentication_error", "missing API key")
			return
		}
		key, err := repository.FindAPIKeyByHash(db, crypto.HashToken(raw))
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// "invalid" rather than "not found" — never confirm whether
				// a key exists, to avoid an enumeration oracle.
				logAuthRejection(c, db, http.StatusUnauthorized, "invalid API key", "authentication_error", "invalid API key")
				gateway.WriteOpenAIError(c, http.StatusUnauthorized, "authentication_error", "invalid API key")
				return
			}
			logAuthRejection(c, db, http.StatusInternalServerError, "auth db lookup failed", "server_error", "internal error")
			gateway.WriteOpenAIError(c, http.StatusInternalServerError, "server_error", "internal error")
			return
		}
		// Stash the raw (plaintext) key so gateway.RelayService.Handle can
		// redact it exactly out of persisted bodies (PRD §6.8.6) — the
		// gateway only ever sees the hash otherwise.
		c.Set(gateway.CallerKeyContextKey, raw)
		gateway.SetGatewayAuth(c, key)
		c.Next()
	}
}

// logAuthRejection writes one request_logs row for a gateway request rejected
// at the auth gate (missing / unknown / lookup-failed API key). The gateway's
// finalize never runs for these — Handle is never called — so without this
// row the 401 attempts would be invisible to dashboard / analytics / request-
// log audit views (Codex adversarial finding). Only the request id, a nil
// api_key_id, the rejection status, and a generic fail_reason are stored —
// never the credential or header content.
//
// errType/message are the exact error.type/message gateway.WriteOpenAIError
// is about to return to the caller (the call site right after this one) —
// passed through rather than re-derived so the persisted response_body
// matches what the caller actually received (PRD §6.8.4, Codex #2).
func logAuthRejection(c *gin.Context, db *gorm.DB, status int, reason, errType, message string) {
	// RequestID middleware is always registered ahead of APIKeyAuth (router.go
	// mounts it first on the root engine), so request_id is always set here.
	// If a future route mounts APIKeyAuth without RequestID, the empty id
	// surfaces loudly in the audit row rather than silently synthesizing a
	// fake id that can't be correlated with the access log.
	requestID := c.GetString("request_id")
	row := &model.RequestLog{
		RequestID:  requestID,
		APIKeyID:   nil,
		StatusCode: status,
		FailReason: &reason,
	}
	if err := repository.CreateRequestLog(db, row); err != nil {
		logger.Warn("middleware: write auth-rejection audit row failed",
			zap.String("request_id", requestID), zap.Error(err))
	}

	// LOG-06 (Codex #2): auth-rejected requests never reach gateway.Handle,
	// so its finalize() never runs and the request would otherwise have no
	// request_log_bodies row at all. gateway.ReadAuditBody is the same
	// bounded-read-then-redact helper gateway.captureRejectedBody uses for
	// its own early-rejection paths — a body failure must never block the
	// 401/500 response above, which ReadAuditBody's nil-on-failure contract
	// already guarantees.
	var reqBody []byte
	if c.Request != nil {
		reqBody = gateway.ReadAuditBody(c.Request.Body, "")
	}
	bodyRow := &model.RequestLogBody{
		RequestID:    requestID,
		RequestBody:  string(reqBody),
		ResponseBody: string(redact.RedactBytes(gateway.LocalErrorBody(errType, message), "")),
	}
	if err := repository.UpsertRequestLogBody(db, bodyRow); err != nil {
		logger.Warn("middleware: write auth-rejection body row failed",
			zap.String("request_id", requestID), zap.Error(err))
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
