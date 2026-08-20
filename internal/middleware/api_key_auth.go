// Package middleware additions for gateway API key auth — the second
// auth path, independent of admin sessions. The caller presents a
// Yolorouter API key (Authorization: Bearer sk-yr-...); we hash it with the
// shared SHA-256 hex recipe (crypto.HashToken — the same one the API-key store
// and the session-token path use), look up the row, and store it on the context
// for the gateway handler.
//
// Pre-call limit enforcement (state/expiry/budget/RPM/concurrency) runs in
// gateway.Service.Handle, NOT here: those rejections need to land in
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

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/loopback"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// APIKeyAuth resolves a caller's Yolorouter API key — presented as
// Authorization: Bearer <key> (OpenAI-compatible clients), X-Api-Key: <key>
// (the Anthropic SDK / Claude Code, which never sends Authorization), or,
// on the Gemini ingress only, x-goog-api-key: <key> / ?key=<key> (the
// Google GenAI SDK) — to its APIKey row and stores it on the context via
// gateway.SetGatewayAuth. A missing, conflicting, malformed, or unknown key
// returns a 401 in the envelope the request's ingress protocol expects
// (gateway.WriteIngressError) — the gateway namespace uses upstream's
// native error shape, NOT pkg/response.
func APIKeyAuth(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		ingress := gateway.IngressProtocolForContext(c)
		requestID := c.GetString(RequestIDKey)

		raw, conflict := resolveAPIKey(c, ingress)
		if conflict {
			logAuthRejection(c, db, ingress, http.StatusUnauthorized, "conflicting API key headers", "authentication_error", "conflicting API key headers")
			gateway.WriteIngressError(c, ingress, http.StatusUnauthorized, "authentication_error", "conflicting API key headers", requestID)
			return
		}
		if raw == "" {
			logAuthRejection(c, db, ingress, http.StatusUnauthorized, "missing API key", "authentication_error", "missing API key")
			gateway.WriteIngressError(c, ingress, http.StatusUnauthorized, "authentication_error", "missing API key", requestID)
			return
		}
		key, err := repository.FindAPIKeyByHash(db, crypto.HashToken(raw))
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// "invalid" rather than "not found" — never confirm whether
				// a key exists, to avoid an enumeration oracle.
				logAuthRejection(c, db, ingress, http.StatusUnauthorized, "invalid API key", "authentication_error", "invalid API key")
				gateway.WriteIngressError(c, ingress, http.StatusUnauthorized, "authentication_error", "invalid API key", requestID)
				return
			}
			logAuthRejection(c, db, ingress, http.StatusInternalServerError, "auth db lookup failed", "server_error", "internal error")
			gateway.WriteIngressError(c, ingress, http.StatusInternalServerError, "server_error", "internal error", requestID)
			return
		}
		// A disabled account's keys stop working at this gate, immediately
		// and reversibly: no key rows are mutated on disable, so
		// re-enabling the account restores every key without repair. The
		// key id IS attributed in the audit row (unlike the unknown-key
		// branches above) — the credential is real, only its owner is
		// suspended.
		ownerStatus, err := repository.FindAPIKeyOwnerStatus(db, key.UserID)
		if err != nil {
			logAuthRejection(c, db, ingress, http.StatusInternalServerError, "auth db lookup failed", "server_error", "internal error")
			gateway.WriteIngressError(c, ingress, http.StatusInternalServerError, "server_error", "internal error", requestID)
			return
		}
		if ownerStatus != model.UserStatusEnabled {
			logAuthRejectionForKey(c, db, ingress, &key.ID, &key.UserID, http.StatusUnauthorized, "account disabled", "authentication_error", "account disabled")
			gateway.WriteIngressError(c, ingress, http.StatusUnauthorized, "authentication_error", "account disabled", requestID)
			return
		}
		gateway.SetGatewayAuth(c, key)
		// The plaintext credential rides along for loopback self-calls only
		// (the gateway re-presenting the caller's identity to itself); it is
		// never logged — the header capture is sanitized separately.
		gateway.SetGatewayCredential(c, raw)
		c.Next()
	}
}

// resolveAPIKey extracts the caller's API key from whichever source(s) the
// request's ingress protocol allows. Two sources are always checked: the
// OpenAI-style Authorization: Bearer header and the Anthropic SDK's
// X-Api-Key header (c.GetHeader is case-insensitive), so the same key
// material authenticates both wire protocols without any client-side
// change. On the Gemini ingress two more sources are checked: the Google
// GenAI SDK, when pointed at Yolorouter, sends Yolorouter's own API key as
// x-goog-api-key or as the ?key= query parameter — never as Authorization
// or X-Api-Key — so those two must be accepted there and ONLY there (an
// OpenAI/Claude/Responses caller must not be able to authenticate via a
// query parameter).
//
// All applicable sources are checked in sequence and reduced to their
// distinct non-empty values, without allocating a slice or a set. More than
// one distinct value is a conflict — a mismatch almost certainly means the
// caller sent a stale credential in one of the sources, and silently
// preferring one would mask that and mint a request whose actually-attributed
// key is invisible to the caller. Returns ("", true) on a mismatch
// (conflict); returns ("", false) when no source carries a value (the
// ordinary missing-key case, handled by the caller).
func resolveAPIKey(c *gin.Context, ingress protocols.ProtocolID) (raw string, conflict bool) {
	var found string
	add := func(v string) (isConflict bool) {
		if v == "" {
			return false
		}
		if found == "" {
			found = v
			return false
		}
		return v != found
	}
	if add(extractBearerKey(c)) {
		return "", true
	}
	if add(c.GetHeader("X-Api-Key")) {
		return "", true
	}
	if ingress == protocols.ProtocolGemini {
		if add(c.GetHeader("x-goog-api-key")) {
			return "", true
		}
		if add(c.Query("key")) {
			return "", true
		}
	}
	return found, false
}

