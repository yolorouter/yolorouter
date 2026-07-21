package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func TestComputeRunningStatusNotConfiguredWhenNoKeys(t *testing.T) {
	if got := computeRunningStatus(nil, 1); got != RunningStatusNotConfigured {
		t.Fatalf("expected not_configured, got %q", got)
	}
}

func TestComputeRunningStatusUnavailableWhenNoEnabledKeys(t *testing.T) {
	keys := []model.ProviderKey{{ManagementStatus: model.ProviderKeyStatusDisabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: 1}}
	if got := computeRunningStatus(keys, 1); got != RunningStatusUnavailable {
		t.Fatalf("expected unavailable, got %q", got)
	}
}

func TestComputeRunningStatusPendingWhenOnlyUntestedCurrentKeys(t *testing.T) {
	keys := []model.ProviderKey{{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusUntested, AuthorizedDestinationVersion: 1}}
	if got := computeRunningStatus(keys, 1); got != RunningStatusPending {
		t.Fatalf("expected pending_test, got %q", got)
	}
}

func TestComputeRunningStatusAvailableWhenAllEnabledKeysGood(t *testing.T) {
	keys := []model.ProviderKey{
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: 1},
		{ManagementStatus: model.ProviderKeyStatusDisabled, VerificationStatus: model.VerificationStatusFailed, AuthorizedDestinationVersion: 1},
	}
	if got := computeRunningStatus(keys, 1); got != RunningStatusAvailable {
		t.Fatalf("expected available, got %q", got)
	}
}

func TestComputeRunningStatusPartialWhenGoodAndBadEnabledKeysCoexist(t *testing.T) {
	keys := []model.ProviderKey{
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: 1},
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusFailed, AuthorizedDestinationVersion: 1},
	}
	if got := computeRunningStatus(keys, 1); got != RunningStatusPartial {
		t.Fatalf("expected partial, got %q", got)
	}
}

func TestComputeRunningStatusPartialWhenGoodKeyCoexistsWithUntestedEnabledKey(t *testing.T) {
	keys := []model.ProviderKey{
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: 1},
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusUntested, AuthorizedDestinationVersion: 1},
	}
	if got := computeRunningStatus(keys, 1); got != RunningStatusPartial {
		t.Fatalf("expected partial (untested enabled keys are explicitly included), got %q", got)
	}
}

func TestComputeRunningStatusUnavailableWhenGoodKeyNeedsReentry(t *testing.T) {
	// authorized_destination_version=1 but current destinationVersion=2:
	// this key "passed" against an address that no longer applies — not
	// good anymore.
	keys := []model.ProviderKey{
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: 1},
	}
	if got := computeRunningStatus(keys, 2); got != RunningStatusUnavailable {
		t.Fatalf("expected unavailable when the only passed key needs re-entry, got %q", got)
	}
}

// fakeProviderClient never makes a real network call — tests configure
// outcomes per call. sideEffect, when set, runs synchronously before the
// call returns — used to simulate a concurrent DB write racing against an
// in-flight test call (e.g. a plaintext swap bumping config_version while
// TestAllProviderKeys is mid-test for that key), so the write-back CAS then
// observes a stale snapshot and discards the result.
type fakeProviderClient struct {
	result     TestResult
	err        error
	calls      int
	lastModel  string
	sideEffect func()
}

func (f *fakeProviderClient) TestChatCompletion(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	f.calls++
	f.lastModel = model
	if f.sideEffect != nil {
		f.sideEffect()
	}
	return f.result, f.err
}

func (f *fakeProviderClient) TestStreamingCompletion(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	f.calls++
	f.lastModel = model
	if f.sideEffect != nil {
		f.sideEffect()
	}
	return f.result, f.err
}

func (f *fakeProviderClient) TestFunctionCalling(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	f.calls++
	f.lastModel = model
	if f.sideEffect != nil {
		f.sideEffect()
	}
	return f.result, f.err
}

func testMasterKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

func newTestProviderService(t *testing.T) (*ProviderService, *gorm.DB, *fakeProviderClient) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	client := &fakeProviderClient{result: TestResult{Outcome: TestSuccess, DurationMs: 10}}
	svc := NewProviderService(db, testMasterKey(), client)
	return svc, db, client
}

// newTestProviderServiceWithInvalidMasterKey builds a service whose
// masterKey is a length aes.NewCipher rejects (anything but 16/24/32
// bytes) — the only reliable way to force crypto.Encrypt to fail from a
// black-box test, exercising the service layer's encErr branches.
func newTestProviderServiceWithInvalidMasterKey(t *testing.T) (*ProviderService, *gorm.DB, *fakeProviderClient) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	client := &fakeProviderClient{result: TestResult{Outcome: TestSuccess, DurationMs: 10}}
	svc := NewProviderService(db, []byte("too-short-for-aes"), client)
	return svc, db, client
}

func TestVerifyMasterKeyFingerprintWritesOnFirstRun(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	if err := svc.VerifyMasterKeyFingerprint(time.Now()); err != nil {
		t.Fatalf("expected first run to succeed and write the fingerprint, got: %v", err)
	}
	// Second call with the SAME key must also succeed (decrypts its own
	// previously-written probe).
	if err := svc.VerifyMasterKeyFingerprint(time.Now()); err != nil {
		t.Fatalf("expected second run with the same key to succeed, got: %v", err)
	}
}

func TestVerifyMasterKeyFingerprintFailsOnMismatchedKey(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	if err := svc.VerifyMasterKeyFingerprint(time.Now()); err != nil {
		t.Fatalf("first run failed: %v", err)
	}

	otherKey := make([]byte, 32)
	for i := range otherKey {
		otherKey[i] = byte(255 - i)
	}
	svcWithDifferentKey := NewProviderService(db, otherKey, &fakeProviderClient{})
	if err := svcWithDifferentKey.VerifyMasterKeyFingerprint(time.Now()); err == nil {
		t.Fatalf("expected a mismatched master key to fail the fingerprint check")
	}
}

func TestVerifyMasterKeyFingerprintErrorsWhenEncryptFails(t *testing.T) {
	svc, _, _ := newTestProviderServiceWithInvalidMasterKey(t)
	if err := svc.VerifyMasterKeyFingerprint(time.Now()); err == nil {
		t.Fatalf("expected an error when the master key is an invalid AES key length")
	}
}

func TestVerifyMasterKeyFingerprintErrorsWhenClaimFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "provider_key_fingerprint")

	if err := svc.VerifyMasterKeyFingerprint(time.Now()); err == nil {
		t.Fatalf("expected an error when the provider_key_fingerprint table is missing")
	}
}

// VerifyMasterKeyFingerprint's third error branch — GetProviderKeyFingerprint
// failing AFTER ClaimProviderKeyFingerprintIfAbsent has already succeeded —
// is not covered by any test here: forcing exactly that interleaving (the
// claim's INSERT ... ON CONFLICT DO NOTHING succeeding while the immediately
// following SELECT on the very same row fails) isn't reachable by dropping
// or trigger-blocking the table, since both statements touch the same table
// within the same synchronous call and any table-level fault that breaks
// the SELECT also breaks the INSERT before it ever runs. Doing so would
// need dependency injection into the repository layer, which is out of
// this task's scope. See the final coverage report for how this is
// accounted for.

