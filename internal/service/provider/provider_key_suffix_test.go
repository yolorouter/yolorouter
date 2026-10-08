// Provider key display-suffix coverage for the live write paths: the
// suffix is derived and stored on every write path that touches a key's
// plaintext — provider creation, key append, and the plaintext swap — and
// the key list API carries it beside the prefix. The legacy-backfill half
// of this coverage lives in internal/keysuffixbackfill.
package provider

import (
	"context"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestKeySuffixForDerivation pins the suffix rule itself: the last four
// runes of the plaintext, rune-sliced (a byte slice could split a
// multi-byte character), and nothing at all for a key so short the suffix
// would be (nearly) the whole secret — a suffix must never become a second
// copy of the key.
func TestKeySuffixForDerivation(t *testing.T) {
	cases := []struct {
		name      string
		plaintext string
		want      string
	}{
		{"typical key", "sk-abcdefghijklmnopqrstuvwxyz1234", "1234"},
		{"exactly four runes is the whole key", "abcd", ""},
		{"shorter than four", "abc", ""},
		{"multi-byte runes are not split", "sk-αβγδxyzwщ", "yzwщ"},
	}
	for _, tc := range cases {
		if got := KeySuffixFor(tc.plaintext); got != tc.want {
			t.Errorf("%s: KeySuffixFor = %q, want %q", tc.name, got, tc.want)
		}
	}
	// Valid UTF-8 round-trip: a byte-sliced suffix of multi-byte text would
	// be invalid UTF-8 and marshal through JSON as U+FFFD.
	if got := KeySuffixFor("sk-αβγδxyzwщ"); !utf8.ValidString(got) {
		t.Errorf("suffix %q is not valid UTF-8", got)
	}
}

// TestCreateProviderStoresKeySuffixAndListExposesIt covers the new-key
// leg through the public service path: creating a provider (whose first
// key rides along) stores the plaintext's last four characters at insert,
// and the list/detail API carries prefix AND suffix together — the two
// ends of the display pair.
func TestCreateProviderStoresKeySuffixAndListExposesIt(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	plaintext := "sk-abcdefghijklmnopqrstuvwxyz1234"

	view, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "suffix-holder", BaseURL: "https://suffix-holder.example.com",
		KeyLabel: "k1", KeyPlaintext: plaintext, TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	key, err := repository.FindProviderKeyByLabel(db, view.ID, "k1")
	if err != nil {
		t.Fatalf("reload the created key: %v", err)
	}
	if want := "1234"; key.KeySuffix != want {
		t.Errorf("stored key_suffix = %q, want the plaintext's last four %q", key.KeySuffix, want)
	}
	if key.KeyPrefix == "" {
		t.Error("key_prefix must keep its existing derivation alongside the new suffix")
	}

	list, err := svc.ListProviders()
	if err != nil {
		t.Fatalf("ListProviders failed: %v", err)
	}
	if len(list) != 1 || len(list[0].Keys) != 1 {
		t.Fatalf("list must show the one provider with its one key, got %d/%d", len(list), len(list[0].Keys))
	}
	kv := list[0].Keys[0]
	if kv.KeySuffix != "1234" {
		t.Errorf("list API key_suffix = %q, want 1234", kv.KeySuffix)
	}
	if kv.KeyPrefix != key.KeyPrefix {
		t.Errorf("list API key_prefix = %q, want the stored %q", kv.KeyPrefix, key.KeyPrefix)
	}
}

// TestCreateProviderKeyAndSwapUpdateSuffix covers the other two write
// paths: appending a second key stores its own suffix, and replacing an
// existing key's plaintext (edit-with-new-key) swaps prefix AND suffix to
// the new credential's — a stale suffix on a swapped key would misidentify
// every future reference to it.
func TestCreateProviderKeyAndSwapUpdateSuffix(t *testing.T) {
	svc, db, _ := newTestProviderService(t)
	view, err := svc.CreateProvider(context.Background(), CreateProviderInput{
		Name: "swap-holder", BaseURL: "https://swap-holder.example.com",
		KeyLabel: "k1", KeyPlaintext: "sk-first-credential-aaaa1111",
		TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}

	appended, err := svc.CreateProviderKey(context.Background(), view.ID, CreateKeyInput{
		Label: "k2", Plaintext: "sk-second-credential-bbbb2222", TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProviderKey failed: %v", err)
	}
	if appended.KeySuffix != "2222" {
		t.Errorf("appended key suffix = %q, want 2222", appended.KeySuffix)
	}

	newPlaintext := "sk-rotated-credential-cccc3333"
	newPtr := newPlaintext
	swapped, err := svc.UpdateProviderKey(context.Background(), view.ID, appended.ID, UpdateKeyInput{
		Label: "k2", Plaintext: &newPtr, TestModel: "gpt-4o-mini",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("UpdateProviderKey (plaintext swap) failed: %v", err)
	}
	if swapped.KeySuffix != "3333" {
		t.Errorf("swapped key suffix = %q, want the new credential's 3333", swapped.KeySuffix)
	}
	if swapped.KeyPrefix == appended.KeyPrefix {
		t.Errorf("swapped key prefix %q must change with the credential like the suffix does", swapped.KeyPrefix)
	}

	// The stored row agrees with the returned view.
	key, err := repository.FindProviderKeyByID(db, appended.ID)
	if err != nil {
		t.Fatalf("reload the swapped key: %v", err)
	}
	if key.KeySuffix != "3333" {
		t.Errorf("stored swapped key_suffix = %q, want 3333", key.KeySuffix)
	}
}

// TestLegacyEmptySuffixRowSurvivesView pins the empty-suffix boundary
// from the backend side: a row whose suffix is still empty (the pre-column legacy
// state the backfill has not reached) renders through the view with an
// empty suffix beside a present prefix — no error, no invented value.
func TestLegacyEmptySuffixRowSurvivesView(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	provider := &model.Provider{Name: "legacy-holder", ProviderType: "openai",
		BaseURL: "https://legacy.example.com", ManagementStatus: model.ProviderStatusEnabled,
		DestinationVersion: 1, CreatedAt: now, UpdatedAt: now}
	legacy := &model.ProviderKey{ProviderID: provider.ID, Label: "old-key",
		EncryptedKey: "legacy-ciphertext", KeyPrefix: "sk-legacy", KeySuffix: "",
		SortOrder: 1, TestModel: "gpt-4o-mini", ManagementStatus: model.ProviderKeyStatusDisabled,
		VerificationStatus: model.VerificationStatusUntested, AuthorizedDestinationVersion: 1,
		ConfigVersion: 1, TestGeneration: 0, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateProviderWithKey(db, provider, legacy); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	view := toKeyView(*legacy, provider.DestinationVersion)
	if view.KeySuffix != "" {
		t.Errorf("legacy row's view suffix = %q, want the empty not-yet-derived marker", view.KeySuffix)
	}
	if view.KeyPrefix != "sk-legacy" {
		t.Errorf("legacy row's view prefix = %q, want sk-legacy", view.KeyPrefix)
	}
}
