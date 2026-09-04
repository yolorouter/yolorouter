package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/service/apikey"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/service/providerclient/providerclienttest"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
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

// providerclienttest.Fake never makes a real network call — tests configure
// outcomes per call. sideEffect, when set, runs synchronously before the
// call returns — used to simulate a concurrent DB write racing against an
// in-flight test call (e.g. a plaintext swap bumping config_version while
// TestAllProviderKeys is mid-test for that key), so the write-back CAS then
// observes a stale snapshot and discards the result. perTarget, when set,
// overrides result/err for a specific (proto, baseURL) pair — used by tests
// exercising verifyKeyAllDestinations against more than one destination;
// falls back to result/err for any (proto, baseURL) not present in the map.
// strptr returns a pointer to s, for populating UpdateProviderInput's
// *string fields (ProviderType/ProtocolEndpoints) where a non-nil pointer —
// even to an empty string — is the "field present, apply it" signal, as
// opposed to nil ("field absent, leave unchanged").
func strptr(s string) *string {
	return &s
}

func newTestProviderService(t *testing.T) (*ProviderService, *gorm.DB, *providerclienttest.Fake) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	client := &providerclienttest.Fake{Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 10}}
	svc := NewProviderService(db, testutil.ProviderSecrets(), client)
	return svc, db, client
}

// seedEnabledProviderForModelTest creates an enabled provider with one
// verified key through the public CreateProvider path — the same seeding the
// model-admin test suite uses (each suite carries its own copy: the helper
// drives only exported API, and neither package's tests can import the
// other's without an import cycle).
func seedEnabledProviderForModelTest(t *testing.T, providerService *ProviderService, name string) *ProviderView {
	t.Helper()
	provider, err := providerService.CreateProvider(context.Background(), CreateProviderInput{
		Name: name, BaseURL: "https://" + name + ".example.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	return provider
}

// newTestProviderServiceWithInvalidMasterKey builds a service whose
// masterKey is a length aes.NewCipher rejects (anything but 16/24/32
// bytes) — the only reliable way to force crypto.Encrypt to fail from a
// black-box test, exercising the service layer's encErr branches.
func newTestProviderServiceWithInvalidMasterKey(t *testing.T) (*ProviderService, *gorm.DB, *providerclienttest.Fake) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	client := &providerclienttest.Fake{Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 10}}
	svc := NewProviderService(db, crypto.NewSecretBox([]byte("too-short-for-aes")), client)
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
	svcWithDifferentKey := NewProviderService(db, crypto.NewSecretBox(otherKey), &providerclienttest.Fake{})
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
	testutil.DropTable(t, db, "provider_key_fingerprint")

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
		{"shorter than the prefix threshold stores nothing", "abc", ""},
		{"one below the threshold (19 runes) stores nothing", "abcdefghij012345678", ""},
		{"exactly at the threshold (20 runes) uses the first 10", "abcdefghij0123456789", "abcdefghij"},
		{"caps at 10 chars for long plaintext", "sk-abcdefghijklmnopqrstuvwxyz1234", "sk-abcdefg"},
		// Regression: the original byte-sliced
		// implementation could cut a multi-byte UTF-8 character in half if
		// one straddled the cutoff, producing invalid UTF-8. These multi-byte
		// runes (é = 2 bytes, 中 = 3 bytes) fall within the first 10 runes of a
		// plaintext long enough to store a prefix; a rune-safe implementation
		// must return valid UTF-8 either way.
		{"multi-byte rune within the first 10 runes stays valid UTF-8", "café-abcdefghijklmnopqrstuvwxyz1234", "café-abcde"},
		{"multi-byte rune near the 10-rune cap stays valid UTF-8", "sk-中国12345678901234ab", "sk-中国12345"},
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

func TestCreateProviderRejectsPlaintextShorterThanMinimum(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	_, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "short-key", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "short", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected an error for a plaintext shorter than the minimum length")
	}
}

func TestCreateProviderDefaultsProviderTypeToOpenAIWhenOmitted(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	view, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "no-type", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if view.ProviderType != "openai" {
		t.Fatalf("expected default provider_type openai for backward compatibility, got %q", view.ProviderType)
	}
}

func TestCreateProviderAcceptsExplicitProviderTypeAndEchoesItInView(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	view, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "anthropic-main", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "claude-3",
		ProviderType: "anthropic",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if view.ProviderType != "anthropic" {
		t.Fatalf("expected provider_type anthropic, got %q", view.ProviderType)
	}
}

func TestCreateProviderEchoesProtocolEndpointsInView(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	view, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "with-endpoints", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ProtocolEndpoints: `{"responses":"https://gw.example.com/v1"}`,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if view.ProtocolEndpoints != `{"responses":"https://gw.example.com/v1"}` {
		t.Fatalf("expected protocol_endpoints to round-trip, got %q", view.ProtocolEndpoints)
	}
}

func TestCreateProviderRejectsInvalidProviderType(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	_, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "bad-type", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ProviderType: "claude",
	}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrProviderProtocolInvalid) {
		t.Fatalf("expected ErrProviderProtocolInvalid for an unsupported provider_type, got %v", err)
	}
}

func TestCreateProviderRejectsMalformedProtocolEndpoints(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	_, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "bad-endpoints", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ProtocolEndpoints: `{not-json`,
	}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrProviderProtocolInvalid) {
		t.Fatalf("expected ErrProviderProtocolInvalid for malformed protocol_endpoints JSON, got %v", err)
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
	testutil.DropTable(t, db, "providers")

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
	testutil.BlockTableWrites(t, db, "providers", "UPDATE")

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
	testutil.BlockTableWrites(t, db, "providers", "UPDATE")

	// Same BaseURL — skips UpdateProviderBaseURL entirely, isolating the
	// UpdateProviderNameNote failure.
	_, err = svc.UpdateProvider(provider.ID, UpdateProviderInput{Name: "renamed", Note: strptr("n"), BaseURL: provider.BaseURL}, now)
	if err == nil || errors.Is(err, errcode.ErrProviderNameTaken) {
		t.Fatalf("expected a raw DB error (not ErrProviderNameTaken), got %v", err)
	}
}

// TestUpdateProviderNameOnlyPatchDoesNotChangeProviderTypeOrBumpDestinationVersion
// is the direct regression test for the PATCH-omitted-field edge case: an
// empty ProviderType/ProtocolEndpoints in an UpdateProviderInput means "not
// supplied in this request, leave unchanged" — NOT "reset to the create
// path's empty-means-openai default". A name-only PATCH on an anthropic
// provider must leave provider_type == "anthropic" and must not bump
// destination_version (which would spuriously invalidate every existing
// key's authorization for an edit that never touched the protocol/address).
func TestUpdateProviderNameOnlyPatchDoesNotChangeProviderTypeOrBumpDestinationVersion(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	created, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234",
		TestModel: "gpt-4o-mini", ProviderType: "anthropic",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if created.ProviderType != "anthropic" {
		t.Fatalf("expected provider_type=anthropic after create, got %q", created.ProviderType)
	}

	// PATCH omits provider_type/protocol_endpoints entirely (name-only edit).
	updated, err := svc.UpdateProvider(created.ID, UpdateProviderInput{Name: "renamed", BaseURL: created.BaseURL}, now)
	if err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}
	if updated.ProviderType != "anthropic" {
		t.Fatalf("expected provider_type to remain anthropic after a name-only PATCH, got %q", updated.ProviderType)
	}
	if updated.Keys[0].NeedsReentry {
		t.Fatalf("expected the existing key to NOT need re-entry after a name-only PATCH (destination_version must not bump), got needs_reentry=true")
	}
}

// TestUpdateProviderProtocolChangeBumpsDestinationVersionAndPersists is the
// direct test for this task's core requirement: a PATCH that actually
// changes provider_type/protocol_endpoints must write both columns AND bump
// destination_version in the same atomic UPDATE, so an existing key's
// authorized_destination_version immediately mismatches (needs re-entry) —
// the same invariant a base_url change already enforces.
func TestUpdateProviderProtocolChangeBumpsDestinationVersionAndPersists(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	created, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234",
		TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if created.ProviderType != "openai" {
		t.Fatalf("expected default provider_type=openai, got %q", created.ProviderType)
	}

	updated, err := svc.UpdateProvider(created.ID, UpdateProviderInput{
		Name: created.Name, BaseURL: created.BaseURL,
		ProviderType: strptr("anthropic"), ProtocolEndpoints: strptr(`{"responses":"https://gw/v1"}`),
	}, now)
	if err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}
	if updated.ProviderType != "anthropic" || updated.ProtocolEndpoints != `{"responses":"https://gw/v1"}` {
		t.Fatalf("expected provider_type/protocol_endpoints updated, got provider_type=%q protocol_endpoints=%q",
			updated.ProviderType, updated.ProtocolEndpoints)
	}
	if !updated.Keys[0].NeedsReentry {
		t.Fatalf("expected the existing key to need re-entry after a protocol-changing PATCH, got needs_reentry=false")
	}
}