func TestCreateProviderCreatesProviderAndFirstKey(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()

	view, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "openai-main", BaseURL: "https://api.example.com/v1",
		KeyLabel: "primary", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if view.ID == 0 || len(view.Keys) != 1 {
		t.Fatalf("expected a provider with 1 key, got %+v", view)
	}
	if view.Keys[0].KeyPrefix == "" || view.Keys[0].KeyPrefix == "sk-abcdefghijklmnopqrstuvwxyz1234" {
		t.Fatalf("expected a masked key_prefix, not empty or the full plaintext, got %q", view.Keys[0].KeyPrefix)
	}
}

func TestCreateProviderRejectsDuplicateName(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	input := CreateProviderInput{Name: "dup", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini"}

	if _, err := svc.CreateProvider(context.Background(), input, now); err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	if _, err := svc.CreateProvider(context.Background(), input, now); !errors.Is(err, errcode.ErrProviderNameTaken) {
		t.Fatalf("expected ErrProviderNameTaken, got %v", err)
	}
}

func TestKeyPrefixForClampsToPlaintextLength(t *testing.T) {
	cases := []struct {
		name      string
		plaintext string
		want      string
	}{
		{"empty", "", ""},
		{"shorter than 4 chars clamps to empty", "abc", ""},
		{"exactly 4 chars clamps to empty", "abcd", ""},
		{"between 4 and 14 chars uses len-4", "abcdefgh", "abcd"},
		{"caps at 10 chars for long plaintext", "sk-abcdefghijklmnopqrstuvwxyz1234", "sk-abcdefg"},
		// Regression: the original byte-sliced
		// implementation could cut a multi-byte UTF-8 character in half if
		// one straddled the cutoff, producing invalid UTF-8. Each of these
		// multi-byte runes (é = 2 bytes, 中 = 3 bytes) sits exactly at the
		// old byte-index cutoff for its plaintext's length; a rune-safe
		// implementation must return valid UTF-8 either way.
		{"multi-byte rune straddling the len-4 cutoff", "café-abcdefghijklmnopqrstuvwxyz1234", "café-abcde"},
		{"multi-byte rune straddling the 10-rune cap", "sk-中国12345678901234", "sk-中国12345"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := keyPrefixFor(c.plaintext)
			if got != c.want {
				t.Fatalf("keyPrefixFor(%q) = %q, want %q", c.plaintext, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("keyPrefixFor(%q) = %q is not valid UTF-8", c.plaintext, got)
			}
		})
	}
}

func TestCreateProviderErrorsWhenEncryptFails(t *testing.T) {
	svc, _, _ := newTestProviderServiceWithInvalidMasterKey(t)
	_, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected an error when the master key is an invalid AES key length")
	}
}

func TestCreateProviderRejectsPlaintextShorterThan20Chars(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	_, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "short-key", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "too-short", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected an error for a plaintext shorter than the minimum length")
	}
}

func TestUpdateProviderNotFound(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	_, err := svc.UpdateProvider(9999, UpdateProviderInput{Name: "x", BaseURL: "https://a.example.com"}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestUpdateProviderErrorsWhenProvidersTableMissing(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "providers")

	_, err := svc.UpdateProvider(1, UpdateProviderInput{Name: "x", BaseURL: "https://a.example.com"}, time.Now().UTC())
	if err == nil || errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected a raw DB error (not ErrProviderNotFound), got %v", err)
	}
}

func TestUpdateProviderErrorsWhenBaseURLUpdateFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://old.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	blockTableWrites(t, db, "providers", "UPDATE")

	_, err = svc.UpdateProvider(provider.ID, UpdateProviderInput{Name: provider.Name, BaseURL: "https://new.example.com"}, now)
	if err == nil {
		t.Fatalf("expected an error when the base_url UPDATE fails")
	}
}

func TestUpdateProviderRejectsDuplicateNameOnRename(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	if _, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "taken", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now); err != nil {
		t.Fatalf("CreateProvider(taken) failed: %v", err)
	}
	other, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "other", BaseURL: "https://b.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider(other) failed: %v", err)
	}

	_, err = svc.UpdateProvider(other.ID, UpdateProviderInput{Name: "taken", BaseURL: other.BaseURL}, now)
	if !errors.Is(err, errcode.ErrProviderNameTaken) {
		t.Fatalf("expected ErrProviderNameTaken, got %v", err)
	}
}

// TestUpdateProviderRollsBackBaseURLWhenNameConflicts is the direct
// regression test for a bug: base_url (and its
// destination_version bump, which instantly invalidates every key's
// authorization) and name/note used to be written as two independent,
// non-transactional statements. If the base_url write committed and the
// name write then failed on a duplicate name, the admin saw a failed
// request but the base_url/destination_version change had already
// silently landed. Both writes must now share one transaction so a name
// conflict rolls back the base_url change too.
func TestUpdateProviderRollsBackBaseURLWhenNameConflicts(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	if _, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "taken", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now); err != nil {
		t.Fatalf("CreateProvider(taken) failed: %v", err)
	}
	other, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "other", BaseURL: "https://b.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider(other) failed: %v", err)
	}

	_, err = svc.UpdateProvider(other.ID, UpdateProviderInput{Name: "taken", BaseURL: "https://changed.example.com"}, now)
	if !errors.Is(err, errcode.ErrProviderNameTaken) {
		t.Fatalf("expected ErrProviderNameTaken, got %v", err)
	}

	reloaded, err := svc.GetProviderDetail(other.ID)
	if err != nil {
		t.Fatalf("GetProviderDetail failed: %v", err)
	}
	if reloaded.BaseURL != "https://b.example.com" {
		t.Fatalf("expected base_url to be rolled back to the original value, got %q", reloaded.BaseURL)
	}
	if reloaded.Keys[0].NeedsReentry {
		t.Fatalf("expected the existing key to NOT need re-entry after a rolled-back base_url change, got needs_reentry=true")
	}
}

func TestUpdateProviderErrorsWhenNameNoteUpdateFailsForNonUniqueReason(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	blockTableWrites(t, db, "providers", "UPDATE")

	// Same BaseURL — skips UpdateProviderBaseURL entirely, isolating the
	// UpdateProviderNameNote failure.
	_, err = svc.UpdateProvider(provider.ID, UpdateProviderInput{Name: "renamed", Note: "n", BaseURL: provider.BaseURL}, now)
	if err == nil || errors.Is(err, errcode.ErrProviderNameTaken) {
		t.Fatalf("expected a raw DB error (not ErrProviderNameTaken), got %v", err)
	}
}

func TestSetProviderStatusEnablesAndDisables(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	if err := svc.SetProviderStatus(provider.ID, false, now); err != nil {
		t.Fatalf("SetProviderStatus(false) failed: %v", err)
	}
	var got model.Provider
	if err := db.Where("id = ?", provider.ID).First(&got).Error; err != nil {
		t.Fatalf("reload provider failed: %v", err)
	}
	if got.ManagementStatus != model.ProviderStatusDisabled {
		t.Fatalf("expected management_status=disabled, got %d", got.ManagementStatus)
	}

	if err := svc.SetProviderStatus(provider.ID, true, now); err != nil {
		t.Fatalf("SetProviderStatus(true) failed: %v", err)
	}
	if err := db.Where("id = ?", provider.ID).First(&got).Error; err != nil {
		t.Fatalf("reload provider failed: %v", err)
	}
	if got.ManagementStatus != model.ProviderStatusEnabled {
		t.Fatalf("expected management_status=enabled, got %d", got.ManagementStatus)
	}
}

