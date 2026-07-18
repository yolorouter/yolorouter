package service

import (
	"strings"
	"testing"
)

func TestGenerateRandomTokenSucceeds(t *testing.T) {
	token, err := generateRandomToken(32, "prefix-")
	if err != nil {
		t.Fatalf("generateRandomToken failed: %v", err)
	}
	if !strings.HasPrefix(token, "prefix-") {
		t.Fatalf("expected token to start with the given prefix, got %q", token)
	}
	if len(token) <= len("prefix-") {
		t.Fatalf("expected a non-empty encoded body after the prefix, got %q", token)
	}
}

// generateRandomToken's rand.Read error branch is NOT covered by any test:
// since Go 1.24, crypto/rand.Read is documented to never return an error —
// on failure it crashes the program irrecoverably (see
// https://go.dev/issue/66821) rather than returning one, and crypto/rand.Read
// no longer goes through the overridable crypto/rand.Reader variable, so it
// cannot be forced to return an error from a test either. That branch is
// dead code under the Go version this project builds with; see the final
// coverage report for the rest of this package's shortfall accounting.