// TestUpdateProviderProtocolEndpointsSemanticReSubmitDoesNotBumpVersion
// proves the compare uses the NORMALIZED value: re-submitting a
// semantically-identical protocol_endpoints JSON object (same keys/values,
// different key order) must not count as a change, so it never bumps
// destination_version.
func TestUpdateProviderProtocolEndpointsSemanticReSubmitDoesNotBumpVersion(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	created, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234",
		TestModel: "gpt-4o-mini", ProtocolEndpoints: `{"anthropic":"https://gw/a","responses":"https://gw/r"}`,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	updated, err := svc.UpdateProvider(created.ID, UpdateProviderInput{
		Name: created.Name, BaseURL: created.BaseURL,
		ProtocolEndpoints: strptr(`{"responses":"https://gw/r","anthropic":"https://gw/a"}`),
	}, now)
	if err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}
	if updated.Keys[0].NeedsReentry {
		t.Fatalf("expected a semantically-identical protocol_endpoints re-submit to NOT bump destination_version, got needs_reentry=true")
	}
}

// TestUpdateProviderRejectsInvalidProviderType proves PATCH validation
// reuses the same error surface as CreateProvider (ErrProviderProtocolInvalid),
// not a generic 500, for a bad provider_type value.
func TestUpdateProviderRejectsInvalidProviderType(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	created, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	_, err = svc.UpdateProvider(created.ID, UpdateProviderInput{
		Name: created.Name, BaseURL: created.BaseURL, ProviderType: strptr("not-a-real-protocol"),
	}, now)
	if !errors.Is(err, errcode.ErrProviderProtocolInvalid) {
		t.Fatalf("expected ErrProviderProtocolInvalid, got %v", err)
	}
}

// TestUpdateProviderClearingLastEndpointRemovesItAndBumpsVersion is the
// regression test for finding 1(a): the edit UI always sends
// protocol_endpoints, and disabling the last extra endpoint must send an
// authoritative empty string that actually clears the stored value — not a
// value silently ignored because ProtocolEndpoints used to be a plain string
// indistinguishable from "not supplied".
func TestUpdateProviderClearingLastEndpointRemovesItAndBumpsVersion(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	created, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234",
		TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderStatusEnabled,
		ProviderType: "openai", ProtocolEndpoints: `{"anthropic":""}`,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if created.ProtocolEndpoints != `{"anthropic":""}` {
		t.Fatalf("expected seeded protocol_endpoints, got %q", created.ProtocolEndpoints)
	}
	// The freshly-created, server-verified key must start authorized against
	// the current destination (needs_reentry=false) so the test can prove the
	// PATCH below is what flips it.
	if created.Keys[0].NeedsReentry {
		t.Fatalf("expected the newly-created key to be authorized against the initial destination, got needs_reentry=true")
	}

	updated, err := svc.UpdateProvider(created.ID, UpdateProviderInput{
		Name: created.Name, BaseURL: created.BaseURL,
		ProviderType: strptr("openai"), ProtocolEndpoints: strptr(""),
	}, now)
	if err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}
	if updated.ProtocolEndpoints != "" {
		t.Fatalf("expected protocol_endpoints cleared to empty, got %q", updated.ProtocolEndpoints)
	}
	if !updated.Keys[0].NeedsReentry {
		t.Fatalf("expected clearing the last endpoint to bump destination_version and require key re-entry, got needs_reentry=false")
	}

	var stored model.Provider
	if err := db.Where("id = ?", created.ID).First(&stored).Error; err != nil {
		t.Fatalf("reload provider failed: %v", err)
	}
	if stored.ProtocolEndpoints != "" {
		t.Fatalf("expected protocol_endpoints persisted as empty in storage, got %q", stored.ProtocolEndpoints)
	}
}

// TestUpdateProviderSwitchingPrimaryToSoleExtraEndpointClearsStaleOverride
// is the regression test for finding 1(b): switching the primary protocol
// to what used to be the sole extra endpoint (the edit UI auto-unchecks
// that now-primary entry and serializes protocol_endpoints to "") must not
// leave the OLD {"<newprimary>":"<url>"} entry behind — negotiate's
// egressBaseURL would otherwise keep using that stale URL to override the
// new primary's base_url instead of routing to base_url directly.
func TestUpdateProviderSwitchingPrimaryToSoleExtraEndpointClearsStaleOverride(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	created, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234",
		TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderStatusEnabled,
		ProviderType: "openai", ProtocolEndpoints: `{"anthropic":"https://old.example.com"}`,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	updated, err := svc.UpdateProvider(created.ID, UpdateProviderInput{
		Name: created.Name, BaseURL: created.BaseURL,
		ProviderType: strptr("anthropic"), ProtocolEndpoints: strptr(""),
	}, now)
	if err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}
	if updated.ProviderType != "anthropic" {
		t.Fatalf("expected provider_type=anthropic, got %q", updated.ProviderType)
	}
	if updated.ProtocolEndpoints != "" {
		t.Fatalf("expected protocol_endpoints cleared (no stale anthropic override), got %q", updated.ProtocolEndpoints)
	}
	if !updated.Keys[0].NeedsReentry {
		t.Fatalf("expected switching the primary protocol to bump destination_version and require key re-entry, got needs_reentry=false")
	}

	var stored model.Provider
	if err := db.Where("id = ?", created.ID).First(&stored).Error; err != nil {
		t.Fatalf("reload provider failed: %v", err)
	}
	if stored.ProviderType != "anthropic" || stored.ProtocolEndpoints != "" {
		t.Fatalf("expected stored provider_type=anthropic, protocol_endpoints=\"\" (no stale override), got provider_type=%q protocol_endpoints=%q",
			stored.ProviderType, stored.ProtocolEndpoints)
	}
}

// TestUpdateProviderNoteOmittedPreservedPresentEmptyClears is the regression
// test for the Note clobber gap: Note is a *string with the same nil-vs-present
// semantics as ProviderType/ProtocolEndpoints — an omitted note (nil) must
// leave the stored note untouched (a name/protocol edit must not silently wipe
// it), while a present note (including empty) is authoritative and clears it.
func TestUpdateProviderNoteOmittedPreservedPresentEmptyClears(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	created, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234",
		TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderStatusEnabled, Note: "keep me",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	// PATCH omits note (nil) — a name-only edit must preserve the stored note.
	if _, err := svc.UpdateProvider(created.ID, UpdateProviderInput{Name: "renamed", BaseURL: created.BaseURL}, now); err != nil {
		t.Fatalf("UpdateProvider (note omitted) failed: %v", err)
	}
	var afterOmit model.Provider
	if err := db.Where("id = ?", created.ID).First(&afterOmit).Error; err != nil {
		t.Fatalf("reload provider failed: %v", err)
	}
	if afterOmit.Note != "keep me" {
		t.Fatalf("expected an omitted note to be preserved, got %q", afterOmit.Note)
	}

	// PATCH with a present-empty note explicitly clears it.
	if _, err := svc.UpdateProvider(created.ID, UpdateProviderInput{Name: "renamed", BaseURL: created.BaseURL, Note: strptr("")}, now); err != nil {
		t.Fatalf("UpdateProvider (note cleared) failed: %v", err)
	}
	var afterClear model.Provider
	if err := db.Where("id = ?", created.ID).First(&afterClear).Error; err != nil {
		t.Fatalf("reload provider failed: %v", err)
	}
	if afterClear.Note != "" {
		t.Fatalf("expected a present-empty note to clear it, got %q", afterClear.Note)
	}
}

// TestUpdateProviderNameOnlyPatchLeavesProtocolUnchanged proves the nil-vs-
// present distinction: a PATCH that omits ProviderType/ProtocolEndpoints
// (nil, not empty-string) must leave an existing non-default protocol
// configuration untouched and must NOT bump destination_version — otherwise
// every plain name/note edit through the UI would spuriously invalidate
// every key's authorization.
func TestUpdateProviderNameOnlyPatchLeavesProtocolUnchanged(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	now := time.Now().UTC()
	created, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234",
		TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderStatusEnabled,
		ProviderType: "anthropic", ProtocolEndpoints: `{"responses":"https://gw.example.com/v1"}`,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	updated, err := svc.UpdateProvider(created.ID, UpdateProviderInput{
		Name: "renamed", Note: strptr("updated note"), BaseURL: created.BaseURL,
		// ProviderType/ProtocolEndpoints intentionally omitted (nil).
	}, now)
	if err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}
	if updated.Name != "renamed" {
		t.Fatalf("expected name updated, got %q", updated.Name)
	}
	if updated.ProviderType != "anthropic" || updated.ProtocolEndpoints != `{"responses":"https://gw.example.com/v1"}` {
		t.Fatalf("expected provider_type/protocol_endpoints unchanged by a name-only PATCH, got provider_type=%q protocol_endpoints=%q",
			updated.ProviderType, updated.ProtocolEndpoints)
	}
	if updated.Keys[0].NeedsReentry {
		t.Fatalf("expected a name-only PATCH to NOT bump destination_version, got needs_reentry=true")
	}

	var stored model.Provider
	if err := db.Where("id = ?", created.ID).First(&stored).Error; err != nil {
		t.Fatalf("reload provider failed: %v", err)
	}
	if stored.ProviderType != "anthropic" || stored.ProtocolEndpoints != `{"responses":"https://gw.example.com/v1"}` {
		t.Fatalf("expected stored provider_type/protocol_endpoints unchanged, got provider_type=%q protocol_endpoints=%q",
			stored.ProviderType, stored.ProtocolEndpoints)
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
	testutil.BlockTableWrites(t, db, "providers", "UPDATE")

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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestAuthFailed, DurationMs: 5}
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