// TestSetProviderStatusReturnsNotFoundForUnknownProvider is the direct
// regression test for a bug: this was the only
// provider-scoped mutation with no prior existence check, so toggling a
// nonexistent provider ID matched zero rows, GORM reported no error, and
// the caller got a false success instead of ErrProviderNotFound.
func TestSetProviderStatusReturnsNotFoundForUnknownProvider(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	if err := svc.SetProviderStatus(999999, false, time.Now().UTC()); !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestSetProviderStatusErrorsWhenUpdateFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	blockTableWrites(t, db, "providers", "UPDATE")

	if err := svc.SetProviderStatus(provider.ID, true, now); err == nil {
		t.Fatalf("expected an error when the management_status UPDATE fails")
	}
}

func TestUpdateProviderBaseURLResetsAllKeysToNeedsReentry(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	view, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-a", BaseURL: "https://old.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	updated, err := svc.UpdateProvider(view.ID, UpdateProviderInput{Name: "provider-a", BaseURL: "https://new.example.com"}, now)
	if err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}
	if updated.Keys[0].NeedsReentry != true {
		t.Fatalf("expected the existing key to be flagged needs_reentry after an address change")
	}
}

func TestCreateProviderKeyServerSideReverifyEnablesOnSuccess(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestSuccess, DurationMs: 5}
	now := time.Now().UTC()

	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	keyView, err := svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderKeyStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProviderKey failed: %v", err)
	}
	if keyView.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status=passed after a successful server-side test, got %d", keyView.VerificationStatus)
	}
	if keyView.ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Fatalf("expected management_status=enabled, got %d", keyView.ManagementStatus)
	}
}

func TestCreateProviderKeyServerSideReverifyForcesDisabledOnFailure(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestAuthFailed, DurationMs: 5}
	now := time.Now().UTC()

	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	keyView, err := svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderKeyStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProviderKey should not itself error on a failed test — it must still create the row: %v", err)
	}
	if keyView.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("expected verification_status=failed, got %d", keyView.VerificationStatus)
	}
	if keyView.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected management_status forced to disabled despite the request asking for enabled, got %d", keyView.ManagementStatus)
	}
}

func TestCreateProviderKeyRejectsDuplicateLabel(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "primary", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	_, err = svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "primary", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, now)
	if !errors.Is(err, errcode.ErrProviderKeyLabelTaken) {
		t.Fatalf("expected ErrProviderKeyLabelTaken, got %v", err)
	}
}

func TestCreateProviderKeyRejectsPlaintextShorterThan20Chars(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	_, err = svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "too-short", TestModel: "gpt-4o-mini",
	}, now)
	if err == nil {
		t.Fatalf("expected an error for a plaintext shorter than the minimum length")
	}
}

func TestCreateProviderKeyErrorsWhenEncryptFails(t *testing.T) {
	svc, db, _ := newTestProviderServiceWithInvalidMasterKey(t)
	now := time.Now().UTC()

	// CreateProvider itself can't succeed with this invalid master key
	// either (it encrypts its own first key the same way), so seed the
	// provider row directly to give CreateProviderKey something to look up
	// before it reaches its own crypto.Encrypt call.
	seeded := &model.Provider{
		Name: "p1", ProviderType: "openai", BaseURL: "https://a.example.com",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(seeded).Error; err != nil {
		t.Fatalf("seed provider failed: %v", err)
	}

	_, err := svc.CreateProviderKey(context.Background(), seeded.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, now)
	if err == nil {
		t.Fatalf("expected an error when the master key is an invalid AES key length")
	}
}

func TestCreateProviderKeyErrorsWhenLabelLookupFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	dropTable(t, db, "provider_keys")

	_, err = svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, now)
	if err == nil || errors.Is(err, errcode.ErrProviderKeyLabelTaken) {
		t.Fatalf("expected a raw DB error (not ErrProviderKeyLabelTaken), got %v", err)
	}
}

func TestCreateProviderKeyNotFoundProvider(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	_, err := svc.CreateProviderKey(context.Background(), 9999, CreateKeyInput{
		Label: "k1", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestCreateProviderKeyErrorsWhenProviderLookupFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "providers")

	_, err := svc.CreateProviderKey(context.Background(), 1, CreateKeyInput{
		Label: "k1", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err == nil || errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected a raw DB error (not ErrProviderNotFound), got %v", err)
	}
}

// NextSortOrder's own error return (inside CreateProviderKey, between the
// label lookup and the pending-test insert) is not exercised by any test
// here: it's a `SELECT MAX(sort_order) ...` against provider_keys, the
// exact same table FindProviderKeyByLabel (called immediately before it)
// also queries. There is no way to break just the aggregate SELECT while
// leaving the label lookup's SELECT (which — via gorm's default full-struct
// column list — also implicitly reads sort_order) intact, short of
// dependency-injecting the repository layer, which is out of this task's
// scope. Same reasoning applies to CreateProviderKeyPendingTest's
// isUniqueViolation branch a few lines below (a label-uniqueness TOCTOU
// race exactly like TestCreateProviderConcurrentSameNameHitsUniqueViolationBranch's,
// and subject to the same single-connection-pool serialization that
// prevented that one from reliably triggering either).

func TestCreateProviderKeyErrorsWhenPendingInsertFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	blockTableWrites(t, db, "provider_keys", "INSERT")

	_, err = svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, now)
	if err == nil || errors.Is(err, errcode.ErrProviderKeyLabelTaken) {
		t.Fatalf("expected a raw DB error (not ErrProviderKeyLabelTaken), got %v", err)
	}
}

// TestCreateProviderKeyStillCreatesRowWhenClientCallErrors exercises
// runNewPlaintextTestAndCommit's own err-from-client branch: the client
// itself refusing the call (e.g. its concurrency cap) must not be silently
// classified as a passing test (TestSuccess is TestOutcome's zero value) —
// the row is created but stays untested/disabled since no real test ran.
func TestCreateProviderKeyStillCreatesRowWhenClientCallErrors(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	client.err = fmt.Errorf("too many concurrent provider test calls in flight")

	view, err := svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderKeyStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProviderKey should not itself error when the client refuses the call: %v", err)
	}
	if view.VerificationStatus != model.VerificationStatusUntested {
		t.Fatalf("expected verification_status to stay untested, got %d", view.VerificationStatus)
	}
	if view.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected management_status to stay disabled, got %d", view.ManagementStatus)
	}
}

// TestCreateProviderKeyDiscardsResultWhenCommitLosesCASRace mirrors
// TestTestAllProviderKeysSkipsWhenCommitLosesCASRace for the
// create-a-brand-new-key flow: the fake client's side effect simulates a
// concurrent write landing on this exact row between the test call and the
// write-back CAS, so the real test result must be silently discarded.
func TestCreateProviderKeyDiscardsResultWhenCommitLosesCASRace(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess, DurationMs: 5}
	client.sideEffect = func() {
		if err := db.Exec("UPDATE provider_keys SET config_version = config_version + 1 WHERE label = ?", "k2").Error; err != nil {
			t.Fatalf("simulated concurrent config_version bump failed: %v", err)
		}
	}

	view, err := svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderKeyStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProviderKey failed: %v", err)
	}
	if view.VerificationStatus != model.VerificationStatusUntested {
		t.Fatalf("expected the discarded test result to leave verification_status untested, got %d", view.VerificationStatus)
	}
	if view.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected the discarded test result to leave management_status disabled, got %d", view.ManagementStatus)
	}
}

