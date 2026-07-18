package model

import "testing"

func TestProviderTableNames(t *testing.T) {
	if got := (Provider{}).TableName(); got != "providers" {
		t.Fatalf("expected \"providers\", got %q", got)
	}
	if got := (ProviderKey{}).TableName(); got != "provider_keys" {
		t.Fatalf("expected \"provider_keys\", got %q", got)
	}
	if got := (ProviderKeyFingerprint{}).TableName(); got != "provider_key_fingerprint" {
		t.Fatalf("expected \"provider_key_fingerprint\", got %q", got)
	}
}

func TestProviderKeyStatusConstantsAreDistinct(t *testing.T) {
	if ProviderKeyStatusEnabled == ProviderKeyStatusDisabled {
		t.Fatalf("enabled and disabled constants must differ")
	}
	if VerificationStatusUntested == VerificationStatusPassed || VerificationStatusPassed == VerificationStatusFailed {
		t.Fatalf("verification status constants must all differ")
	}
}
