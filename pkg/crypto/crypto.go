// Package crypto provides AES-GCM encryption and decryption utilities.
// It is used to encrypt the provider upstream API key at rest. Note a known
// gap that was deliberately not carried over: storing the key as plaintext and
// hiding it only with json:"-" is not real protection, so here the key is
// encrypted instead.
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
)

// Encrypt encrypts plaintext with AES-GCM using the provided 32-byte key.
// Returns base64-encoded ciphertext (nonce prepended).
func Encrypt(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a base64-encoded AES-GCM ciphertext (nonce prepended).
func Decrypt(key []byte, encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

// SecretBox is the one holder of the master key for at-rest secret fields
// (provider upstream keys, API-key plaintext copies, OAuth client secrets).
// Modules that persist or read an encrypted field carry a SecretBox instead
// of the raw key bytes, so the key material itself has a single owner and
// the encrypt/decrypt recipe cannot fork per call site.
//
// The wire format is exactly Encrypt/Decrypt's: AES-256-GCM, nonce
// prepended, base64-encoded. Ciphertext written by either API is readable
// by the other — SecretBox is a handle, not a new format.
type SecretBox struct {
	key []byte
}

// NewSecretBox wraps an already-decoded 32-byte AES-256-GCM key. The key is
// copied, so a caller that later overwrites its slice cannot silently change
// what every holder of the box encrypts with.
func NewSecretBox(key []byte) SecretBox {
	return SecretBox{key: bytes.Clone(key)}
}

// Encrypt seals plaintext under the boxed key. Same output format as the
// package-level Encrypt.
func (b SecretBox) Encrypt(plaintext string) (string, error) {
	return Encrypt(b.key, plaintext)
}

// Decrypt opens a ciphertext produced by Encrypt (either API).
func (b SecretBox) Decrypt(encoded string) (string, error) {
	return Decrypt(b.key, encoded)
}

// KeyFromBase64 decodes a base64-encoded 32-byte key string.
func KeyFromBase64(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

// HashToken returns the SHA-256 hex digest of a token. API keys and session
// tokens use this for at-rest lookup: the token is already a high-entropy
// random value, so a fast indexable hash suffices (not bcrypt). This is the
// single source of truth for that recipe — the service layer,
// repository.hashSessionToken, the bearer-key auth middleware, and the gateway
// test helper all call here rather than carrying their own copies.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GenerateRandomToken generates n cryptographically secure random bytes,
// base64 URL-safe (no padding) encoded, with an optional prefix. Session
// tokens, API keys, OAuth state/verifier values, and any future "random
// token + hashed storage" feature all use this same recipe — this is the
// one place it's implemented.
func GenerateRandomToken(n int, prefix string) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}