// TestCreateProviderKeyErrorsWhenReloadFailsAfterVerify forces the final
// FindProviderKeyByID (after the server-side re-verify) to fail by having
// the fake client's side effect delete the just-inserted row while the
// "test call" is in flight — simulating the row vanishing (e.g. a
// concurrent provider/key deletion) between creation and reload.
func TestCreateProviderKeyErrorsWhenReloadFailsAfterVerify(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	client.sideEffect = func() {
		if err := db.Exec("DELETE FROM provider_keys WHERE label = ?", "k2").Error; err != nil {
			t.Fatalf("simulated concurrent deletion failed: %v", err)
		}
	}

	if _, err := svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, now); err == nil {
		t.Fatalf("expected an error when the key row vanishes before the final reload")
	}
}

func TestUpdateProviderKeyWithNewPlaintextResetsAndRetests(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess, DurationMs: 5}

	newPlaintext := "sk-newnewnewnewnewnewnewnewnewnew"
	updated, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: provider.Keys[0].Label, Plaintext: &newPlaintext, TestModel: "gpt-4o-mini", ManagementStatus: new(model.ProviderKeyStatusEnabled),
	}, now)
	if err != nil {
		t.Fatalf("UpdateProviderKey failed: %v", err)
	}
	if updated.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected the new plaintext's own test result (passed), got %d", updated.VerificationStatus)
	}
}

func TestUpdateProviderKeyLabelOnlyDoesNotRetrigger(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	callsBefore := client.calls

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: new(provider.Keys[0].ManagementStatus),
	}, now)
	if err != nil {
		t.Fatalf("UpdateProviderKey failed: %v", err)
	}
	if client.calls != callsBefore {
		t.Fatalf("expected a label-only edit to trigger no network test, calls went from %d to %d", callsBefore, client.calls)
	}
}

// TestUpdateProviderKeyLabelOnlyEditWithOmittedStatusPreservesCurrentStatus
// is the direct regression test for a bug:
// UpdateKeyInput.ManagementStatus used to be a plain int, so a request that
// legally omits management_status entirely (updateKeyRequest's JSON tag is
// binding:"omitempty,oneof=1 2") bound to Go's zero value 0 and was written
// straight to the DB via UpdateProviderKeyLabelAndStatus — silently
// corrupting a previously-enabled key (management_status=1) into status 0,
// neither Enabled nor Disabled. The field is now *int (nil = not provided),
// mirroring Plaintext's own nil-means-unchanged convention on the same
// struct.
func TestUpdateProviderKeyLabelOnlyEditWithOmittedStatusPreservesCurrentStatus(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if provider.Keys[0].ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Fatalf("test setup: expected the created key to be enabled, got status %d", provider.Keys[0].ManagementStatus)
	}

	updated, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: provider.Keys[0].TestModel,
	}, now)
	if err != nil {
		t.Fatalf("UpdateProviderKey failed: %v", err)
	}
	if updated.ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Fatalf("expected management_status to stay Enabled(%d) when omitted from the request, got %d",
			model.ProviderKeyStatusEnabled, updated.ManagementStatus)
	}
}

// TestUpdateProviderKeyLabelOnlyEditCannotEnableUnverifiedKey is the direct
// regression test for a bug: this path used to write
// ManagementStatus straight to the DB with no verification check at all,
// unlike SetProviderKeyStatus — so a label-only edit could silently enable
// a key that had never passed a real test.
func TestUpdateProviderKeyLabelOnlyEditCannotEnableUnverifiedKey(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestAuthFailed}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	// key's verification_status is now "failed" (client returned TestAuthFailed).

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: new(model.ProviderKeyStatusEnabled),
	}, now)
	if !errors.Is(err, errcode.ErrProviderKeyNotVerified) {
		t.Fatalf("expected ErrProviderKeyNotVerified, got %v", err)
	}
}

// TestUpdateProviderKeyLabelOnlyEditCannotEnableKeyNeedingReentry mirrors
// TestSetProviderKeyStatusRejectsReenablingKeyThatNeedsReentry, but through
// the label-only edit path instead of the dedicated status endpoint —
// both entry points share verifyKeyEnableAllowed and must both reject.
func TestUpdateProviderKeyLabelOnlyEditCannotEnableKeyNeedingReentry(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestSuccess}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://old.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.UpdateProvider(provider.ID, UpdateProviderInput{Name: provider.Name, BaseURL: "https://new.example.com"}, now); err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}
	// The key passed verification against the OLD address; the base_url
	// change above bumped destination_version, so it now needs re-entry
	// despite verification_status still reading "passed".

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: new(model.ProviderKeyStatusEnabled),
	}, now)
	if !errors.Is(err, errcode.ErrProviderKeyNeedsReentry) {
		t.Fatalf("expected ErrProviderKeyNeedsReentry, got %v", err)
	}
}

// TestSetProviderKeyStatusRejectsReenablingKeyThatNeedsReentry is the
// direct regression test for a bug: this path only
// checked VerificationStatus, never AuthorizedDestinationVersion — so a
// key that passed verification against an address the provider no longer
// points at could still be re-enabled via the plain status toggle, even
// though TestProviderKey explicitly refuses to even test that same key
// for the identical reason.
func TestSetProviderKeyStatusRejectsReenablingKeyThatNeedsReentry(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestSuccess}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://old.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.UpdateProvider(provider.ID, UpdateProviderInput{Name: provider.Name, BaseURL: "https://new.example.com"}, now); err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}

	if err := svc.SetProviderKeyStatus(provider.ID, provider.Keys[0].ID, true, now); !errors.Is(err, errcode.ErrProviderKeyNeedsReentry) {
		t.Fatalf("expected ErrProviderKeyNeedsReentry, got %v", err)
	}
}

// TestUpdateProviderKeyRejectsKeyBelongingToDifferentProvider,
// TestSetProviderKeyStatusRejectsKeyBelongingToDifferentProvider, and
// TestTestProviderKeyRejectsKeyBelongingToDifferentProvider are the direct
// regression tests for a bug: all three previously looked
// a key up purely by keyID and never checked it against the providerID in
// the URL, unlike SwapProviderKeySortOrder (used by the reorder endpoint),
// which correctly conditions its update on "id = ? AND provider_id = ?".
func TestUpdateProviderKeyRejectsKeyBelongingToDifferentProvider(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	providerA, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-a", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider(a) failed: %v", err)
	}
	providerB, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-b", BaseURL: "https://b.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider(b) failed: %v", err)
	}

	_, err = svc.UpdateProviderKey(context.Background(), providerA.ID, providerB.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: providerB.Keys[0].TestModel, ManagementStatus: new(providerB.Keys[0].ManagementStatus),
	}, now)
	if !errors.Is(err, errcode.ErrProviderKeyNotFound) {
		t.Fatalf("expected ErrProviderKeyNotFound for a key belonging to a different provider, got %v", err)
	}
}

func TestSetProviderKeyStatusRejectsKeyBelongingToDifferentProvider(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	providerA, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-a", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider(a) failed: %v", err)
	}
	providerB, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-b", BaseURL: "https://b.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider(b) failed: %v", err)
	}

	if err := svc.SetProviderKeyStatus(providerA.ID, providerB.Keys[0].ID, false, now); !errors.Is(err, errcode.ErrProviderKeyNotFound) {
		t.Fatalf("expected ErrProviderKeyNotFound for a key belonging to a different provider, got %v", err)
	}
}

