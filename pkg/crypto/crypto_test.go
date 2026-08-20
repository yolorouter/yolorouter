package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey(t)
	const plaintext = "sk-test-1234567890abcdef"

	encoded, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if encoded == plaintext {
		t.Fatalf("Encrypt returned plaintext unchanged")
	}

	decoded, err := Decrypt(key, encoded)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if decoded != plaintext {
		t.Fatalf("expected %q, got %q", plaintext, decoded)
	}
}

func TestEncryptProducesDifferentCiphertextEachTime(t *testing.T) {
	key := testKey(t)
	a, err := Encrypt(key, "same-plaintext")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	b, err := Encrypt(key, "same-plaintext")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if a == b {
		t.Fatalf("expected different ciphertext due to random nonce, got identical output")
	}
}

func TestDecryptFailsWithWrongKey(t *testing.T) {
	key := testKey(t)
	encoded, err := Encrypt(key, "secret-value")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = byte(255 - i)
	}
	if _, err := Decrypt(wrongKey, encoded); err == nil {
		t.Fatalf("expected Decrypt to fail with wrong key, got nil error")
	}
}

func TestDecryptFailsWithMalformedBase64(t *testing.T) {
	key := testKey(t)
	if _, err := Decrypt(key, "not-valid-base64!!!"); err == nil {
		t.Fatalf("expected Decrypt to fail on malformed base64, got nil error")
	}
}

func TestDecryptFailsWhenCiphertextTooShort(t *testing.T) {
	key := testKey(t)
	tooShort := base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := Decrypt(key, tooShort); err == nil {
		t.Fatalf("expected Decrypt to fail on too-short ciphertext, got nil error")
	}
}

func TestKeyFromBase64AcceptsValid32ByteKey(t *testing.T) {
	raw := testKey(t)
	encoded := base64.StdEncoding.EncodeToString(raw)
	key, err := KeyFromBase64(encoded)
	if err != nil {
		t.Fatalf("KeyFromBase64 failed: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected 32-byte key, got %d bytes", len(key))
	}
}

func TestKeyFromBase64RejectsWrongLength(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if _, err := KeyFromBase64(shortKey); err == nil {
		t.Fatalf("expected error for wrong-length key, got nil")
	}
}

func TestKeyFromBase64RejectsMalformedBase64(t *testing.T) {
	if _, err := KeyFromBase64(strings.Repeat("!", 44)); err == nil {
		t.Fatalf("expected error for malformed base64, got nil")
	}
}

// TestEncryptFailsWithInvalidKeyLength covers the aes.NewCipher error branch
// in Encrypt: AES only accepts 16/24/32-byte keys, so any other length makes
// aes.NewCipher return an error.
func TestEncryptFailsWithInvalidKeyLength(t *testing.T) {
	invalidKey := []byte("too-short-key")
	if _, err := Encrypt(invalidKey, "plaintext"); err == nil {
		t.Fatalf("expected error for invalid key length, got nil")
	}
}

// TestDecryptFailsWithInvalidKeyLength covers the aes.NewCipher error branch
// in Decrypt for the same reason as TestEncryptFailsWithInvalidKeyLength.
func TestDecryptFailsWithInvalidKeyLength(t *testing.T) {
	invalidKey := []byte("too-short-key")
	// Must be valid base64 so the failure surfaces from aes.NewCipher, not
	// from the earlier base64-decode step.
	encoded := base64.StdEncoding.EncodeToString([]byte("some-ciphertext-bytes"))
	if _, err := Decrypt(invalidKey, encoded); err == nil {
		t.Fatalf("expected error for invalid key length, got nil")
	}
}

// failingReader is an io.Reader that always fails, used to simulate
// crypto/rand.Reader exhaustion/failure when generating the GCM nonce.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated rand failure")
}

// TestEncryptFailsWhenRandReaderErrors covers the io.ReadFull(rand.Reader, ...)
// error branch in Encrypt by temporarily swapping crypto/rand.Reader with a
// reader that always fails.
func TestEncryptFailsWhenRandReaderErrors(t *testing.T) {
	original := rand.Reader
	rand.Reader = failingReader{}
	defer func() { rand.Reader = original }()

	key := testKey(t)
	if _, err := Encrypt(key, "plaintext"); err == nil {
		t.Fatalf("expected error when rand.Reader fails, got nil")
	}
}

// TestSecretBoxInteropWithBareFunctions pins that SecretBox is a handle,
// not a format: ciphertext sealed by either API opens under the other.
// Every stored secret in the database was written by one of the two, so a
// SecretBox that diverged in nonce layout or encoding would strand
// existing rows.
func TestSecretBoxInteropWithBareFunctions(t *testing.T) {
	key := testKey(t)
	box := NewSecretBox(key)
	const plaintext = "sk-interop-secret"

	boxed, err := box.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("box.Encrypt: %v", err)
	}
	if got, err := Decrypt(key, boxed); err != nil || got != plaintext {
		t.Fatalf("bare Decrypt of box ciphertext = %q, %v; want %q", got, err, plaintext)
	}

	bare, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got, err := box.Decrypt(bare); err != nil || got != plaintext {
		t.Fatalf("box.Decrypt of bare ciphertext = %q, %v; want %q", got, err, plaintext)
	}
}

func TestGenerateRandomTokenSucceeds(t *testing.T) {
	token, err := GenerateRandomToken(32, "prefix-")
	if err != nil {
		t.Fatalf("GenerateRandomToken failed: %v", err)
	}
	if !strings.HasPrefix(token, "prefix-") {
		t.Fatalf("expected token to start with the given prefix, got %q", token)
	}
	if len(token) <= len("prefix-") {
		t.Fatalf("expected a non-empty encoded body after the prefix, got %q", token)
	}
}

// GenerateRandomToken's rand.Read error branch is NOT covered by any test:
// since Go 1.24, crypto/rand.Read is documented to never return an error —
// on failure it crashes the program irrecoverably (see
// https://go.dev/issue/66821) rather than returning one, and crypto/rand.Read
// no longer goes through the overridable crypto/rand.Reader variable, so it
// cannot be forced to return an error from a test either. That branch is
// dead code under the Go version this project builds with.