func TestCreateProviderKeyRejectsPlaintextShorterThanMinimum(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	_, err = svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "short", TestModel: "gpt-4o-mini",
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
	testutil.DropTable(t, db, "provider_keys")

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
	testutil.DropTable(t, db, "providers")

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
// IsUniqueViolation branch a few lines below (a label-uniqueness TOCTOU
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
	testutil.BlockTableWrites(t, db, "provider_keys", "INSERT")

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
// classified as a passing test (providerclient.TestSuccess is providerclient.TestOutcome's zero value) —
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
	client.Err = fmt.Errorf("too many concurrent provider test calls in flight")

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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
	client.SideEffect = func() {
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
	client.SideEffect = func() {
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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}

	newPlaintext := "sk-newnewnewnewnewnewnewnewnewnew"
	updated, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: provider.Keys[0].Label, Plaintext: &newPlaintext, TestModel: "gpt-4o-mini", ManagementStatus: testutil.Ptr(model.ProviderKeyStatusEnabled),
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
	callsBefore := client.Calls

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(provider.Keys[0].ManagementStatus),
	}, now)
	if err != nil {
		t.Fatalf("UpdateProviderKey failed: %v", err)
	}
	if client.Calls != callsBefore {
		t.Fatalf("expected a label-only edit to trigger no network test, calls went from %d to %d", callsBefore, client.Calls)
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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestAuthFailed}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	// key's verification_status is now "failed" (client returned providerclient.TestAuthFailed).

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(model.ProviderKeyStatusEnabled),
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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
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
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(model.ProviderKeyStatusEnabled),
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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
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
		Label: "renamed", TestModel: providerB.Keys[0].TestModel, ManagementStatus: testutil.Ptr(providerB.Keys[0].ManagementStatus),
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
	testutil.DropTable(t, db, "providers")

	if _, err := svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{Label: "renamed"}, now); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestUpdateProviderKeyErrorsWhenKeyLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	testutil.DropTable(t, db, "provider_keys")

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
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(provider.Keys[0].ManagementStatus),
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
	client.SideEffect = func() {
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

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, k2.ID, UpdateKeyInput{Label: "k1", TestModel: k2.TestModel, ManagementStatus: testutil.Ptr(k2.ManagementStatus)}, now)
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
	testutil.BlockTableWrites(t, db, "provider_keys", "UPDATE")

	_, err = svc.UpdateProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, UpdateKeyInput{
		Label: "renamed", TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(provider.Keys[0].ManagementStatus),
	}, now)
	if err == nil || errors.Is(err, errcode.ErrProviderKeyLabelTaken) {
		t.Fatalf("expected a raw DB error (not ErrProviderKeyLabelTaken), got %v", err)
	}
}

func TestUpdateProviderKeyRejectsPlaintextShorterThanMinimumOnEdit(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	shortPlaintext := "short"

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
		Label: "k1", Plaintext: &newPlaintext, TestModel: k2.TestModel, ManagementStatus: testutil.Ptr(k2.ManagementStatus),
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
	testutil.BlockTableWrites(t, db, "provider_keys", "UPDATE")
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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestAuthFailed}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	// key's verification_status is now "failed" (client returned providerclient.TestAuthFailed).

	if err := svc.SetProviderKeyStatus(provider.ID, provider.Keys[0].ID, true, now); !errors.Is(err, errcode.ErrProviderKeyNotVerified) {
		t.Fatalf("expected ErrProviderKeyNotVerified, got %v", err)
	}
}

func TestSetProviderKeyStatusAllowsEnablingVerifiedKey(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
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
	testutil.DropTable(t, db, "provider_keys")

	if err := svc.SetProviderKeyStatus(1, 1, true, time.Now().UTC()); err == nil || errors.Is(err, errcode.ErrProviderKeyNotFound) {
		t.Fatalf("expected a raw DB error (not ErrProviderKeyNotFound), got %v", err)
	}
}

func TestTestProviderKeyRejectsWhenNeedsReentry(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestAuthFailed}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	view, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now)
	if err != nil {
		t.Fatalf("TestProviderKey failed: %v", err)
	}
	if view.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status=passed after retest succeeds, got %d", view.VerificationStatus)
	}
}

// The proven-recovery listener fires exactly when a committed retest
// overwrote verification to passed — and stays silent for an inconclusive
// probe, which proves nothing about the key.
func TestKeyRetestPassedListenerFiresOnProofOnly(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	keyID := provider.Keys[0].ID

	var fired []uint
	var observedAt time.Time
	svc.SetKeyRetestPassedListener(func(id uint, configVersion int, observed time.Time) {
		fired = append(fired, id)
		observedAt = observed
	})

	// Inconclusive probe: a rate-limited retest proves nothing — no signal.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestRateLimited}
	if _, err := svc.TestProviderKey(context.Background(), provider.ID, keyID, now); err != nil {
		t.Fatalf("TestProviderKey (rate limited) failed: %v", err)
	}
	if len(fired) != 0 {
		t.Fatalf("listener fired on an inconclusive retest: %v", fired)
	}

	// Passed probe: verification overwritten to passed — proof, one signal.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if _, err := svc.TestProviderKey(context.Background(), provider.ID, keyID, now); err != nil {
		t.Fatalf("TestProviderKey (success) failed: %v", err)
	}
	if len(fired) != 1 || fired[0] != keyID {
		t.Fatalf("listener fired %v, want exactly one call for key %d", fired, keyID)
	}
	if observedAt.IsZero() || observedAt.After(time.Now().UTC()) {
		t.Fatalf("observedAt = %v, want a real pre-commit stamp", observedAt)
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
	testutil.DropTable(t, db, "providers")

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
	testutil.BlockTableWrites(t, db, "provider_keys", "UPDATE")

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
	client.Err = fmt.Errorf("too many concurrent provider test calls in flight")

	if _, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now); err == nil {
		t.Fatalf("expected an error when the client itself refuses the call")
	}
}

func TestTestProviderKeyErrorsWhenKeyLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	testutil.DropTable(t, db, "provider_keys")

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
	client.SideEffect = func() {
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
	testutil.DropTable(t, db, "providers")

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
	testutil.DropTable(t, db, "provider_keys")

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
	// the default zero-value providerclient.TestSuccess in place, CreateProvider's own test
	// would already mark the key passed and the assertion below would hold
	// even if the batch path stopped writing anything at all.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestAuthFailed}
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now) // ManagementStatus defaults to disabled; auth-failed test leaves it disabled + verification=failed.
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}

	callsBefore := client.Calls
	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if client.Calls != callsBefore+1 {
		t.Fatalf("expected the disabled key to be network-tested, calls went from %d to %d", callsBefore, client.Calls)
	}
	if len(results) != 1 || results[0].Skipped {
		t.Fatalf("expected 1 non-skipped result for the disabled key, got %+v", results)
	}
	if results[0].Outcome == nil || *results[0].Outcome != int(providerclient.TestSuccess) {
		t.Fatalf("expected outcome=providerclient.TestSuccess, got %+v", results[0])
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
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
	client.SideEffect = func() {
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
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "provider_keys", "UPDATE")

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
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(model.ProviderKeyStatusEnabled),
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

// A batch test must stop at a budget of its own rather than run for
// keys × destinations × the per-probe cap. That product has no upper bound, and
// once it passes the server's own WriteTimeout the handler still finishes its
// writes but can no longer emit a response — results land in the database while
// the browser is told the request failed. Keys the budget never reached must
// come back as not-run: reporting them as a failed test would be a verdict on a
// credential nothing ever tried.
func TestTestAllProviderKeysStopsAtBudgetAndReportsNotRun(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if _, err := svc.CreateProviderKey(context.Background(), provider.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzz9", TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderKeyStatusEnabled,
	}, now); err != nil {
		t.Fatalf("CreateProviderKey failed: %v", err)
	}

	// Snapshot every key's pre-run bookkeeping. test_generation is the telling
	// one: BeginProviderKeyRetest bumps it the moment a key is claimed for
	// testing, so an unchanged value proves the key was never even started.
	generationBefore := map[uint]int{}
	var before []model.ProviderKey
	if err := db.Find(&before).Error; err != nil {
		t.Fatalf("load keys: %v", err)
	}
	for _, k := range before {
		generationBefore[k.ID] = k.TestGeneration
	}

	originalBudget := providerBatchTestBudget
	providerBatchTestBudget = 30 * time.Millisecond
	t.Cleanup(func() { providerBatchTestBudget = originalBudget })
	client.SideEffect = func() { time.Sleep(60 * time.Millisecond) }

	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected a result row per key, got %d", len(results))
	}

	last := results[len(results)-1]
	if !last.NotRun {
		t.Fatalf("the key the budget never reached must be reported as not-run, got %+v", last)
	}
	if !last.Skipped || last.Outcome != nil {
		t.Fatalf("a not-run key carries no verdict, got %+v", last)
	}

	// An unreached key must be left completely untouched — not claimed, not
	// committed — or the budget silently rewrites verification state for a
	// credential nothing tested.
	var stored model.ProviderKey
	if err := db.First(&stored, last.KeyID).Error; err != nil {
		t.Fatalf("load key: %v", err)
	}
	if stored.TestGeneration != generationBefore[last.KeyID] {
		t.Fatalf("a not-run key must not be claimed for testing: generation %d -> %d",
			generationBefore[last.KeyID], stored.TestGeneration)
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
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	client.Err = fmt.Errorf("too many concurrent provider test calls in flight")

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
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
	client.SideEffect = func() {
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
	if results[0].Outcome == nil || *results[0].Outcome != int(providerclient.TestSuccess) {
		t.Fatalf("expected the outcome to still be reported even though it was discarded, got %+v", results[0])
	}

	// The key's own row must NOT have been updated with the discarded
	// result's duration — CreateProvider's own initial re-verify already
	// wrote last_test_duration_ms=10 (newTestProviderService's default
	// canned Fake result), so a batch test that actually applied
	// would have overwritten it with 5 (this test's client.Result); if the
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
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 7}

	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if len(results) != 1 || results[0].Skipped {
		t.Fatalf("expected 1 non-skipped result, got %+v", results)
	}
	if results[0].Outcome == nil || *results[0].Outcome != int(providerclient.TestSuccess) {
		t.Fatalf("expected outcome=providerclient.TestSuccess, got %+v", results[0])
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
		Label: provider.Keys[0].Label, TestModel: provider.Keys[0].TestModel, ManagementStatus: testutil.Ptr(model.ProviderKeyStatusEnabled),
	}, now); err != nil {
		t.Fatalf("re-enable after create failed: %v", err)
	}
	if _, err := svc.UpdateProvider(provider.ID, UpdateProviderInput{Name: provider.Name, BaseURL: "https://new.example.com"}, now); err != nil {
		t.Fatalf("UpdateProvider failed: %v", err)
	}

	callsBefore := client.Calls
	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if client.Calls != callsBefore {
		t.Fatalf("expected zero network calls for a key needing re-entry, calls went from %d to %d", callsBefore, client.Calls)
	}
	if len(results) != 1 || !results[0].NeedsReentry {
		t.Fatalf("expected 1 result flagged needs_reentry, got %+v", results)
	}
}