func TestTestProviderKeyRejectsKeyBelongingToDifferentProvider(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	providerA, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-a", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider(a) failed: %v", err)
	}
	providerB, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-b", BaseURL: "https://b.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider(b) failed: %v", err)
	}

	if _, err := svc.TestProviderKey(context.Background(), providerA.ID, providerB.Keys[0].ID, now); !errors.Is(err, errcode.ErrProviderKeyNotFound) {
		t.Fatalf("expected ErrProviderKeyNotFound for a key belonging to a different provider, got %v", err)
	}
}

func TestUpdateProviderKeyNotFound(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	_, err := svc.UpdateProviderKey(context.Background(), 9999, 9999, UpdateKeyInput{Label: "x"}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrProviderKeyNotFound) {
		t.Fatalf("expected ErrProviderKeyNotFound, got %v", err)
	}
}

func TestUpdateProviderKeyErrorsWhenProviderLookupFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	dropTable(t, db, "providers")

	if _, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{Label: "renamed"}, now); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestUpdateProviderKeyErrorsWhenKeyLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "provider_keys")

	if _, err := svc.UpdateProviderKey(context.Background(), 1, 1, UpdateKeyInput{Label: "renamed"}, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestUpdateProviderKeyErrorsWhenReloadFailsAfterLabelOnlyEdit(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	// Simulates the row vanishing between the label/status UPDATE and the
	// reload immediately after it (e.g. a concurrent delete) — an AFTER
	// UPDATE trigger fires once the UPDATE itself has already succeeded, so
	// UpdateProviderKeyLabelAndStatus's own error return is NOT exercised
	// by this (see the plain UPDATE-blocking tests for that), only the
	// reload's.
	stmt := "CREATE TRIGGER delete_after_key_update AFTER UPDATE ON provider_keys " +
		"BEGIN DELETE FROM provider_keys WHERE id = NEW.id; END"
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("create AFTER UPDATE trigger failed: %v", err)
	}

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: new(provider.Keys[0].ManagementStatus),
	}, now)
	if err == nil {
		t.Fatalf("expected an error when the reload after a label-only edit fails")
	}
}

func TestUpdateProviderKeyErrorsWhenEncryptFailsOnPlaintextSwap(t *testing.T) {
	svc, db, _ := newTestProviderServiceWithInvalidMasterKey(t)
	now := time.Now().UTC()
	seededProvider := &model.Provider{
		Name: "p1", ProviderType: "openai", BaseURL: "https://a.example.com",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(seededProvider).Error; err != nil {
		t.Fatalf("seed provider failed: %v", err)
	}
	seededKey := &model.ProviderKey{
		ProviderID: seededProvider.ID, Label: "k1", EncryptedKey: "irrelevant", KeyPrefix: "x",
		TestModel: "gpt-4o-mini", SortOrder: 1, ManagementStatus: model.ProviderKeyStatusDisabled,
		VerificationStatus: model.VerificationStatusUntested, AuthorizedDestinationVersion: 1,
		ConfigVersion: 1, TestGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(seededKey).Error; err != nil {
		t.Fatalf("seed key failed: %v", err)
	}
	newPlaintext := "sk-newnewnewnewnewnewnewnewnewnew"

	_, err := svc.UpdateProviderKey(context.Background(), seededProvider.ID, seededKey.ID, UpdateKeyInput{
		Label: "k1", Plaintext: &newPlaintext, TestModel: "gpt-4o-mini",
	}, now)
	if err == nil {
		t.Fatalf("expected an error when the master key is an invalid AES key length")
	}
}

// TestUpdateProviderKeyErrorsWhenReloadFailsAfterPlaintextSwap forces the
// final FindProviderKeyByID (after SwapProviderKeyPlaintext + the
// server-side re-verify) to fail, using the fake client's side effect to
// delete the row mid-test-call.
func TestUpdateProviderKeyErrorsWhenReloadFailsAfterPlaintextSwap(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	keyID := provider.Keys[0].ID
	client.sideEffect = func() {
		if err := db.Exec("DELETE FROM provider_keys WHERE id = ?", keyID).Error; err != nil {
			t.Fatalf("simulated concurrent deletion failed: %v", err)
		}
	}
	newPlaintext := "sk-newnewnewnewnewnewnewnewnewnew"

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, keyID, UpdateKeyInput{
		Label: provider.Keys[0].Label, Plaintext: &newPlaintext, TestModel: provider.Keys[0].TestModel,
	}, now)
	if err == nil {
		t.Fatalf("expected an error when the key row vanishes before the final reload")
	}
}

func TestUpdateProviderKeyRejectsDuplicateLabelOnLabelOnlyEdit(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	k2, err := svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProviderKey failed: %v", err)
	}

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, k2.ID, UpdateKeyInput{Label: "k1", TestModel: k2.TestModel, ManagementStatus: new(k2.ManagementStatus)}, now)
	if !errors.Is(err, errcode.ErrProviderKeyLabelTaken) {
		t.Fatalf("expected ErrProviderKeyLabelTaken, got %v", err)
	}
}

func TestUpdateProviderKeyErrorsWhenLabelStatusUpdateFailsForNonUniqueReason(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	blockTableWrites(t, db, "provider_keys", "UPDATE")

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: new(provider.Keys[0].ManagementStatus),
	}, now)
	if err == nil || errors.Is(err, errcode.ErrProviderKeyLabelTaken) {
		t.Fatalf("expected a raw DB error (not ErrProviderKeyLabelTaken), got %v", err)
	}
}

func TestUpdateProviderKeyRejectsPlaintextShorterThan20CharsOnEdit(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	shortPlaintext := "too-short"

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: provider.Keys[0].Label, Plaintext: &shortPlaintext, TestModel: provider.Keys[0].TestModel,
	}, now)
	if err == nil {
		t.Fatalf("expected an error for a plaintext shorter than the minimum length")
	}
}

func TestUpdateProviderKeyRejectsDuplicateLabelWithNewPlaintext(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	k2, err := svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProviderKey failed: %v", err)
	}
	newPlaintext := "sk-newnewnewnewnewnewnewnewnewnew"

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, k2.ID, UpdateKeyInput{
		Label: "k1", Plaintext: &newPlaintext, TestModel: k2.TestModel, ManagementStatus: new(k2.ManagementStatus),
	}, now)
	if !errors.Is(err, errcode.ErrProviderKeyLabelTaken) {
		t.Fatalf("expected ErrProviderKeyLabelTaken, got %v", err)
	}
}

func TestUpdateProviderKeyErrorsWhenSwapPlaintextFailsForNonUniqueReason(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	blockTableWrites(t, db, "provider_keys", "UPDATE")
	newPlaintext := "sk-newnewnewnewnewnewnewnewnewnew"

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: provider.Keys[0].Label, Plaintext: &newPlaintext, TestModel: provider.Keys[0].TestModel,
	}, now)
	if err == nil || errors.Is(err, errcode.ErrProviderKeyLabelTaken) {
		t.Fatalf("expected a raw DB error (not ErrProviderKeyLabelTaken), got %v", err)
	}
}

