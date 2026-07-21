package service

import (
	"crypto/rand"
	"encoding/base64"

	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// generateRandomToken generates n cryptographically secure random bytes,
// base64 URL-safe (no padding) encoded, with an optional prefix. Session
// tokens, API Keys, and any future "random token + hashed storage" feature
// all use this same recipe — this is the one place it's implemented
// (ported from .claude/reference-projects/yolorouter-deprecated's
// internal/service/token_helpers.go per this project's "copy the
// reference implementation" rule).
func generateRandomToken(n int, prefix string) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken delegates to crypto.HashToken — the single SHA-256 hex recipe
// shared with the gateway's bearer-key lookup (middleware) and the
// session-token hash (repository). Kept as a thin wrapper so service-
// internal callers read as plain hashToken(...) without importing pkg/crypto
// at every call site.
func hashToken(token string) string {
	return crypto.HashToken(token)
}