func TestTestKeyPreviewNeverPersists(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}

	result, _, err := svc.TestKeyPreview(context.Background(), "https://a.example.com", "sk-preview-only", "gpt-4o-mini", "", "")
	if err != nil {
		t.Fatalf("TestKeyPreview failed: %v", err)
	}
	if result.Outcome != providerclient.TestSuccess {
		t.Fatalf("expected providerclient.TestSuccess, got %v", result.Outcome)
	}
	if client.LastProto != protocols.ProtocolOpenAI {
		t.Fatalf("expected an empty provider_type to default to openai, got %q", client.LastProto)
	}
	var count int64
	db.Model(&model.ProviderKey{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected TestKeyPreview to write nothing to the database, found %d rows", count)
	}
}

// TestTestKeyPreviewThreadsProviderTypeToProtocol proves an admin can
// preview-test an anthropic key before the provider row even exists: the
// provider_type on the request body must reach the client as the anthropic
// protocol, not silently fall back to openai.
func TestTestKeyPreviewThreadsProviderTypeToProtocol(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}

	if _, _, err := svc.TestKeyPreview(context.Background(), "https://api.anthropic.com", "sk-ant-preview", "claude-3-5-sonnet", "anthropic", ""); err != nil {
		t.Fatalf("TestKeyPreview failed: %v", err)
	}
	if client.LastProto != protocols.ProtocolClaude {
		t.Fatalf("expected provider_type=anthropic to thread through as ProtocolClaude, got %q", client.LastProto)
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
	testutil.DropTable(t, db, "providers")

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
	testutil.DropTable(t, db, "provider_keys")

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
	testutil.DropTable(t, db, "providers")

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
	testutil.DropTable(t, db, "provider_keys")

	if _, err := svc.GetProviderDetail(provider.ID); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestCreateProviderErrorsWhenNameLookupFails(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	testutil.DropTable(t, db, "providers")

	_, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err == nil || errors.Is(err, errcode.ErrProviderNameTaken) {
		t.Fatalf("expected a raw DB error (not ErrProviderNameTaken), got %v", err)
	}
}

// TestCreateProviderConcurrentSameNameHitsUniqueViolationBranch documents
// (and guards, at the outcome level) the TOCTOU race IsUniqueViolation's
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

// providerclient.TestOutcome and model.LastTestResult* are two hand-maintained lists
// of one enum: the outcome int is written verbatim into
// provider_keys.last_test_result, and the frontend indexes its label array by
// that same int. Nothing but this test stops the lists from drifting when a
// new outcome is appended to only one of them, and a drift silently mislabels
// every result stored from that point on.
func TestLastTestResultConstantsMirrorTestOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		outcome providerclient.TestOutcome
		stored  int
	}{
		{"success", providerclient.TestSuccess, model.LastTestResultSuccess},
		{"auth failed", providerclient.TestAuthFailed, model.LastTestResultAuthFailed},
		{"permission denied", providerclient.TestPermissionDenied, model.LastTestResultPermissionDenied},
		{"model not found", providerclient.TestModelNotFound, model.LastTestResultModelNotFound},
		{"quota unavailable", providerclient.TestQuotaUnavailable, model.LastTestResultQuotaUnavailable},
		{"rate limited", providerclient.TestRateLimited, model.LastTestResultRateLimited},
		{"unreachable", providerclient.TestUnreachable, model.LastTestResultUnreachable},
		{"upstream error", providerclient.TestUpstreamError, model.LastTestResultUpstreamError},
		{"verification unsupported", providerclient.TestVerificationUnsupported, model.LastTestResultVerificationUnsupported},
		{"timeout", providerclient.TestTimeout, model.LastTestResultTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if int(c.outcome) != c.stored {
				t.Fatalf("outcome %s = %d but its stored constant is %d", c.name, int(c.outcome), c.stored)
			}
		})
	}
}

func TestClassifyTestResultCoversEveryOutcome(t *testing.T) {
	outcomeInt := func(o providerclient.TestOutcome) *int { v := int(o); return &v }

	cases := []struct {
		name              string
		result            providerclient.TestResult
		wantVerification  int
		wantOverwrite     bool
		wantLastTestValue *int
	}{
		{"success", providerclient.TestResult{Outcome: providerclient.TestSuccess}, model.VerificationStatusPassed, true, outcomeInt(providerclient.TestSuccess)},
		{"auth failed", providerclient.TestResult{Outcome: providerclient.TestAuthFailed}, model.VerificationStatusFailed, true, outcomeInt(providerclient.TestAuthFailed)},
		{"quota unavailable", providerclient.TestResult{Outcome: providerclient.TestQuotaUnavailable}, model.VerificationStatusFailed, true, outcomeInt(providerclient.TestQuotaUnavailable)},
		{"permission denied not model scoped", providerclient.TestResult{Outcome: providerclient.TestPermissionDenied, IsModelScoped: false}, model.VerificationStatusFailed, true, outcomeInt(providerclient.TestPermissionDenied)},
		{"permission denied model scoped", providerclient.TestResult{Outcome: providerclient.TestPermissionDenied, IsModelScoped: true}, 0, false, outcomeInt(providerclient.TestPermissionDenied)},
		{"model not found", providerclient.TestResult{Outcome: providerclient.TestModelNotFound}, 0, false, outcomeInt(providerclient.TestModelNotFound)},
		{"rate limited", providerclient.TestResult{Outcome: providerclient.TestRateLimited}, 0, false, outcomeInt(providerclient.TestRateLimited)},
		{"unreachable", providerclient.TestResult{Outcome: providerclient.TestUnreachable}, 0, false, outcomeInt(providerclient.TestUnreachable)},
		// A timeout says nothing about whether the credential is valid — the
		// upstream never got far enough to judge it. Inconclusive, exactly
		// like unreachable: it must never overwrite verification_status, or a
		// slow upstream would demote a key that is perfectly good.
		{"timeout", providerclient.TestResult{Outcome: providerclient.TestTimeout}, 0, false, outcomeInt(providerclient.TestTimeout)},
		{"upstream error", providerclient.TestResult{Outcome: providerclient.TestUpstreamError}, 0, false, outcomeInt(providerclient.TestUpstreamError)},
		// providerclient.TestVerificationUnsupported: a destination whose
		// protocol has no success-body validator returns a 2xx that cannot be
		// certified as a genuine pass, so this must never overwrite
		// verification_status — same "inconclusive" shape as
		// providerclient.TestModelNotFound/providerclient.TestRateLimited, not providerclient.TestSuccess's.
		{"verification unsupported", providerclient.TestResult{Outcome: providerclient.TestVerificationUnsupported}, 0, false, outcomeInt(providerclient.TestVerificationUnsupported)},
		{"unknown outcome falls to default", providerclient.TestResult{Outcome: providerclient.TestOutcome(999)}, 0, false, outcomeInt(providerclient.TestOutcome(999))},
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

// --- Finding 1: verification must cover EVERY routable destination ---

// TestCreateProviderKeyVerifiesEveryRoutableDestination is the direct
// regression test for Finding 1: a provider whose primary protocol is
// openai but which also declares an anthropic protocol_endpoints host must
// have its brand-new key tested against BOTH hosts before it can be
// authorized — not just the primary. When the second (anthropic) host
// returns 401, the aggregate must be providerclient.TestAuthFailed and the key must NOT
// reach passed/enabled, even though the primary host alone would have
// passed.
func TestCreateProviderKeyVerifiesEveryRoutableDestination(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	const anthropicURL = "https://anthropic.example.com/v1"
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
	client.PerTarget = map[string]providerclienttest.TargetResponse{
		string(protocols.ProtocolClaude) + "|" + anthropicURL: {Result: providerclient.TestResult{Outcome: providerclient.TestAuthFailed, DurationMs: 3}},
	}

	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "multi-endpoint", BaseURL: "https://openai.example.com/v1", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ProtocolEndpoints: `{"anthropic":"` + anthropicURL + `"}`,
		ManagementStatus:  model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if client.Calls != 2 {
		t.Fatalf("expected verification to test both routable destinations (openai + anthropic), got %d calls", client.Calls)
	}
	if provider.Keys[0].VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("expected verification_status=failed since the anthropic destination never passed, got %d", provider.Keys[0].VerificationStatus)
	}
	if provider.Keys[0].ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected the key to stay disabled since not every destination passed, got %d", provider.Keys[0].ManagementStatus)
	}
}