func TestSetProviderKeyStatusRejectsEnablingUnverifiedKey(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestAuthFailed}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	// key's verification_status is now "failed" (client returned TestAuthFailed).

	if err := svc.SetProviderKeyStatus(provider.ID, provider.Keys[0].ID, true, now); !errors.Is(err, errcode.ErrProviderKeyNotVerified) {
		t.Fatalf("expected ErrProviderKeyNotVerified, got %v", err)
	}
}

func TestSetProviderKeyStatusAllowsEnablingVerifiedKey(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestSuccess}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	if err := svc.SetProviderKeyStatus(provider.ID, provider.Keys[0].ID, true, now); err != nil {
		t.Fatalf("expected enabling a passed key to succeed, got: %v", err)
	}
}

func TestSetProviderKeyStatusNotFoundWhenEnablingNonExistentKey(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	if err := svc.SetProviderKeyStatus(9999, 9999, true, time.Now().UTC()); !errors.Is(err, errcode.ErrProviderKeyNotFound) {
		t.Fatalf("expected ErrProviderKeyNotFound, got %v", err)
	}
}

func TestSetProviderKeyStatusErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "provider_keys")

	if err := svc.SetProviderKeyStatus(1, 1, true, time.Now().UTC()); err == nil || errors.Is(err, errcode.ErrProviderKeyNotFound) {
		t.Fatalf("expected a raw DB error (not ErrProviderKeyNotFound), got %v", err)
	}
}

func TestTestProviderKeyRejectsWhenNeedsReentry(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestSuccess}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://old.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.UpdateProvider(provider.ID, UpdateProviderInput{Name: provider.Name, BaseURL: "https://new.example.com"}, now); err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}

	if _, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now); !errors.Is(err, errcode.ErrProviderKeyNeedsReentry) {
		t.Fatalf("expected ErrProviderKeyNeedsReentry, got %v", err)
	}
}

func TestTestProviderKeyRetestsAndUpdatesStatus(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestAuthFailed}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	client.result = TestResult{Outcome: TestSuccess}
	view, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now)
	if err != nil {
		t.Fatalf("TestProviderKey failed: %v", err)
	}
	if view.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status=passed after retest succeeds, got %d", view.VerificationStatus)
	}
}

func TestTestProviderKeyNotFound(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	if _, err := svc.TestProviderKey(context.Background(), 9999, 9999, time.Now().UTC()); !errors.Is(err, errcode.ErrProviderKeyNotFound) {
		t.Fatalf("expected ErrProviderKeyNotFound, got %v", err)
	}
}

func TestTestProviderKeyErrorsWhenProviderLookupFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	dropTable(t, db, "providers")

	if _, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestTestProviderKeyErrorsWhenBeginRetestFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	blockTableWrites(t, db, "provider_keys", "UPDATE")

	if _, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now); err == nil {
		t.Fatalf("expected an error when BeginProviderKeyRetest's UPDATE fails")
	}
}

func TestTestProviderKeyErrorsWhenDecryptFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if err := db.Exec("UPDATE provider_keys SET encrypted_key = ? WHERE id = ?", "not-valid-ciphertext", provider.Keys[0].ID).Error; err != nil {
		t.Fatalf("corrupt encrypted_key failed: %v", err)
	}

	if _, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now); err == nil {
		t.Fatalf("expected an error when the stored ciphertext fails to decrypt")
	}
}

func TestTestProviderKeyErrorsWhenClientCallFails(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	client.err = fmt.Errorf("too many concurrent provider test calls in flight")

	if _, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now); err == nil {
		t.Fatalf("expected an error when the client itself refuses the call")
	}
}

func TestTestProviderKeyErrorsWhenKeyLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "provider_keys")

	if _, err := svc.TestProviderKey(context.Background(), 1, 1, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

// TestTestProviderKeyErrorsWhenReloadFailsAfterCommit forces the final
// FindProviderKeyByID (after CommitProviderKeyRetestResult) to fail, using
// the fake client's side effect to delete the row mid-test-call.
func TestTestProviderKeyErrorsWhenReloadFailsAfterCommit(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	keyID := provider.Keys[0].ID
	client.sideEffect = func() {
		if err := db.Exec("DELETE FROM provider_keys WHERE id = ?", keyID).Error; err != nil {
			t.Fatalf("simulated concurrent deletion failed: %v", err)
		}
	}

	if _, err := svc.TestProviderKey(context.Background(), provider.ID, keyID, now); err == nil {
		t.Fatalf("expected an error when the key row vanishes before the final reload")
	}
}

func TestTestAllProviderKeysNotFoundProvider(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	if _, err := svc.TestAllProviderKeys(context.Background(), 9999, time.Now().UTC()); !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestTestAllProviderKeysErrorsWhenProviderLookupFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "providers")

	if _, err := svc.TestAllProviderKeys(context.Background(), 1, time.Now().UTC()); err == nil || errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected a raw DB error (not ErrProviderNotFound), got %v", err)
	}
}

func TestTestAllProviderKeysErrorsWhenListKeysFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	dropTable(t, db, "provider_keys")

	if _, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

// A disabled key IS batch-tested: the design intent is that batch test
// verifies every !needs_reentry key (including a not-yet-enabled one) so an
// admin can test-then-enable a fresh key. The test only RECORDS the
// verification result — it must never auto-enable the key.
func TestTestAllProviderKeysTestsDisabledKeyButDoesNotEnable(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	// Seed the key as NOT-passed at creation so the post-batch passed
	// assertion actually proves the batch path recorded a result. If we left
	// the default zero-value TestSuccess in place, CreateProvider's own test
	// would already mark the key passed and the assertion below would hold
	// even if the batch path stopped writing anything at all.
	client.result = TestResult{Outcome: TestAuthFailed}
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now) // ManagementStatus defaults to disabled; auth-failed test leaves it disabled + verification=failed.
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess, DurationMs: 5}

	callsBefore := client.calls
	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if client.calls != callsBefore+1 {
		t.Fatalf("expected the disabled key to be network-tested, calls went from %d to %d", callsBefore, client.calls)
	}
	if len(results) != 1 || results[0].Skipped {
		t.Fatalf("expected 1 non-skipped result for the disabled key, got %+v", results)
	}
	if results[0].Outcome == nil || *results[0].Outcome != int(TestSuccess) {
		t.Fatalf("expected outcome=TestSuccess, got %+v", results[0])
	}

	var reloaded model.ProviderKey
	if err := db.Where("id = ?", provider.Keys[0].ID).First(&reloaded).Error; err != nil {
		t.Fatalf("reload key failed: %v", err)
	}
	// verification flipped failed -> passed proves the batch path wrote the
	// result; duration_ms=5 (only the batch run set that) corroborates it.
	if reloaded.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status=passed to be recorded by the batch test, got %d", reloaded.VerificationStatus)
	}
	if reloaded.LastTestDurationMs == nil || *reloaded.LastTestDurationMs != 5 {
		t.Fatalf("expected last_test_duration_ms=5 from the batch run, got %v", reloaded.LastTestDurationMs)
	}
	if reloaded.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected batch test to NOT auto-enable the key, management_status=%d", reloaded.ManagementStatus)
	}
}