// authRejectionBodyCap bounds how much of an UNauthenticated request body the
// auth-rejection audit path stores. 16 KiB is ample to see what a rejected
// request attempted (model, first messages) while denying a keyless attacker
// a 20 MiB-per-request storage-amplification vector into request_log_bodies.
const authRejectionBodyCap = 16 << 10 // 16 KiB

// logAuthRejection writes one request_logs row for a gateway request rejected
// at the auth gate (missing / unknown / lookup-failed API key). The gateway's
// finalize never runs for these — Handle is never called — so without this
// row the 401 attempts would be invisible to dashboard / analytics / request-
// log audit views. Only the request id, a nil
// api_key_id, the rejection status, and a generic fail_reason are stored —
// never the credential or header content.
//
// errType/message are the exact error.type/message gateway.WriteIngressError
// is about to return to the caller (the call site right after this one) —
// passed through rather than re-derived so the persisted response_body
// matches what the caller actually received.
func logAuthRejection(c *gin.Context, db *gorm.DB, ingress protocols.ProtocolID, status int, reason, errType, message string) {
	logAuthRejectionForKey(c, db, ingress, nil, nil, status, reason, errType, message)
}

// logAuthRejectionForKey is logAuthRejection with an attributed key and
// owning account — used by the disabled-owner branch, where the
// credential itself is valid and the audit row must stay visible in that
// account's own scoped views (they filter on request_logs.user_id).
func logAuthRejectionForKey(c *gin.Context, db *gorm.DB, ingress protocols.ProtocolID, keyID, userID *uint, status int, reason, errType, message string) {
	// RequestID middleware is always registered ahead of APIKeyAuth (router.go
	// mounts it first on the root engine), so request_id is always set here.
	// If a future route mounts APIKeyAuth without RequestID, the empty id
	// surfaces loudly in the audit row rather than silently synthesizing a
	// fake id that can't be correlated with the access log.
	requestID := c.GetString(RequestIDKey)
	row := &model.RequestLog{
		RequestID:  requestID,
		APIKeyID:   keyID,
		UserID:     userID,
		StatusCode: status,
		FailReason: &reason,
	}
	// This is the second request_logs writer (gateway.Handle's recorder is
	// the first) — a rejected loopback sub-call must carry the same source
	// marking here, or it would read as a caller request in the log. The
	// same trust rule applies: the parent linkage is only honored alongside
	// a valid process token.
	if c.GetHeader(loopback.HeaderInternal) == loopback.Token {
		row.Source = model.RequestLogSourceVisionFallback
		row.ParentRequestID = c.GetHeader(loopback.HeaderParent)
	}
	if err := repository.CreateRequestLog(db, row); err != nil {
		logger.Warn("middleware: write auth-rejection audit row failed",
			zap.String("request_id", requestID), zap.Error(err))
	}

	// auth-rejected requests never reach gateway.Handle,
	// so its finalize() never runs and the request would otherwise have no
	// request_log_bodies row at all. A body failure must never block the
	// 401/500 response above, which ReadAuditBodyCapped's nil-on-failure
	// contract already guarantees. Capped at authRejectionBodyCap (far below
	// the 20 MiB post-auth cap): this path serves UNauthenticated callers, so
	// a small ceiling keeps the audit useful without letting a keyless
	// attacker inflate request_log_bodies with 20 MiB bodies per rejection.
	var reqBody, reqHeaders []byte
	if c.Request != nil {
		reqBody = gateway.ReadAuditBodyCapped(c.Request.Body, authRejectionBodyCap)
		// Record the (masked) request headers even for an
		// auth-rejected request, mirroring gateway.Handle's own capture.
		reqHeaders = gateway.SanitizeHeaders(c.Request.Header)
	}
	// LocalIngressErrorBody is authoritative for each ingress dialect's
	// request-id handling (inside the message, a structural field, or
	// header-only), so the raw message goes in and the stored response_body
	// matches what the caller actually received byte for byte, on every
	// ingress — no transform is mirrored here.
	bodyRow := &model.RequestLogBody{
		RequestID:      requestID,
		RequestHeaders: string(reqHeaders),
		RequestBody:    string(reqBody),
		ResponseBody:   string(gateway.LocalIngressErrorBody(ingress, status, errType, message, requestID)),
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