// TestCreateProviderKeyPassesOnlyWhenEveryRoutableDestinationSucceeds is the
// positive counterpart: when BOTH the primary and the protocol_endpoints
// destination succeed, the aggregate is providerclient.TestSuccess and the key reaches
// passed/enabled exactly like the single-destination case always has.
func TestCreateProviderKeyPassesOnlyWhenEveryRoutableDestinationSucceeds(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}

	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "multi-endpoint-ok", BaseURL: "https://openai.example.com/v1", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ProtocolEndpoints: `{"anthropic":"https://anthropic.example.com/v1"}`,
		ManagementStatus:  model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if client.Calls != 2 {
		t.Fatalf("expected 2 verification calls (openai + anthropic), got %d", client.Calls)
	}
	if provider.Keys[0].VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status=passed when every routable destination succeeds, got %d", provider.Keys[0].VerificationStatus)
	}
	if provider.Keys[0].ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Fatalf("expected management_status=enabled after a fully passing verification, got %d", provider.Keys[0].ManagementStatus)
	}
}

// TestCreateProviderKeyHitsRealAnthropicProtocolEndpointHost proves the
// credential-scope fix end-to-end through a real providerclient.HTTPProviderClient (not
// the fake): a provider primarily typed openai, with an anthropic
// protocol_endpoints host pointed at a second real httptest server, must
// actually send an HTTP request to that second host during verification —
// not just resolve its URL on paper. The anthropic host 401s, so the key
// must not reach passed even though the primary openai host alone succeeds.
func TestCreateProviderKeyHitsRealAnthropicProtocolEndpointHost(t *testing.T) {
	openaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	}))
	defer openaiSrv.Close()

	var anthropicHits int32
	anthropicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&anthropicHits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer anthropicSrv.Close()

	realClient := providerclient.NewHTTPProviderClient(true)
	// allowPrivate=true: safehttp's SSRF-safe transport denies loopback
	// dials by default, which is exactly where httptest servers listen; the
	// allow-private mode is the production-supported way to permit them.

	db := testutil.NewSQLiteDB(t)
	svc := NewProviderService(db, testutil.ProviderSecrets(), realClient)
	now := time.Now().UTC()

	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "real-multi-endpoint", BaseURL: openaiSrv.URL, KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ProtocolEndpoints: fmt.Sprintf(`{"anthropic":%q}`, anthropicSrv.URL),
		ManagementStatus:  model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if atomic.LoadInt32(&anthropicHits) == 0 {
		t.Fatalf("expected the anthropic protocol_endpoints host to receive a real credential-test request")
	}
	if provider.Keys[0].VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("expected verification_status=failed since the anthropic destination 401s, got %d", provider.Keys[0].VerificationStatus)
	}
}

// TestTestProviderKeyRetestCoversEveryRoutableDestination proves the retest
// entry point (TestProviderKey), not just the brand-new-key create path,
// also routes through verifyKeyAllDestinations.
func TestTestProviderKeyRetestCoversEveryRoutableDestination(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	const anthropicURL = "https://anthropic.example.com/v1"
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ProtocolEndpoints: `{"anthropic":"` + anthropicURL + `"}`,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	callsAfterCreate := client.Calls
	client.PerTarget = map[string]providerclienttest.TargetResponse{
		string(protocols.ProtocolClaude) + "|" + anthropicURL: {Result: providerclient.TestResult{Outcome: providerclient.TestAuthFailed}},
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}

	view, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now)
	if err != nil {
		t.Fatalf("TestProviderKey failed: %v", err)
	}
	if client.Calls != callsAfterCreate+2 {
		t.Fatalf("expected the retest to hit both routable destinations, calls went from %d to %d", callsAfterCreate, client.Calls)
	}
	if view.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("expected verification_status=failed since the anthropic destination fails on retest, got %d", view.VerificationStatus)
	}
}

// TestTestAllProviderKeysBatchCoversEveryRoutableDestination is
// TestTestProviderKeyRetestCoversEveryRoutableDestination's counterpart for
// the batch retest path (TestAllProviderKeys).
func TestTestAllProviderKeysBatchCoversEveryRoutableDestination(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	now := time.Now().UTC()
	const anthropicURL = "https://anthropic.example.com/v1"
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ProtocolEndpoints: `{"anthropic":"` + anthropicURL + `"}`,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	callsAfterCreate := client.Calls
	client.PerTarget = map[string]providerclienttest.TargetResponse{
		string(protocols.ProtocolClaude) + "|" + anthropicURL: {Result: providerclient.TestResult{Outcome: providerclient.TestAuthFailed}},
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}

	results, err := svc.TestAllProviderKeys(context.Background(), provider.ID, now)
	if err != nil {
		t.Fatalf("TestAllProviderKeys failed: %v", err)
	}
	if client.Calls != callsAfterCreate+2 {
		t.Fatalf("expected the batch retest to hit both routable destinations, calls went from %d to %d", callsAfterCreate, client.Calls)
	}
	if len(results) != 1 || results[0].Outcome == nil || *results[0].Outcome != int(providerclient.TestAuthFailed) {
		t.Fatalf("expected the batch result to report the anthropic destination's failure, got %+v", results)
	}
}

// TestTestProviderKeyRetestDemotesWhenSecondaryDecisiveFailureBeatsWeakPrimary
// is the direct regression test for the severity-ranked aggregate: on a
// retest of a currently-Passed key, a DECISIVE failure at a SECONDARY
// destination (providerclient.TestAuthFailed — the credential just got rejected there) must
// win over a weaker, inconclusive failure at the PRIMARY (providerclient.TestModelNotFound,
// which classifyTestResult leaves non-overwriting). A first-encountered-wins
// aggregate would return the primary's weak result and wrongly leave the key
// Passed/routable to the rejected destination; the most-severe rule demotes
// it to Failed.
func TestTestProviderKeyRetestDemotesWhenSecondaryDecisiveFailureBeatsWeakPrimary(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	const anthropicURL = "https://anthropic.example.com/v1"
	// Both destinations succeed at create time, so the key starts Passed.
	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "p1", BaseURL: "https://a.example.com", KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ProtocolEndpoints: `{"anthropic":"` + anthropicURL + `"}`,
		ManagementStatus:  model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	var seeded model.ProviderKey
	if err := db.Where("id = ?", provider.Keys[0].ID).First(&seeded).Error; err != nil {
		t.Fatalf("reload key failed: %v", err)
	}
	if seeded.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("test setup: expected the key to start Passed, got %d", seeded.VerificationStatus)
	}

	// Retest: PRIMARY (openai@hostA) returns a weak, non-overwriting
	// providerclient.TestModelNotFound; SECONDARY (anthropic@hostB) returns a decisive
	// providerclient.TestAuthFailed.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	client.PerTarget = map[string]providerclienttest.TargetResponse{
		string(protocols.ProtocolClaude) + "|" + anthropicURL: {Result: providerclient.TestResult{Outcome: providerclient.TestAuthFailed}},
	}

	view, err := svc.TestProviderKey(context.Background(), provider.ID, provider.Keys[0].ID, now)
	if err != nil {
		t.Fatalf("TestProviderKey failed: %v", err)
	}
	if view.LastTestResult == nil || *view.LastTestResult != int(providerclient.TestAuthFailed) {
		t.Fatalf("expected the aggregate to surface the secondary's decisive providerclient.TestAuthFailed, got last_test_result=%v", view.LastTestResult)
	}
	if view.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("expected the key to be DEMOTED to Failed by the secondary's decisive failure, got %d", view.VerificationStatus)
	}
}

func TestVerificationSeverityRanksDecisiveFailuresAboveInconclusive(t *testing.T) {
	cases := []struct {
		name   string
		result providerclient.TestResult
		want   int
	}{
		{"success", providerclient.TestResult{Outcome: providerclient.TestSuccess}, 0},
		{"auth failed is decisive", providerclient.TestResult{Outcome: providerclient.TestAuthFailed}, 2},
		{"quota unavailable is decisive", providerclient.TestResult{Outcome: providerclient.TestQuotaUnavailable}, 2},
		{"non-model-scoped permission denied is decisive", providerclient.TestResult{Outcome: providerclient.TestPermissionDenied, IsModelScoped: false}, 2},
		{"model-scoped permission denied is inconclusive", providerclient.TestResult{Outcome: providerclient.TestPermissionDenied, IsModelScoped: true}, 1},
		{"model not found is inconclusive", providerclient.TestResult{Outcome: providerclient.TestModelNotFound}, 1},
		{"rate limited is inconclusive", providerclient.TestResult{Outcome: providerclient.TestRateLimited}, 1},
		{"unreachable is inconclusive", providerclient.TestResult{Outcome: providerclient.TestUnreachable}, 1},
		{"upstream error is inconclusive", providerclient.TestResult{Outcome: providerclient.TestUpstreamError}, 1},
		{"verification unsupported is inconclusive", providerclient.TestResult{Outcome: providerclient.TestVerificationUnsupported}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verificationSeverity(c.result); got != c.want {
				t.Fatalf("verificationSeverity(%+v) = %d, want %d", c.result, got, c.want)
			}
		})
	}
}