// TestTestProviderKeyReturnsErrorWhenCommitLosesCASRace covers the single-key
// counterpart to TestTestAllProviderKeysSkipsWhenCommitLosesCASRace: when a
// concurrent config_version bump lands mid-test, the write-back CAS matches no
// row and the result is not persisted. Rather than reload+return a stale row
// (whose last_test_result the UI would present as this run's outcome), the
// service surfaces a retryable ErrProviderKeyTestNotSaved.
func TestTestProviderKeyReturnsErrorWhenCommitLosesCASRace(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	keyID := provider.Keys[0].ID
	client.result = TestResult{Outcome: TestSuccess, DurationMs: 5}
	client.sideEffect = func() {
		if err := db.Exec("UPDATE provider_keys SET config_version = config_version + 1 WHERE id = ?", keyID).Error; err != nil {
			t.Fatalf("simulated concurrent config_version bump failed: %v", err)
		}
	}

	view, err := svc.TestProviderKey(context.Background(), provider.ID, keyID, now)
	if !errors.Is(err, errcode.ErrProviderKeyTestNotSaved) {
		t.Fatalf("expected ErrProviderKeyTestNotSaved after losing the CAS race, got view=%+v err=%v", view, err)
	}
	if view != nil {
		t.Fatalf("expected no view when the result was not saved, got %+v", view)
	}
}

func TestTestAllProviderKeysSkipsWhenBeginRetestFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: new(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	blockTableWrites(t, db, "provider_keys", "UPDATE")

	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped || results[0].Outcome != nil {
		t.Fatalf("expected 1 skipped result with no outcome, got %+v", results)
	}
}

func TestTestAllProviderKeysSkipsWhenDecryptFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: new(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	if err := db.Exec("UPDATE provider_keys SET encrypted_key = ? WHERE id = ?", "not-valid-ciphertext", provider.Keys[0].ID).Error; err != nil {
		t.Fatalf("corrupt encrypted_key failed: %v", err)
	}

	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped || results[0].Outcome != nil {
		t.Fatalf("expected 1 skipped result with no outcome, got %+v", results)
	}
}

func TestTestAllProviderKeysSkipsWhenClientCallErrors(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: new(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	client.err = fmt.Errorf("too many concurrent provider test calls in flight")

	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped || results[0].Outcome != nil {
		t.Fatalf("expected 1 skipped result with no outcome, got %+v", results)
	}
}

// TestTestAllProviderKeysSkipsWhenCommitLosesCASRace exercises the "lost
// CAS race" discard path: the fake client's side effect
// simulates a concurrent plaintext edit (bumping config_version) landing
// WHILE this key's batch test is in flight, so the write-back CAS condition
// no longer matches by the time the commit runs and the result must be
// silently discarded rather than applied.
func TestTestAllProviderKeysSkipsWhenCommitLosesCASRace(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	keyID := provider.Keys[0].ID
	if _, err := svc.UpdateProviderKey(context.Background(), provider.ID, keyID, UpdateKeyInput{
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: new(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess, DurationMs: 5}
	client.sideEffect = func() {
		if err := db.Exec("UPDATE provider_keys SET config_version = config_version + 1 WHERE id = ?", keyID).Error; err != nil {
			t.Fatalf("simulated concurrent config_version bump failed: %v", err)
		}
	}

	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %+v", results)
	}
	if !results[0].Skipped {
		t.Fatalf("expected the result to be marked skipped after losing the CAS race, got %+v", results[0])
	}
	if results[0].Outcome == nil || *results[0].Outcome != int(TestSuccess) {
		t.Fatalf("expected the outcome to still be reported even though it was discarded, got %+v", results[0])
	}

	// The key's own row must NOT have been updated with the discarded
	// result's duration — CreateProvider's own initial re-verify already
	// wrote last_test_duration_ms=10 (newTestProviderService's default
	// fakeProviderClient result), so a batch test that actually applied
	// would have overwritten it with 5 (this test's client.result); if the
	// CAS correctly discarded the race instead, 10 must survive untouched.
	var reloaded model.ProviderKey
	if err := db.Where("id = ?", keyID).First(&reloaded).Error; err != nil {
		t.Fatalf("reload key failed: %v", err)
	}
	if reloaded.LastTestDurationMs == nil || *reloaded.LastTestDurationMs != 10 {
		t.Fatalf("expected the discarded batch result to never be written back (duration_ms should stay 10), got %+v", reloaded.LastTestDurationMs)
	}
}

func TestTestAllProviderKeysRecordsOutcomeOnSuccessfulRetest(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: new(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess, DurationMs: 7}

	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if len(results) != 1 || results[0].Skipped {
		t.Fatalf("expected 1 non-skipped result, got %+v", results)
	}
	if results[0].Outcome == nil || *results[0].Outcome != int(TestSuccess) {
		t.Fatalf("expected outcome=TestSuccess, got %+v", results[0])
	}
	if results[0].DurationMs != 7 {
		t.Fatalf("expected duration_ms=7, got %d", results[0].DurationMs)
	}

	var reloaded model.ProviderKey
	if err := db.Where("id = ?", provider.Keys[0].ID).First(&reloaded).Error; err != nil {
		t.Fatalf("reload key failed: %v", err)
	}
	if reloaded.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status=passed after the batch retest, got %d", reloaded.VerificationStatus)
	}
}

func TestTestAllProviderKeysSkipsNeedsReentryWithoutNetworkCall(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://old.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: new(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	if _, err := svc.UpdateProvider(provider.ID, UpdateProviderInput{Name: provider.Name, BaseURL: "https://new.example.com"}, now); err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}

	callsBefore := client.calls
	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if client.calls != callsBefore {
		t.Fatalf("expected zero network calls for a key needing re-entry, calls went from %d to %d", callsBefore, client.calls)
	}
	if len(results) != 1 || !results[0].NeedsReentry {
		t.Fatalf("expected 1 result flagged needs_reentry, got %+v", results)
	}
}

func TestTestKeyPreviewNeverPersists(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	client.result = TestResult{Outcome: TestSuccess}

	result, err := svc.TestKeyPreview(context.Background(), "https://a.example.com", "sk-preview-only", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestKeyPreview failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess, got %v", result.Outcome)
	}
	var count int64
	db.Model(&model.ProviderKey{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected TestKeyPreview to write nothing to the database, found %d rows", count)
	}
}

func TestListProvidersReturnsEmptySliceWhenNoProviders(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	views, err := svc.ListProviders()
	if err != nil {
		t.Fatalf("ListProviders failed: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("expected an empty slice, got %+v", views)
	}
}

func TestListProvidersReturnsEveryProviderWithItsKeys(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	if _, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now); err != nil {
		t.Fatalf("CreateProvider p1 failed: %v", err)
	}
	if _, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p2", BaseURL: "https://b.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now); err != nil {
		t.Fatalf("CreateProvider p2 failed: %v", err)
	}

	views, err := svc.ListProviders()
	if err != nil {
		t.Fatalf("ListProviders failed: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(views))
	}
	for _, v := range views {
		if len(v.Keys) != 1 {
			t.Fatalf("expected each provider to carry its 1 key, got %+v", v)
		}
	}
}

func TestListProvidersErrorsWhenProvidersTableMissing(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "providers")

	if _, err := svc.ListProviders(); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestListProvidersErrorsWhenProviderKeysTableMissing(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	if _, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now); err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	dropTable(t, db, "provider_keys")

	if _, err := svc.ListProviders(); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestGetProviderDetailNotFound(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	if _, err := svc.GetProviderDetail(9999); !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestGetProviderDetailErrorsWhenProvidersTableMissing(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "providers")

	_, err := svc.GetProviderDetail(1)
	if err == nil || errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected a raw DB error (not ErrProviderNotFound), got %v", err)
	}
}

func TestGetProviderDetailErrorsWhenProviderKeysTableMissing(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	dropTable(t, db, "provider_keys")

	if _, err := svc.GetProviderDetail(provider.ID); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestCreateProviderErrorsWhenNameLookupFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	dropTable(t, db, "providers")

	_, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err == nil || errors.Is(err, errcode.ErrProviderNameTaken) {
		t.Fatalf("expected a raw DB error (not ErrProviderNameTaken), got %v", err)
	}
}

// TestCreateProviderConcurrentSameNameHitsUniqueViolationBranch documents
// (and guards, at the outcome level) the TOCTOU race isUniqueViolation's
// call site inside CreateProvider exists to catch: two goroutines could
// both pass the up-front FindProviderByName check before either commits its
// insert, in which case the real backstop is the UNIQUE constraint
// surfacing through CreateProviderWithKey. It does NOT reliably drive
// coverage of that exact call site, though: pkg/database.Init caps the
// sqlite test connection pool at 1 (SetMaxOpenConns(1)), which in practice
// serializes essentially every attempt behind the single connection — every
// loser ends up observing the winner's row via the ordinary
// FindProviderByName check instead of racing into CreateProviderWithKey.
// Confirmed empirically: 32 concurrent goroutines behind a start barrier,
// 5 runs, 0/5 hit the race branch. The invariant this test asserts
// (exactly 1 winner, every loser gets ErrProviderNameTaken) holds either
// way, so the test still has value — see the coverage report for how this
// specific line is accounted for.
func TestCreateProviderConcurrentSameNameHitsUniqueViolationBranch(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()

	// A larger attempt count plus a start barrier (every goroutine blocks
	// on `start` until all are launched and release simultaneously)
	// maximizes the chance of two goroutines' FindProviderByName reads
	// landing in the same window before either's CreateProviderWithKey
	// commits — the database connection pool is capped at 1 (sqlite,
	// pkg/database.Init), so goroutines interleave at successive
	// acquire/release boundaries of that single connection rather than
	// running fully in lockstep.
	const attempts = 32
	start := make(chan struct{})
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			<-start
			_, err := svc.CreateProvider(context.Background(), CreateProviderInput{
				Name: "race-provider", BaseURL: "https://a.example.com", KeyLabel: fmt.Sprintf("k%d", i), KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
			}, now)
			results <- err
		}()
	}
	close(start)

	succeeded, nameTaken := 0, 0
	for i := 0; i < attempts; i++ {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, errcode.ErrProviderNameTaken):
			nameTaken++
		default:
			t.Fatalf("unexpected error from concurrent CreateProvider: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 successful CreateProvider out of %d concurrent attempts, got %d", attempts, succeeded)
	}
	if nameTaken != attempts-1 {
		t.Fatalf("expected the other %d attempts to see ErrProviderNameTaken, got %d", attempts-1, nameTaken)
	}
}

