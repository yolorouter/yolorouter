package service

import (
	"strings"
	"testing"
)

func TestHashPasswordAndCheckPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if !CheckPassword(hash, "correct-horse-battery-staple") {
		t.Fatalf("expected CheckPassword to accept the correct password")
	}
	if CheckPassword(hash, "wrong-password") {
		t.Fatalf("expected CheckPassword to reject a wrong password")
	}
}

func TestHashPasswordErrorsOnOversizedPassword(t *testing.T) {
	// bcrypt refuses any password over 72 bytes (golang.org/x/crypto/bcrypt's
	// ErrPasswordTooLong) — exercises HashPassword's error return path.
	oversized := strings.Repeat("a", 73)
	if _, err := HashPassword(oversized); err == nil {
		t.Fatalf("expected an error for a password longer than 72 bytes")
	}
}