// --- An uncertifiable destination must not falsely certify a key ---

// TestCreateProviderKeyGeminiDestinationNeverReachesPassed pins the service
// layer's handling of providerclient.TestVerificationUnsupported: a key whose
// client-level test returns it (a 200 the protocol's probe cannot certify)
// must never be classified as passed/enabled, but its last_test_result must
// still be recorded for the UI. The outcome is produced via the fake client —
// every current protocol now has a real success-body validator, but the
// service-layer rule must hold for any future protocol that lacks one.
func TestCreateProviderKeyGeminiDestinationNeverReachesPassed(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestVerificationUnsupported, DurationMs: 5}
	now := time.Now().UTC()

	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "gemini-provider", BaseURL: "https://gemini.example.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gemini-1.5-flash",
		ProviderType: "gemini", ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if provider.Keys[0].VerificationStatus != model.VerificationStatusUntested {
		t.Fatalf("expected verification_status to stay untested for a gemini destination that cannot be certified, got %d", provider.Keys[0].VerificationStatus)
	}
	if provider.Keys[0].ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected management_status to stay disabled since the key never reached passed, got %d", provider.Keys[0].ManagementStatus)
	}
	if provider.Keys[0].LastTestResult == nil || *provider.Keys[0].LastTestResult != int(providerclient.TestVerificationUnsupported) {
		t.Fatalf("expected last_test_result=%d recorded for UI visibility, got %v", providerclient.TestVerificationUnsupported, provider.Keys[0].LastTestResult)
	}
}

// --- Non-regression: single-destination providers verify exactly as before ---

// TestCreateProviderOpenAIOnlyStillVerifiesToPassedWithOneCall proves the
// common case (no protocol_endpoints) is unaffected by the multi-destination
// helper: exactly one verification call still runs, and a passing result
// still reaches passed/enabled.
func TestCreateProviderOpenAIOnlyStillVerifiesToPassedWithOneCall(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
	now := time.Now().UTC()

	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "openai-only", BaseURL: "https://api.openai.com/v1", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if client.Calls != 1 {
		t.Fatalf("expected exactly 1 verification call for a single-destination provider, got %d", client.Calls)
	}
	if provider.Keys[0].VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status=passed for an openai-only provider, got %d", provider.Keys[0].VerificationStatus)
	}
	if provider.Keys[0].ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Fatalf("expected management_status=enabled after a passing verification, got %d", provider.Keys[0].ManagementStatus)
	}
}

// TestCreateProviderAnthropicOnlyStillVerifiesToPassedViaRealClassification
// mirrors the openai-only non-regression check for an anthropic-only
// provider, confirming the protocol still threads through correctly and
// the single-target case makes exactly one call.
func TestCreateProviderAnthropicOnlyStillVerifiesToPassedViaRealClassification(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
	now := time.Now().UTC()

	provider, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "anthropic-only", BaseURL: "https://api.anthropic.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "claude-3-5-sonnet",
		ProviderType: "anthropic", ManagementStatus: model.ProviderStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if client.Calls != 1 {
		t.Fatalf("expected exactly 1 verification call for a single-destination provider, got %d", client.Calls)
	}
	if provider.Keys[0].VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status=passed for an anthropic-only provider, got %d", provider.Keys[0].VerificationStatus)
	}
	if provider.Keys[0].ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Fatalf("expected management_status=enabled after a passing verification, got %d", provider.Keys[0].ManagementStatus)
	}
	if client.LastProto != protocols.ProtocolClaude {
		t.Fatalf("expected the verification call to use the anthropic protocol, got %q", client.LastProto)
	}
}

// TestVerifyKeyAllDestinationsPropagatesClientError proves a transport-level
// error from ANY destination (not just the first) aborts the whole
// verification with that error and returns a zero-value providerclient.TestResult —
// matching runNewPlaintextTestAndCommit's existing "never classify a
// zero-value providerclient.TestResult as success" rule.
func TestVerifyKeyAllDestinationsPropagatesClientError(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}

	provider := &model.Provider{
		Name: "p1", ProviderType: "openai", BaseURL: "https://a.example.com",
		ProtocolEndpoints: `{"anthropic":"https://b.example.com"}`,
		ManagementStatus:  model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(provider).Error; err != nil {
		t.Fatalf("seed provider failed: %v", err)
	}
	client.Err = fmt.Errorf("too many concurrent provider test calls in flight")

	result, perTarget, err := svc.verifyKeyAllDestinations(context.Background(), provider, "sk-abcdefghijklmnopqrstuvwxyz1234", "gpt-4o-mini")
	if err == nil {
		t.Fatalf("expected the client's error to propagate")
	}
	if result != (providerclient.TestResult{}) {
		t.Fatalf("expected a zero-value providerclient.TestResult on error, got %+v", result)
	}
	// An aborted run judged nothing, so it must not hand back a partial
	// breakdown that would later be stored as if it described the whole run.
	if perTarget != nil {
		t.Fatalf("expected no per-destination breakdown on error, got %+v", perTarget)
	}
}

func TestPickKeyForCatalogueFetch(t *testing.T) {
	const dv = 3
	enabledVerified := model.ProviderKey{Label: "verified", ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: dv}
	enabledUntested := model.ProviderKey{Label: "untested", ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusUntested, AuthorizedDestinationVersion: dv}
	disabled := model.ProviderKey{Label: "disabled", ManagementStatus: model.ProviderKeyStatusDisabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: dv}
	needsReentry := model.ProviderKey{Label: "reentry", ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: dv - 1}

	cases := []struct {
		name string
		keys []model.ProviderKey
		want string // Label of the expected key; "" means nil is expected
	}{
		{"no keys", nil, ""},
		{"only disabled", []model.ProviderKey{disabled}, ""},
		{"only needs-reentry", []model.ProviderKey{needsReentry}, ""},
		{"prefers verified even when an untested key comes first", []model.ProviderKey{enabledUntested, enabledVerified}, "verified"},
		{"falls back to the first enabled current key when none verified", []model.ProviderKey{enabledUntested}, "untested"},
		{"skips disabled and needs-reentry to reach a usable key", []model.ProviderKey{disabled, needsReentry, enabledUntested}, "untested"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickKeyForCatalogueFetch(tc.keys, dv)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("expected nil, got %q", got.Label)
				}
				return
			}
			if got == nil || got.Label != tc.want {
				t.Fatalf("expected key %q, got %v", tc.want, got)
			}
		})
	}
}

// seedProviderWithUsableKey creates a provider and forces its first key into
// an enabled, verified, current state so a catalogue fetch can authenticate.
func seedProviderWithUsableKey(t *testing.T, svc *ProviderService, db *gorm.DB, name string) uint {
	t.Helper()
	view, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: name, BaseURL: "https://api.example.com/v1",
		KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	var prov model.Provider
	if err := db.First(&prov, view.ID).Error; err != nil {
		t.Fatalf("load provider failed: %v", err)
	}
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", view.Keys[0].ID).Updates(map[string]any{
		"management_status":              model.ProviderKeyStatusEnabled,
		"verification_status":            model.VerificationStatusPassed,
		"authorized_destination_version": prov.DestinationVersion,
	}).Error; err != nil {
		t.Fatalf("promote key failed: %v", err)
	}
	return view.ID
}

func TestListModelsForProviderReturnsCatalogueForUsableKey(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	client.Models = []string{"model-a", "model-b"}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	id := seedProviderWithUsableKey(t, svc, db, "openai-main")

	res, err := svc.ListModelsForProvider(context.Background(), id)
	if err != nil {
		t.Fatalf("ListModelsForProvider failed: %v", err)
	}
	if res.Outcome == nil || *res.Outcome != providerclient.TestSuccess {
		t.Fatalf("expected providerclient.TestSuccess, got %v", res.Outcome)
	}
	if len(res.Models) != 2 {
		t.Fatalf("expected 2 models, got %v", res.Models)
	}
}

func TestListModelsForProviderFallsBackWhenNoUsableKey(t *testing.T) {
	svc, db, client := newTestProviderService(t)
	client.Models = []string{"unused"}
	view, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "no-usable-key", BaseURL: "https://api.example.com/v1",
		KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	// Force the only key disabled so no key qualifies for a catalogue fetch.
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", view.Keys[0].ID).
		Update("management_status", model.ProviderKeyStatusDisabled).Error; err != nil {
		t.Fatalf("disable key failed: %v", err)
	}

	res, err := svc.ListModelsForProvider(context.Background(), view.ID)
	if err != nil {
		t.Fatalf("ListModelsForProvider failed: %v", err)
	}
	if !res.NoUsableKey {
		t.Fatalf("expected NoUsableKey when the provider's only key is disabled")
	}
	// No request left the process, so there is no upstream verdict to report —
	// not even the zero value, which names success. Answering with a
	// credential-test category (this used to be providerclient.TestAuthFailed,
	// and before the outcome became a pointer, an embedded zero-value result
	// reported a pass) blames the key and the address for a fetch that was
	// never attempted.
	if res.Outcome != nil {
		t.Fatalf("expected no upstream verdict, got outcome %v", *res.Outcome)
	}
	if res.Detail != "" {
		t.Fatalf("expected no upstream detail, got %q", res.Detail)
	}
	if len(res.Models) != 0 {
		t.Fatalf("expected no models on fallback, got %v", res.Models)
	}
}