func TestIsUniqueViolationDetectsKnownDatabaseMessages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"sqlite", errors.New("UNIQUE constraint failed: providers.name"), true},
		{"postgres", errors.New(`duplicate key value violates unique constraint "providers_name_key"`), true},
		{"unrelated", errors.New("some other database error"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUniqueViolation(c.err); got != c.want {
				t.Fatalf("isUniqueViolation(%q) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestClassifyTestResultCoversEveryOutcome(t *testing.T) {
	outcomeInt := func(o TestOutcome) *int { v := int(o); return &v }

	cases := []struct {
		name              string
		result            TestResult
		wantVerification  int
		wantOverwrite     bool
		wantLastTestValue *int
	}{
		{"success", TestResult{Outcome: TestSuccess}, model.VerificationStatusPassed, true, outcomeInt(TestSuccess)},
		{"auth failed", TestResult{Outcome: TestAuthFailed}, model.VerificationStatusFailed, true, outcomeInt(TestAuthFailed)},
		{"quota unavailable", TestResult{Outcome: TestQuotaUnavailable}, model.VerificationStatusFailed, true, outcomeInt(TestQuotaUnavailable)},
		{"permission denied not model scoped", TestResult{Outcome: TestPermissionDenied, IsModelScoped: false}, model.VerificationStatusFailed, true, outcomeInt(TestPermissionDenied)},
		{"permission denied model scoped", TestResult{Outcome: TestPermissionDenied, IsModelScoped: true}, 0, false, outcomeInt(TestPermissionDenied)},
		{"model not found", TestResult{Outcome: TestModelNotFound}, 0, false, outcomeInt(TestModelNotFound)},
		{"rate limited", TestResult{Outcome: TestRateLimited}, 0, false, outcomeInt(TestRateLimited)},
		{"unreachable", TestResult{Outcome: TestUnreachable}, 0, false, outcomeInt(TestUnreachable)},
		{"upstream error", TestResult{Outcome: TestUpstreamError}, 0, false, outcomeInt(TestUpstreamError)},
		{"unknown outcome falls to default", TestResult{Outcome: TestOutcome(999)}, 0, false, outcomeInt(TestOutcome(999))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotVerification, gotOverwrite, gotLastTestResult := classifyTestResult(c.result)
			if gotVerification != c.wantVerification || gotOverwrite != c.wantOverwrite {
				t.Fatalf("classifyTestResult(%+v) = (%d, %v, _), want (%d, %v, _)",
					c.result, gotVerification, gotOverwrite, c.wantVerification, c.wantOverwrite)
			}
			if gotLastTestResult == nil || *gotLastTestResult != *c.wantLastTestValue {
				t.Fatalf("classifyTestResult(%+v) last_test_result = %v, want %v", c.result, gotLastTestResult, *c.wantLastTestValue)
			}
		})
	}
}

func TestReorderProviderKeySwapsOrder(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini",
	}, now); err != nil {
		t.Fatalf("CreateProviderKey failed: %v", err)
	}

	detail, err := svc.GetProviderDetail(provider.ID)
	if err != nil {
		t.Fatalf("GetProviderDetail failed: %v", err)
	}
	secondKeyID := detail.Keys[1].ID

	if err := svc.ReorderProviderKey(provider.ID, secondKeyID, "up", now); err != nil {
		t.Fatalf("ReorderProviderKey failed: %v", err)
	}
	reloaded, err := svc.GetProviderDetail(provider.ID)
	if err != nil {
		t.Fatalf("GetProviderDetail failed: %v", err)
	}
	if reloaded.Keys[0].ID != secondKeyID {
		t.Fatalf("expected the second key to now sort first, got %+v", reloaded.Keys)
	}
}

// TestReorderProviderKeyReturnsNotFoundForUnknownKey is the direct
// regression test for a bug: this used to
// return SwapProviderKeySortOrder's raw gorm.ErrRecordNotFound
// untranslated, unlike UpdateProviderKey/SetProviderKeyStatus/
// TestProviderKey in this same file, which all map the identical
// unknown/cross-provider condition to errcode.ErrProviderKeyNotFound.
func TestReorderProviderKeyReturnsNotFoundForUnknownKey(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	if err := svc.ReorderProviderKey(provider.ID, 999999, "up", now); !errors.Is(err, errcode.ErrProviderKeyNotFound) {
		t.Fatalf("expected ErrProviderKeyNotFound, got %v", err)
	}
}