func TestListModelsForProviderUnknownProviderReturnsNotFound(t *testing.T) {
	svc, _, _ := newTestProviderService(t)
	if _, err := svc.ListModelsForProvider(context.Background(), 999999); !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

// A model with a second routable source survives losing this provider; a model
// whose only routable candidate is here does not. The flag must tell those two
// apart, and a provider nothing references must report an empty list — not an
// error, and not nil.
func TestProviderImpactFlagsModelsLosingTheirLastRoutableSource(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	providerA := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	providerB := seedEnabledProviderForModelTest(t, providerService, "provider-b")
	providerIdle := seedEnabledProviderForModelTest(t, providerService, "provider-idle")

	modelSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	survivor, err := modelSvc.CreateModel(modeladmin.CreateModelInput{Name: "survivor"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	stranded, err := modelSvc.CreateModel(modeladmin.CreateModelInput{Name: "stranded"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	addCandidate := func(modelID, providerID uint) {
		t.Helper()
		if _, err := modelSvc.CreateModelCandidate(context.Background(), modelID, modeladmin.CreateCandidateInput{
			ProviderID: providerID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
			ManagementStatus: model.ModelCandidateStatusEnabled,
		}, now); err != nil {
			t.Fatalf("CreateModelCandidate failed: %v", err)
		}
	}
	addCandidate(survivor.ID, providerA.ID)
	addCandidate(survivor.ID, providerB.ID)
	addCandidate(stranded.ID, providerA.ID)

	impact, err := providerService.GetProviderImpact(providerA.ID, now)
	if err != nil {
		t.Fatalf("GetProviderImpact failed: %v", err)
	}
	if len(impact.Models) != 2 {
		t.Fatalf("impact models = %+v, want the two models referencing provider-a", impact.Models)
	}
	byName := map[string]bool{}
	for _, m := range impact.Models {
		byName[m.Name] = m.NoOtherRoutableSource
	}
	if byName["survivor"] {
		t.Error("survivor has a routable candidate on provider-b and must not be flagged")
	}
	if !byName["stranded"] {
		t.Error("stranded has no other source and must be flagged")
	}

	idleImpact, err := providerService.GetProviderImpact(providerIdle.ID, now)
	if err != nil {
		t.Fatalf("GetProviderImpact failed: %v", err)
	}
	if idleImpact.Models == nil || len(idleImpact.Models) != 0 {
		t.Fatalf("idle provider impact = %+v, want empty non-nil list", idleImpact.Models)
	}
}

// The fallback source must actually be routable, not merely exist: here the
// other provider's candidate exists but is disabled, so the model is flagged.
func TestProviderImpactIgnoresUnroutableFallbackCandidates(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	providerA := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	providerB := seedEnabledProviderForModelTest(t, providerService, "provider-b")

	modelSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	m, err := modelSvc.CreateModel(modeladmin.CreateModelInput{Name: "fragile"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := modelSvc.CreateModelCandidate(context.Background(), m.ID, modeladmin.CreateCandidateInput{
		ProviderID: providerA.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	fallback, err := modelSvc.CreateModelCandidate(context.Background(), m.ID, modeladmin.CreateCandidateInput{
		ProviderID: providerB.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if err := modelSvc.SetCandidateStatus(fallback.ID, false, now); err != nil {
		t.Fatalf("SetCandidateStatus failed: %v", err)
	}

	impact, err := providerService.GetProviderImpact(providerA.ID, now)
	if err != nil {
		t.Fatalf("GetProviderImpact failed: %v", err)
	}
	if len(impact.Models) != 1 || !impact.Models[0].NoOtherRoutableSource {
		t.Fatalf("impact = %+v, want fragile flagged: its only fallback is disabled", impact.Models)
	}
}

func TestProviderImpactUnknownProvider(t *testing.T) {
	providerService, _, _ := newTestProviderService(t)
	if _, err := providerService.GetProviderImpact(9999, time.Now().UTC()); !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("err = %v, want ErrProviderNotFound", err)
	}
}

// A model this provider does not currently serve loses nothing when the
// provider is disabled: its only candidate here is disabled, so it stays in
// the reference list but must not be flagged as losing its last source.
func TestProviderImpactDoesNotBlameAlreadyDeadModelsOnThisProvider(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	providerA := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	modelSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	dead, err := modelSvc.CreateModel(modeladmin.CreateModelInput{Name: "already-dead"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	only, err := modelSvc.CreateModelCandidate(context.Background(), dead.ID, modeladmin.CreateCandidateInput{
		ProviderID: providerA.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if err := modelSvc.SetCandidateStatus(only.ID, false, now); err != nil {
		t.Fatalf("SetCandidateStatus failed: %v", err)
	}

	impact, err := providerService.GetProviderImpact(providerA.ID, now)
	if err != nil {
		t.Fatalf("GetProviderImpact failed: %v", err)
	}
	if len(impact.Models) != 1 || impact.Models[0].Name != "already-dead" {
		t.Fatalf("impact = %+v, want the referencing model listed", impact.Models)
	}
	if impact.Models[0].NoOtherRoutableSource {
		t.Error("already-dead is not served by this provider — disabling it takes nothing away, it must not be flagged")
	}
}

// A disabled model is rejected by the gateway before candidate selection, so
// it is already unavailable to callers — disabling the provider that holds
// its only routable candidate takes nothing away, and it must not be flagged.
func TestProviderImpactDoesNotBlameDisabledModels(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	providerA := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	modelSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	m, err := modelSvc.CreateModel(modeladmin.CreateModelInput{Name: "switched-off"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := modelSvc.CreateModelCandidate(context.Background(), m.ID, modeladmin.CreateCandidateInput{
		ProviderID: providerA.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if err := modelSvc.SetModelStatus(m.ID, false, now); err != nil {
		t.Fatalf("SetModelStatus failed: %v", err)
	}

	impact, err := providerService.GetProviderImpact(providerA.ID, now)
	if err != nil {
		t.Fatalf("GetProviderImpact failed: %v", err)
	}
	if len(impact.Models) != 1 || impact.Models[0].Name != "switched-off" {
		t.Fatalf("impact = %+v, want the referencing model listed", impact.Models)
	}
	if impact.Models[0].NoOtherRoutableSource {
		t.Error("a disabled model is already unavailable — disabling its provider must not be reported as an outage")
	}
}

// Keys are named through stranded models only, once each: a key allowlisting
// two stranded models appears once, a key allowlisting only the surviving
// model is not named, and when nothing is stranded no key is named at all.
func TestProviderImpactNamesKeysOnlyThroughStrandedModels(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	providerA := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	providerB := seedEnabledProviderForModelTest(t, providerService, "provider-b")

	modelSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	makeModel := func(name string, providers ...*ProviderView) uint {
		t.Helper()
		m, err := modelSvc.CreateModel(modeladmin.CreateModelInput{Name: name}, now)
		if err != nil {
			t.Fatalf("CreateModel failed: %v", err)
		}
		for _, p := range providers {
			if _, err := modelSvc.CreateModelCandidate(context.Background(), m.ID, modeladmin.CreateCandidateInput{
				ProviderID: p.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
				ManagementStatus: model.ModelCandidateStatusEnabled,
			}, now); err != nil {
				t.Fatalf("CreateModelCandidate failed: %v", err)
			}
		}
		return m.ID
	}
	stranded1 := makeModel("stranded-1", providerA)
	stranded2 := makeModel("stranded-2", providerA)
	survivor := makeModel("survivor", providerA, providerB)

	keySvc := apikey.NewAPIKeyService(db, testutil.ProviderSecrets())
	both, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{stranded1, stranded2}}, now)
	if err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}
	if _, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{survivor}}, now); err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}
	if _, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), AllowAllModels: true}, now); err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}

	impact, err := providerService.GetProviderImpact(providerA.ID, now)
	if err != nil {
		t.Fatalf("GetProviderImpact failed: %v", err)
	}
	if len(impact.AffectedKeys) != 1 || impact.AffectedKeys[0].ID != both.APIKey.ID {
		t.Fatalf("affected keys = %+v, want exactly one entry for the both-stranded key", impact.AffectedKeys)
	}
	if impact.AllowAllKeyCount != 1 {
		t.Fatalf("allow-all count = %d, want 1", impact.AllowAllKeyCount)
	}

	impactB, err := providerService.GetProviderImpact(providerB.ID, now)
	if err != nil {
		t.Fatalf("GetProviderImpact failed: %v", err)
	}
	if len(impactB.AffectedKeys) != 0 || impactB.AllowAllKeyCount != 0 {
		t.Fatalf("provider-b strands nothing, yet keys reported: %+v allowAll=%d", impactB.AffectedKeys, impactB.AllowAllKeyCount)
	}
}

// The delete confirmation states what the cascade removes, so the impact
// answer must carry the provider's own key and candidate counts — including
// for a provider nothing references, whose early empty-models answer still
// has keys to name.
func TestProviderImpactCountsKeysAndCandidates(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	providerA := seedEnabledProviderForModelTest(t, providerService, "counted-provider")
	providerIdle := seedEnabledProviderForModelTest(t, providerService, "counted-idle")

	if _, err := providerService.CreateProviderKey(context.Background(), providerA.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-abcdefghijklmnopqrstuvwxyz9999", TestModel: "gpt-4o-mini",
	}, now); err != nil {
		t.Fatalf("CreateProviderKey failed: %v", err)
	}

	modelSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	for _, name := range []string{"counted-m1", "counted-m2"} {
		m, err := modelSvc.CreateModel(modeladmin.CreateModelInput{Name: name}, now)
		if err != nil {
			t.Fatalf("CreateModel failed: %v", err)
		}
		if _, err := modelSvc.CreateModelCandidate(context.Background(), m.ID, modeladmin.CreateCandidateInput{
			ProviderID: providerA.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
			ManagementStatus: model.ModelCandidateStatusEnabled,
		}, now); err != nil {
			t.Fatalf("CreateModelCandidate failed: %v", err)
		}
	}

	impact, err := providerService.GetProviderImpact(providerA.ID, now)
	if err != nil {
		t.Fatalf("GetProviderImpact failed: %v", err)
	}
	if impact.KeyCount != 2 {
		t.Errorf("KeyCount = %d, want 2", impact.KeyCount)
	}
	if impact.CandidateCount != 2 {
		t.Errorf("CandidateCount = %d, want 2", impact.CandidateCount)
	}

	idleImpact, err := providerService.GetProviderImpact(providerIdle.ID, now)
	if err != nil {
		t.Fatalf("GetProviderImpact failed: %v", err)
	}
	if idleImpact.KeyCount != 1 {
		t.Errorf("idle KeyCount = %d, want 1 (the empty-models early answer must still count keys)", idleImpact.KeyCount)
	}
	if idleImpact.CandidateCount != 0 {
		t.Errorf("idle CandidateCount = %d, want 0", idleImpact.CandidateCount)
	}
}

// The video-dialect fallback of key verification: a video-only upstream
// account answers the chat probe with ModelNotFound (model gating happens
// past auth, so the credential itself already authenticated), and the
// verification retries through the video task dialect instead of leaving
// the key permanently unverifiable.
func TestKeyPreviewFallsBackToVideoDialectOnModelNotOpen(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestModelNotFound}},
		"video": {Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 40}},
	}

	result, perTarget, err := svc.TestKeyPreview(context.Background(), "https://ark.cn-beijing.volces.com/api/v3", "ark-video-only", "doubao-seedance-2-0-260128", "", "")
	if err != nil {
		t.Fatalf("TestKeyPreview failed: %v", err)
	}
	if result.Outcome != providerclient.TestSuccess {
		t.Fatalf("the video fallback must verify the key, got outcome %d detail %q", result.Outcome, result.Detail)
	}
	if !strings.Contains(result.Detail, "video task dialect") {
		t.Fatalf("the verdict must say which shape decided it, got %q", result.Detail)
	}
	if result.DurationMs < 40 {
		t.Fatalf("the recorded duration must cover both probes, got %d", result.DurationMs)
	}
	if client.CallCountFor("video") != 1 {
		t.Fatalf("exactly one video probe must run, got %d", client.CallCountFor("video"))
	}
	if len(perTarget) != 1 || perTarget[0].Detail != result.Detail {
		t.Fatalf("the per-target breakdown must carry the fallback's verdict, got %+v", perTarget)
	}
}

// The second live shape: an OPENED video model asked to chat answers with
// an upstream error (Ark's chat endpoint returns 500 InternalServiceError
// for a video model), which must fall back to the video dialect just like
// the cold-model shape does.
func TestKeyPreviewFallsBackToVideoDialectOnChatUpstreamError(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestUpstreamError}},
		"video": {Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 40}},
	}

	result, _, err := svc.TestKeyPreview(context.Background(), "https://ark.cn-beijing.volces.com/api/v3", "ark-video-only", "doubao-seedance-2-0-mini-260615", "", "")
	if err != nil {
		t.Fatalf("TestKeyPreview failed: %v", err)
	}
	if result.Outcome != providerclient.TestSuccess {
		t.Fatalf("the video fallback must verify the key, got outcome %d detail %q", result.Outcome, result.Detail)
	}
	if client.CallCountFor("video") != 1 {
		t.Fatalf("exactly one video probe must run, got %d", client.CallCountFor("video"))
	}
}

// A model that is not activated for ANY dialect keeps the chat verdict:
// the fallback replaces the answer only when it passes.
func TestKeyPreviewVideoFallbackDoesNotMaskATrueModelNotOpen(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestModelNotFound}},
		"video": {Result: providerclient.TestResult{Outcome: providerclient.TestModelNotFound}},
		"image": {Result: providerclient.TestResult{Outcome: providerclient.TestModelNotFound}},
	}

	result, _, err := svc.TestKeyPreview(context.Background(), "https://ark.cn-beijing.volces.com/api/v3", "ark-cold", "doubao-seedance-2-0-260128", "", "")
	if err != nil {
		t.Fatalf("TestKeyPreview failed: %v", err)
	}
	if result.Outcome != providerclient.TestModelNotFound {
		t.Fatalf("an unopened model must stay model-not-found, got %d", result.Outcome)
	}
}

// The image-only twin: an account that opened a Seedream-class image
// model and no chat model verifies through the image probe — the video
// fallback runs first (and fails harmlessly), the image fallback decides.
func TestKeyPreviewFallsBackToImageDialectForImageOnlyAccounts(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestUpstreamError, DurationMs: 20}},
		"video": {Result: providerclient.TestResult{Outcome: providerclient.TestModelNotFound, DurationMs: 15}},
		"image": {Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 60}},
	}

	result, perTarget, err := svc.TestKeyPreview(context.Background(), "https://ark.cn-beijing.volces.com/api/v3", "ark-image-only", "doubao-seedream-4-5-251128", "", "")
	if err != nil {
		t.Fatalf("TestKeyPreview failed: %v", err)
	}
	if result.Outcome != providerclient.TestSuccess {
		t.Fatalf("the image fallback must verify the key, got outcome %d detail %q", result.Outcome, result.Detail)
	}
	if !strings.Contains(result.Detail, "image probe") {
		t.Fatalf("the verdict must say which shape decided it, got %q", result.Detail)
	}
	if result.DurationMs < 95 {
		t.Fatalf("the recorded duration must cover chat, the failed video probe, and the image probe, got %d", result.DurationMs)
	}
	if client.CallCountFor("video") != 1 || client.CallCountFor("image") != 1 {
		t.Fatalf("video then image must both have run, got video=%d image=%d", client.CallCountFor("video"), client.CallCountFor("image"))
	}
	if len(perTarget) != 1 || perTarget[0].Detail != result.Detail {
		t.Fatalf("the per-target breakdown must carry the fallback's verdict, got %+v", perTarget)
	}
}

// A transport-level refusal from the fallback probe itself is swallowed —
// the chat verdict stands and the verification does not abort, matching the
// "every other outcome keeps the chat's own answer" rule.
func TestKeyPreviewVideoProbeTransportErrorKeepsChatVerdict(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestUpstreamError, DurationMs: 20}},
		"video": {Err: errors.New("client refused the call")},
		"image": {Err: errors.New("client refused the call")},
	}

	result, _, err := svc.TestKeyPreview(context.Background(), "https://ark.cn-beijing.volces.com/api/v3", "ark-x", "some-model", "", "")
	if err != nil {
		t.Fatalf("a refused fallback probe must not abort the verification: %v", err)
	}
	if result.Outcome != providerclient.TestUpstreamError {
		t.Fatalf("the chat verdict must stand, got %d", result.Outcome)
	}
}

// The fallback is scoped to video-dialect bases: an ordinary OpenAI-shaped
// host answering ModelNotFound is a wrong model name, and probing it with a
// video submit would measure an endpoint no routed request would ever hit.
func TestKeyPreviewNoVideoFallbackOnOrdinaryBases(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestModelNotFound}},
		"video": {Result: providerclient.TestResult{Outcome: providerclient.TestSuccess}},
	}

	result, _, err := svc.TestKeyPreview(context.Background(), "https://api.openai.com/v1", "sk-x", "no-such-model", "", "")
	if err != nil {
		t.Fatalf("TestKeyPreview failed: %v", err)
	}
	if result.Outcome != providerclient.TestModelNotFound {
		t.Fatalf("a non-video base keeps its own verdict, got %d", result.Outcome)
	}
	if client.CallCountFor("video") != 0 {
		t.Fatalf("no video probe may run against an ordinary base, got %d", client.CallCountFor("video"))
	}
}

// The fallback on the minimax base: the same one host serves the
// OpenAI-compatible chat dialect and the V2 video task dialect, so a video
// mapping's chat-shaped ModelNotFound retries through the video dialect —
// the fourth media base, gated by the same MediaDialectBase dispatch.
func TestKeyPreviewFallsBackToVideoDialectOnMiniMaxBase(t *testing.T) {
	svc, _, client := newTestProviderService(t)
	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestModelNotFound}},
		"video": {Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 40}},
	}

	result, _, err := svc.TestKeyPreview(context.Background(), "https://api.minimax.cn", "minimax-video", "MiniMax-H3", "", "")
	if err != nil {
		t.Fatalf("TestKeyPreview failed: %v", err)
	}
	if result.Outcome != providerclient.TestSuccess {
		t.Fatalf("the video fallback must verify the key, got outcome %d detail %q", result.Outcome, result.Detail)
	}
	if !strings.Contains(result.Detail, "video task dialect") {
		t.Fatalf("the verdict must say which shape decided it, got %q", result.Detail)
	}
	if client.CallCountFor("video") != 1 {
		t.Fatalf("exactly one video probe must run, got %d", client.CallCountFor("video"))
	}
}
