package repository

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

func seedProviderWithKey(t *testing.T, db *gorm.DB, name string) (*model.Provider, *model.ProviderKey) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	provider := &model.Provider{
		Name: name, ProviderType: "openai", BaseURL: "https://api.example.com/v1",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	key := &model.ProviderKey{
		Label: "primary", EncryptedKey: "ciphertext", KeyPrefix: "sk-abc", TestModel: "gpt-4o-mini",
		SortOrder: 1, ManagementStatus: model.ProviderKeyStatusEnabled,
		AuthorizedDestinationVersion: 1, ConfigVersion: 1, TestGeneration: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := CreateProviderWithKey(db, provider, key); err != nil {
		t.Fatalf("CreateProviderWithKey failed: %v", err)
	}
	return provider, key
}

func TestCreateProviderWithKeyPersistsBothRowsInOneTransaction(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, key := seedProviderWithKey(t, db, "openai-main")

	if provider.ID == 0 {
		t.Fatalf("expected provider.ID to be populated after create")
	}
	if key.ID == 0 || key.ProviderID != provider.ID {
		t.Fatalf("expected key.ProviderID=%d, got %d (key.ID=%d)", provider.ID, key.ProviderID, key.ID)
	}
}

// TestMarkProviderKeyVerificationFailedIfCurrentCAS: the gateway
// invalidates a key after a real upstream 401 by CAS-writing
// verification_status=Failed, but ONLY when the row still matches (Passed +
// the destination the key was just sent to). A destination mismatch (admin
// changed BaseURL mid-request) or a no-longer-Passed status must report
// applied=false so the gateway never clobbers a concurrent edit.
func TestMarkProviderKeyVerificationFailedIfCurrentCAS(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	provider, key := seedProviderWithKey(t, db, "openai-main")

	// Seed the key as Passed for the current destination — the state a key
	// must be in before the gateway would ever route traffic to it.
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", key.ID).
		Update("verification_status", model.VerificationStatusPassed).Error; err != nil {
		t.Fatalf("seed passed: %v", err)
	}

	// Happy path: Passed + destination matches -> flipped to Failed.
	applied, err := MarkProviderKeyVerificationFailedIfCurrent(db, key.ID, provider.DestinationVersion, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !applied {
		t.Fatal("expected CAS to apply on a Passed key at the matching destination")
	}
	var reloaded model.ProviderKey
	if err := db.First(&reloaded, key.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("expected verification_status=Failed, got %d", reloaded.VerificationStatus)
	}

	// CAS guard: destination mismatch -> not applied, row untouched.
	_, key2 := seedProviderWithKey(t, db, "openai-other")
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", key2.ID).
		Update("verification_status", model.VerificationStatusPassed).Error; err != nil {
		t.Fatalf("seed passed: %v", err)
	}
	applied, err = MarkProviderKeyVerificationFailedIfCurrent(db, key2.ID, provider.DestinationVersion+999, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied {
		t.Fatal("expected CAS NOT to apply when destination mismatches")
	}
	var reloaded2 model.ProviderKey
	if err := db.First(&reloaded2, key2.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded2.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("row should be untouched (still Passed), got %d", reloaded2.VerificationStatus)
	}
}

func TestFindProviderByIDReturnsNotFoundForMissingID(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, err := FindProviderByID(db, 9999)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound, got %v", err)
	}
}

func TestFindProviderByNameIsCaseSensitiveExactMatch(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedProviderWithKey(t, db, "openai-main")

	if _, err := FindProviderByName(db, "openai-main"); err != nil {
		t.Fatalf("expected to find provider by exact name, got: %v", err)
	}
	if _, err := FindProviderByName(db, "nonexistent"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound for a name that doesn't exist, got %v", err)
	}
}

func TestListProvidersReturnsAllRows(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedProviderWithKey(t, db, "provider-a")
	seedProviderWithKey(t, db, "provider-b")

	list, err := ListProviders(db)
	if err != nil {
		t.Fatalf("ListProviders failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(list))
	}
}

func TestUpdateProviderBaseURLAtomicallyBumpsDestinationVersion(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "openai-main")
	now := time.Now().UTC().Truncate(time.Second)

	newVersion, err := UpdateProviderBaseURL(db, provider.ID, "https://new.example.com/v1", now)
	if err != nil {
		t.Fatalf("UpdateProviderBaseURL failed: %v", err)
	}
	if newVersion != 2 {
		t.Fatalf("expected destination_version to become 2, got %d", newVersion)
	}

	reloaded, err := FindProviderByID(db, provider.ID)
	if err != nil {
		t.Fatalf("FindProviderByID failed: %v", err)
	}
	if reloaded.BaseURL != "https://new.example.com/v1" || reloaded.DestinationVersion != 2 {
		t.Fatalf("expected base_url updated and destination_version=2, got base_url=%q version=%d", reloaded.BaseURL, reloaded.DestinationVersion)
	}
}

func TestUpdateProviderNameNoteDoesNotTouchDestinationVersion(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "openai-main")
	now := time.Now().UTC().Truncate(time.Second)

	if err := UpdateProviderNameNote(db, provider.ID, "renamed", "a note", now); err != nil {
		t.Fatalf("UpdateProviderNameNote failed: %v", err)
	}
	reloaded, err := FindProviderByID(db, provider.ID)
	if err != nil {
		t.Fatalf("FindProviderByID failed: %v", err)
	}
	if reloaded.Name != "renamed" || reloaded.Note != "a note" || reloaded.DestinationVersion != 1 {
		t.Fatalf("expected name/note updated and destination_version unchanged, got name=%q note=%q version=%d", reloaded.Name, reloaded.Note, reloaded.DestinationVersion)
	}
}

func TestFindProviderKeyByIDReturnsNotFoundForMissingID(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	if _, err := FindProviderKeyByID(db, 9999); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound, got %v", err)
	}
}

func TestFindProviderKeyByLabelScopedToProvider(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	providerA, _ := seedProviderWithKey(t, db, "provider-a")
	providerB, _ := seedProviderWithKey(t, db, "provider-b")

	if _, err := FindProviderKeyByLabel(db, providerA.ID, "primary"); err != nil {
		t.Fatalf("expected to find label \"primary\" under provider A, got: %v", err)
	}
	if _, err := FindProviderKeyByLabel(db, providerB.ID, "primary"); err != nil {
		t.Fatalf("expected to find label \"primary\" under provider B independently, got: %v", err)
	}
	if _, err := FindProviderKeyByLabel(db, providerA.ID, "nonexistent"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound, got %v", err)
	}
}

func TestListProviderKeysByProviderOrdersBySortOrder(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "provider-a") // sort_order 1, label "primary"
	now := time.Now().UTC().Truncate(time.Second)
	second := &model.ProviderKey{
		ProviderID: provider.ID, Label: "secondary", EncryptedKey: "ct2", KeyPrefix: "sk-def", TestModel: "gpt-4o-mini",
		SortOrder: 2, ManagementStatus: model.ProviderKeyStatusEnabled,
		AuthorizedDestinationVersion: 1, ConfigVersion: 1, TestGeneration: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := CreateProviderKeyPendingTest(db, second, now); err != nil {
		t.Fatalf("CreateProviderKeyPendingTest failed: %v", err)
	}

	keys, err := ListProviderKeysByProvider(db, provider.ID)
	if err != nil {
		t.Fatalf("ListProviderKeysByProvider failed: %v", err)
	}
	if len(keys) != 2 || keys[0].Label != "primary" || keys[1].Label != "secondary" {
		t.Fatalf("expected [primary, secondary] in sort_order, got %+v", keys)
	}
}

func TestListProviderKeysByProviderIDsGroupsAcrossMultipleProviders(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	providerA, keyA := seedProviderWithKey(t, db, "provider-a")
	providerB, keyB := seedProviderWithKey(t, db, "provider-b")

	keys, err := ListProviderKeysByProviderIDs(db, []uint{providerA.ID, providerB.ID})
	if err != nil {
		t.Fatalf("ListProviderKeysByProviderIDs failed: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys across both providers, got %d", len(keys))
	}
	if keys[0].ProviderID != providerA.ID || keys[0].ID != keyA.ID {
		t.Fatalf("expected first key to belong to provider A (id %d), got provider_id=%d id=%d", providerA.ID, keys[0].ProviderID, keys[0].ID)
	}
	if keys[1].ProviderID != providerB.ID || keys[1].ID != keyB.ID {
		t.Fatalf("expected second key to belong to provider B (id %d), got provider_id=%d id=%d", providerB.ID, keys[1].ProviderID, keys[1].ID)
	}
}

func TestListProviderKeysByProviderIDsReturnsNilForEmptyInput(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	keys, err := ListProviderKeysByProviderIDs(db, nil)
	if err != nil {
		t.Fatalf("expected no error for empty id list, got: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected no keys for an empty id list, got %d", len(keys))
	}
}

func TestNextSortOrderIncrementsFromCurrentMax(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "provider-a") // existing sort_order=1

	next, err := NextSortOrder(db, provider.ID)
	if err != nil {
		t.Fatalf("NextSortOrder failed: %v", err)
	}
	if next != 2 {
		t.Fatalf("expected 2, got %d", next)
	}
}

func TestNextSortOrderStartsAtOneForEmptyProvider(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	provider := &model.Provider{Name: "empty", ProviderType: "openai", BaseURL: "https://a.example.com",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(provider).Error; err != nil {
		t.Fatalf("create empty provider: %v", err)
	}

	next, err := NextSortOrder(db, provider.ID)
	if err != nil {
		t.Fatalf("NextSortOrder failed: %v", err)
	}
	if next != 1 {
		t.Fatalf("expected 1 for a provider with no keys yet, got %d", next)
	}
}

func TestCreateProviderKeyPendingTestSnapshotsCurrentDestinationVersion(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := UpdateProviderBaseURL(db, provider.ID, "https://changed.example.com", now); err != nil {
		t.Fatalf("UpdateProviderBaseURL failed: %v", err)
	} // destination_version is now 2

	newKey := &model.ProviderKey{
		ProviderID: provider.ID, Label: "new-key", EncryptedKey: "ct", KeyPrefix: "sk-xyz", TestModel: "gpt-4o-mini",
		SortOrder: 2, ManagementStatus: model.ProviderKeyStatusDisabled,
		VerificationStatus: model.VerificationStatusUntested,
		CreatedAt:          now, UpdatedAt: now,
	}
	snapshot, err := CreateProviderKeyPendingTest(db, newKey, now)
	if err != nil {
		t.Fatalf("CreateProviderKeyPendingTest failed: %v", err)
	}
	if snapshot != 2 {
		t.Fatalf("expected snapshot version 2, got %d", snapshot)
	}
	if newKey.ID == 0 {
		t.Fatalf("expected key.ID to be populated")
	}
	if newKey.AuthorizedDestinationVersion != 2 || newKey.ConfigVersion != 1 || newKey.TestGeneration != 1 {
		t.Fatalf("expected authorized_destination_version=2 config_version=1 test_generation=1, got %+v", newKey)
	}
}

// TestCreateProviderKeyPendingTestSortOrderCollisionErrorNamesSortOrder is
// the direct regression test for: isUniqueViolation couldn't distinguish UNIQUE(provider_id, label) from
// UNIQUE(provider_id, sort_order) violations on this table, so a
// concurrent "add key" race that only collided on sort_order (an internal
// bookkeeping value the caller never chose) was misreported to the admin
// as their chosen label being taken. This pins down that the real driver
// error text for a sort_order collision actually contains "sort_order" —
// the fact isSortOrderUniqueViolation (provider_service.go) relies on to
// tell the two constraints apart and retry only the sort_order case.
func TestCreateProviderKeyPendingTestSortOrderCollisionErrorNamesSortOrder(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, existingKey := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	colliding := &model.ProviderKey{
		ProviderID: provider.ID, Label: "a-different-label", EncryptedKey: "ct", KeyPrefix: "sk-xyz", TestModel: "gpt-4o-mini",
		SortOrder: existingKey.SortOrder, ManagementStatus: model.ProviderKeyStatusDisabled,
		VerificationStatus: model.VerificationStatusUntested,
		CreatedAt:          now, UpdatedAt: now,
	}
	_, err := CreateProviderKeyPendingTest(db, colliding, now)
	if err == nil {
		t.Fatalf("expected a UNIQUE(provider_id, sort_order) violation, got no error")
	}
	if !strings.Contains(err.Error(), "sort_order") {
		t.Fatalf("expected the error to name sort_order (not label), got: %v", err)
	}
}

func TestSwapProviderKeyPlaintextAtomicallyWritesEverythingAndSnapshotsVersion(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, key := seedProviderWithKey(t, db, "provider-a")
	// Simulate the key having previously passed, to prove the swap resets it.
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", key.ID).
		Update("verification_status", model.VerificationStatusPassed).Error; err != nil {
		t.Fatalf("seed verification_status=passed: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)

	configVersion, testGeneration, snapshot, err := SwapProviderKeyPlaintext(db, key.ID,
		"renamed", "gpt-4o", "new-ciphertext", "sk-new", model.ProviderKeyStatusDisabled, now)
	if err != nil {
		t.Fatalf("SwapProviderKeyPlaintext failed: %v", err)
	}
	if configVersion != 2 {
		t.Fatalf("expected config_version bumped to 2, got %d", configVersion)
	}
	if testGeneration != 2 {
		t.Fatalf("expected test_generation claimed to 2 (key was seeded with test_generation=1), got %d", testGeneration)
	}
	if snapshot != provider.DestinationVersion {
		t.Fatalf("expected snapshot %d to equal provider's current destination_version, got %d", provider.DestinationVersion, snapshot)
	}

	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.VerificationStatus != model.VerificationStatusUntested {
		t.Fatalf("expected verification_status forced to untested, got %d", reloaded.VerificationStatus)
	}
	// The whole point of this function: label,
	// test_model, ciphertext, prefix, and management_status must all land
	// in the SAME atomic write as the version bookkeeping above — not
	// three separate statements a crash could interrupt partway through.
	if reloaded.Label != "renamed" || reloaded.TestModel != "gpt-4o" || reloaded.EncryptedKey != "new-ciphertext" ||
		reloaded.KeyPrefix != "sk-new" || reloaded.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected label/test_model/encrypted_key/key_prefix/management_status all written atomically, got %+v", reloaded)
	}
}

func TestBeginProviderKeyRetestClaimsGenerationAndSnapshotsEncryptedKeyWithoutTouchingConfigVersionOrStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", key.ID).
		Update("verification_status", model.VerificationStatusPassed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	configVersion, testGeneration, encryptedKey, err := BeginProviderKeyRetest(db, key.ID)
	if err != nil {
		t.Fatalf("BeginProviderKeyRetest failed: %v", err)
	}
	if configVersion != 1 {
		t.Fatalf("expected config_version unchanged at 1, got %d", configVersion)
	}
	if testGeneration != 2 {
		t.Fatalf("expected test_generation claimed to 2, got %d", testGeneration)
	}
	if encryptedKey != "ciphertext" {
		t.Fatalf("expected the claim to atomically return the current encrypted_key snapshot %q, got %q", "ciphertext", encryptedKey)
	}

	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status untouched by a retest claim, got %d", reloaded.VerificationStatus)
	}
}

func TestCommitProviderKeyPlaintextTestResultAppliesWhenCASMatches(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	configVersion, testGeneration, snapshot, err := SwapProviderKeyPlaintext(db, key.ID,
		key.Label, key.TestModel, "ciphertext-v2", "sk-v2", model.ProviderKeyStatusDisabled, now)
	if err != nil {
		t.Fatalf("SwapProviderKeyPlaintext failed: %v", err)
	}

	applied, err := CommitProviderKeyPlaintextTestResult(db, key.ID, configVersion, testGeneration, snapshot,
		true, model.VerificationStatusPassed, new(0), "gpt-4o-mini", 123, now)
	if err != nil {
		t.Fatalf("CommitProviderKeyPlaintextTestResult failed: %v", err)
	}
	if !applied {
		t.Fatalf("expected the CAS write to apply")
	}

	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.VerificationStatus != model.VerificationStatusPassed {
		t.Fatalf("expected verification_status=passed, got %d", reloaded.VerificationStatus)
	}
	if reloaded.AuthorizedDestinationVersion != provider.DestinationVersion {
		t.Fatalf("expected authorized_destination_version=%d, got %d", provider.DestinationVersion, reloaded.AuthorizedDestinationVersion)
	}
}

func TestCommitProviderKeyPlaintextTestResultDiscardsWhenAddressChangedMidTest(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	configVersion, testGeneration, snapshot, err := SwapProviderKeyPlaintext(db, key.ID,
		key.Label, key.TestModel, "ciphertext-v2", "sk-v2", model.ProviderKeyStatusDisabled, now)
	if err != nil {
		t.Fatalf("SwapProviderKeyPlaintext failed: %v", err)
	}
	// Simulate the address changing WHILE the (fake, out-of-transaction)
	// network test was in flight.
	if _, err := UpdateProviderBaseURL(db, provider.ID, "https://raced.example.com", now); err != nil {
		t.Fatalf("UpdateProviderBaseURL failed: %v", err)
	}

	applied, err := CommitProviderKeyPlaintextTestResult(db, key.ID, configVersion, testGeneration, snapshot,
		true, model.VerificationStatusPassed, new(0), "gpt-4o-mini", 123, now)
	if err != nil {
		t.Fatalf("CommitProviderKeyPlaintextTestResult failed: %v", err)
	}
	if applied {
		t.Fatalf("expected the CAS write to be discarded because destination_version changed mid-test")
	}
}

func TestCommitProviderKeyPlaintextTestResultPreservesVerificationWhenNotOverwriting(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	configVersion, testGeneration, snapshot, err := SwapProviderKeyPlaintext(db, key.ID,
		key.Label, key.TestModel, "ciphertext-v2", "sk-v2", model.ProviderKeyStatusDisabled, now)
	if err != nil {
		t.Fatalf("SwapProviderKeyPlaintext failed: %v", err)
	}
	// overwriteVerification=false simulates a TestModelNotFound/TestRateLimited
	// outcome on a BRAND NEW plaintext — verification_status must stay at the
	// "untested" value SwapProviderKeyPlaintext already forced, not
	// whatever verificationStatus value is passed here.
	applied, err := CommitProviderKeyPlaintextTestResult(db, key.ID, configVersion, testGeneration, snapshot,
		false, model.VerificationStatusPassed, new(3), "gpt-4o-mini", 50, now)
	if err != nil {
		t.Fatalf("CommitProviderKeyPlaintextTestResult failed: %v", err)
	}
	if !applied {
		t.Fatalf("expected the CAS write to apply")
	}

	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.VerificationStatus != model.VerificationStatusUntested {
		t.Fatalf("expected verification_status to remain untested (not overwritten), got %d", reloaded.VerificationStatus)
	}
	if reloaded.LastTestModel != "gpt-4o-mini" {
		t.Fatalf("expected last_test_model still recorded even when verification_status is preserved, got %q", reloaded.LastTestModel)
	}
}

func TestCommitProviderKeyRetestResultDiscardsWhenGenerationStale(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	configVersion, testGeneration, _, err := BeginProviderKeyRetest(db, key.ID)
	if err != nil {
		t.Fatalf("BeginProviderKeyRetest failed: %v", err)
	}
	// A second, later-claimed retest race — bumps test_generation further.
	if _, _, _, err := BeginProviderKeyRetest(db, key.ID); err != nil {
		t.Fatalf("second BeginProviderKeyRetest failed: %v", err)
	}

	applied, err := CommitProviderKeyRetestResult(db, key.ID, configVersion, testGeneration,
		true, model.VerificationStatusPassed, new(0), "gpt-4o-mini", 100, now)
	if err != nil {
		t.Fatalf("CommitProviderKeyRetestResult failed: %v", err)
	}
	if applied {
		t.Fatalf("expected the stale-generation write to be discarded")
	}
}

func TestCommitProviderKeyRetestResultAppliesWhenCASMatches(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	configVersion, testGeneration, _, err := BeginProviderKeyRetest(db, key.ID)
	if err != nil {
		t.Fatalf("BeginProviderKeyRetest failed: %v", err)
	}

	applied, err := CommitProviderKeyRetestResult(db, key.ID, configVersion, testGeneration,
		true, model.VerificationStatusFailed, new(1), "gpt-4o-mini", 200, now)
	if err != nil {
		t.Fatalf("CommitProviderKeyRetestResult failed: %v", err)
	}
	if !applied {
		t.Fatalf("expected the CAS write to apply")
	}
	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("expected verification_status=failed, got %d", reloaded.VerificationStatus)
	}
}

func TestUpdateProviderKeyLabelAndStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	if err := UpdateProviderKeyLabelAndStatus(db, key.ID, "renamed-label", "gpt-4o", model.ProviderKeyStatusDisabled, now); err != nil {
		t.Fatalf("UpdateProviderKeyLabelAndStatus failed: %v", err)
	}
	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.Label != "renamed-label" || reloaded.TestModel != "gpt-4o" || reloaded.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected label/test_model/status updated, got %+v", reloaded)
	}
}

// TestUpdateProviderKeyLabelAndStatusIfVerifiedAppliesWhenVerified and
// TestUpdateProviderKeyLabelAndStatusIfVerifiedSkipsWhenStale are the
// direct regression tests for: UpdateProviderKey's enable path used to check verifyKeyEnableAllowed
// (verification_status/authorized_destination_version) and then write
// management_status with a plain, unconditional UPDATE — a
// check-then-act gap where a concurrent base_url change or retest could
// invalidate the key in between. This proves the CAS-guarded write both
// applies when the guard columns still match, and correctly skips (rather
// than silently committing a stale enable) once they no longer do.
func TestUpdateProviderKeyLabelAndStatusIfVerifiedAppliesWhenVerified(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", key.ID).
		Update("verification_status", model.VerificationStatusPassed).Error; err != nil {
		t.Fatalf("seed verification_status failed: %v", err)
	}

	applied, err := UpdateProviderKeyLabelAndStatusIfVerified(db, key.ID, "renamed", "gpt-4o", model.ProviderKeyStatusEnabled,
		model.VerificationStatusPassed, provider.DestinationVersion, now)
	if err != nil {
		t.Fatalf("UpdateProviderKeyLabelAndStatusIfVerified failed: %v", err)
	}
	if !applied {
		t.Fatalf("expected the CAS to apply when verification_status/authorized_destination_version match")
	}
	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.Label != "renamed" || reloaded.ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Fatalf("expected label/status updated, got %+v", reloaded)
	}
}

func TestUpdateProviderKeyLabelAndStatusIfVerifiedSkipsWhenStale(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	// key.VerificationStatus is still Untested (seedProviderWithKey's
	// default) — simulating a concurrent retest/base_url change that
	// invalidated the key between the service layer's
	// verifyKeyEnableAllowed check and this write.
	applied, err := UpdateProviderKeyLabelAndStatusIfVerified(db, key.ID, "renamed", "gpt-4o", model.ProviderKeyStatusEnabled,
		model.VerificationStatusPassed, 1, now)
	if err != nil {
		t.Fatalf("UpdateProviderKeyLabelAndStatusIfVerified failed: %v", err)
	}
	if applied {
		t.Fatalf("expected the CAS to skip once verification_status no longer matches expected")
	}
	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.Label == "renamed" {
		t.Fatalf("expected the write to be skipped, but label was changed to %q", reloaded.Label)
	}
}

func TestSetProviderKeyManagementStatusIfVerifiedAppliesWhenVerified(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	if err := db.Model(&model.ProviderKey{}).Where("id = ?", key.ID).
		Update("verification_status", model.VerificationStatusPassed).Error; err != nil {
		t.Fatalf("seed verification_status failed: %v", err)
	}

	applied, err := SetProviderKeyManagementStatusIfVerified(db, key.ID, model.ProviderKeyStatusEnabled,
		model.VerificationStatusPassed, provider.DestinationVersion, now)
	if err != nil {
		t.Fatalf("SetProviderKeyManagementStatusIfVerified failed: %v", err)
	}
	if !applied {
		t.Fatalf("expected the CAS to apply when verification_status/authorized_destination_version match")
	}
}

func TestSetProviderKeyManagementStatusIfVerifiedSkipsWhenStale(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	// seedProviderWithKey defaults to management_status=Enabled, so start
	// from Disabled to make an unexpected Enabled result after the CAS
	// attempt unambiguous evidence that it wrongly applied.
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	if err := SetProviderKeyManagementStatus(db, key.ID, model.ProviderKeyStatusDisabled, now); err != nil {
		t.Fatalf("SetProviderKeyManagementStatus failed: %v", err)
	}

	// key.VerificationStatus is still Untested (seedProviderWithKey's
	// default) — simulating a concurrent retest/base_url change that
	// invalidated the key between the service layer's
	// verifyKeyEnableAllowed check and this write.
	applied, err := SetProviderKeyManagementStatusIfVerified(db, key.ID, model.ProviderKeyStatusEnabled,
		model.VerificationStatusPassed, 1, now)
	if err != nil {
		t.Fatalf("SetProviderKeyManagementStatusIfVerified failed: %v", err)
	}
	if applied {
		t.Fatalf("expected the CAS to skip once verification_status no longer matches expected")
	}
	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected the write to be skipped (still Disabled), but management_status was changed to %d", reloaded.ManagementStatus)
	}
}

func TestSetProviderKeyManagementStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	if err := SetProviderKeyManagementStatus(db, key.ID, model.ProviderKeyStatusDisabled, now); err != nil {
		t.Fatalf("SetProviderKeyManagementStatus failed: %v", err)
	}
	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Fatalf("expected management_status=disabled, got %d", reloaded.ManagementStatus)
	}
}

// TestCASProviderKeyManagementStatusSkipsWhenCurrentValueChanged is the
// direct regression test for: runNewPlaintextTestAndCommit's final "enable/disable per the original
// request" write used to be an unconditional SetProviderKeyManagementStatus
// call, so a legitimate concurrent PATCH .../status change landing between
// the CAS-committed verification result and that write could be silently
// clobbered by the stale plaintext-test request's own intent. This proves
// CASProviderKeyManagementStatus correctly refuses to apply — and leaves
// the concurrent write's value in place — once management_status no
// longer matches the value the caller last observed.
func TestCASProviderKeyManagementStatusSkipsWhenCurrentValueChanged(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	// Simulate a concurrent PATCH .../status call that already moved the
	// row away from Disabled (the value the stale caller still expects)
	// before the stale caller's own CAS-guarded write runs.
	if err := SetProviderKeyManagementStatus(db, key.ID, model.ProviderKeyStatusEnabled, now); err != nil {
		t.Fatalf("SetProviderKeyManagementStatus failed: %v", err)
	}

	applied, err := CASProviderKeyManagementStatus(db, key.ID, model.ProviderKeyStatusDisabled, model.ProviderKeyStatusDisabled, now)
	if err != nil {
		t.Fatalf("CASProviderKeyManagementStatus failed: %v", err)
	}
	if applied {
		t.Fatalf("expected the CAS to skip once management_status no longer matches expectedCurrent")
	}

	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Fatalf("expected the concurrent write's value (Enabled) to survive untouched, got %d", reloaded.ManagementStatus)
	}
}

func TestCASProviderKeyManagementStatusAppliesWhenCurrentValueMatches(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	if err := SetProviderKeyManagementStatus(db, key.ID, model.ProviderKeyStatusDisabled, now); err != nil {
		t.Fatalf("SetProviderKeyManagementStatus failed: %v", err)
	}

	applied, err := CASProviderKeyManagementStatus(db, key.ID, model.ProviderKeyStatusDisabled, model.ProviderKeyStatusEnabled, now)
	if err != nil {
		t.Fatalf("CASProviderKeyManagementStatus failed: %v", err)
	}
	if !applied {
		t.Fatalf("expected the CAS to apply when management_status matches expectedCurrent")
	}
	reloaded, err := FindProviderKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("FindProviderKeyByID failed: %v", err)
	}
	if reloaded.ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Fatalf("expected management_status=enabled, got %d", reloaded.ManagementStatus)
	}
}

func TestSwapProviderKeySortOrderSwapsWithNeighbor(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, first := seedProviderWithKey(t, db, "provider-a") // sort_order=1
	now := time.Now().UTC().Truncate(time.Second)
	second := &model.ProviderKey{
		ProviderID: provider.ID, Label: "secondary", EncryptedKey: "ct2", KeyPrefix: "sk-def",
		SortOrder: 2, ManagementStatus: model.ProviderKeyStatusEnabled,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := CreateProviderKeyPendingTest(db, second, now); err != nil {
		t.Fatalf("CreateProviderKeyPendingTest failed: %v", err)
	}

	ok, err := SwapProviderKeySortOrder(db, provider.ID, second.ID, "up", now)
	if err != nil {
		t.Fatalf("SwapProviderKeySortOrder failed: %v", err)
	}
	if !ok {
		t.Fatalf("expected swap to succeed")
	}

	keys, err := ListProviderKeysByProvider(db, provider.ID)
	if err != nil {
		t.Fatalf("ListProviderKeysByProvider failed: %v", err)
	}
	if keys[0].ID != second.ID || keys[1].ID != first.ID {
		t.Fatalf("expected order [second, first] after swapping second up, got [%d, %d]", keys[0].ID, keys[1].ID)
	}
}

func TestSwapProviderKeySortOrderNoOpAtBoundary(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, first := seedProviderWithKey(t, db, "provider-a") // sole key, sort_order=1
	now := time.Now().UTC().Truncate(time.Second)

	ok, err := SwapProviderKeySortOrder(db, provider.ID, first.ID, "up", now)
	if err != nil {
		t.Fatalf("SwapProviderKeySortOrder failed: %v", err)
	}
	if ok {
		t.Fatalf("expected no-op (false) when there's no neighbor to swap with")
	}
}

func TestProviderKeyFingerprintClaimAndGet(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := GetProviderKeyFingerprint(db); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound before any fingerprint is written, got %v", err)
	}

	if err := ClaimProviderKeyFingerprintIfAbsent(db, "encrypted-probe-value", now); err != nil {
		t.Fatalf("ClaimProviderKeyFingerprintIfAbsent failed: %v", err)
	}
	fp, err := GetProviderKeyFingerprint(db)
	if err != nil {
		t.Fatalf("GetProviderKeyFingerprint failed: %v", err)
	}
	if fp.EncryptedProbe != "encrypted-probe-value" {
		t.Fatalf("expected stored probe value, got %q", fp.EncryptedProbe)
	}
}

// TestProviderKeyFingerprintClaimNeverOverwritesExistingRow is the direct
// regression test for: two instances racing on
// first boot with DIFFERENT master keys must not let the later one's claim
// silently overwrite the earlier one's row — ClaimProviderKeyFingerprintIfAbsent
// is a DO NOTHING insert, so the first writer's probe always wins and the
// second caller's own probe is simply discarded (its caller then correctly
// fails the decrypt-verify step in service.VerifyMasterKeyFingerprint,
// covered in its own tests).
// TestCreateProviderWithKeyFailsWhenProviderNameAlreadyExists covers the
// tx.Create(provider) error branch with a genuine DB error (a UNIQUE
// constraint violation), not gorm.ErrRecordNotFound.
func TestCreateProviderWithKeyFailsWhenProviderNameAlreadyExists(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedProviderWithKey(t, db, "duplicate-name")

	now := time.Now().UTC().Truncate(time.Second)
	dupProvider := &model.Provider{
		Name: "duplicate-name", ProviderType: "openai", BaseURL: "https://api.example.com/v1",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	dupKey := &model.ProviderKey{
		Label: "primary", EncryptedKey: "ciphertext", KeyPrefix: "sk-abc", TestModel: "gpt-4o-mini",
		SortOrder: 1, ManagementStatus: model.ProviderKeyStatusEnabled,
		AuthorizedDestinationVersion: 1, ConfigVersion: 1, TestGeneration: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	err := CreateProviderWithKey(db, dupProvider, dupKey)
	if err == nil {
		t.Fatalf("expected a UNIQUE constraint error creating a provider with a duplicate name")
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected a genuine constraint-violation error, not gorm.ErrRecordNotFound, got %v", err)
	}
}

func TestListProvidersReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.CloseDB(t, db)

	if _, err := ListProviders(db); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestUpdateProviderBaseURLReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "provider-a")
	testutil.CloseDB(t, db)

	if _, err := UpdateProviderBaseURL(db, provider.ID, "https://new.example.com", time.Now().UTC()); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestUpdateProviderManagementStatusTogglesStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	if applied, err := UpdateProviderManagementStatus(db, provider.ID, model.ProviderStatusDisabled, now); err != nil {
		t.Fatalf("UpdateProviderManagementStatus failed: %v", err)
	} else if !applied {
		t.Fatalf("expected applied=true for an existing provider")
	}
	reloaded, err := FindProviderByID(db, provider.ID)
	if err != nil {
		t.Fatalf("FindProviderByID failed: %v", err)
	}
	if reloaded.ManagementStatus != model.ProviderStatusDisabled {
		t.Fatalf("expected management_status=disabled, got %d", reloaded.ManagementStatus)
	}
}

// TestUpdateProviderManagementStatusReturnsNotAppliedForUnknownProvider is
// the direct regression test for an efficiency finding:
// SetProviderStatus (service layer) used to do a separate FindProviderByID
// existence check before this write; it now relies on this function's own
// applied=false return instead, so this pins down that RowsAffected==0
// correctly surfaces as applied=false rather than a false "success".
func TestUpdateProviderManagementStatusReturnsNotAppliedForUnknownProvider(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	applied, err := UpdateProviderManagementStatus(db, 999999, model.ProviderStatusDisabled, now)
	if err != nil {
		t.Fatalf("UpdateProviderManagementStatus failed: %v", err)
	}
	if applied {
		t.Fatalf("expected applied=false for a nonexistent provider ID")
	}
}

func TestListProviderKeysByProviderReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "provider-a")
	testutil.CloseDB(t, db)

	if _, err := ListProviderKeysByProvider(db, provider.ID); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestNextSortOrderReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "provider-a")
	testutil.CloseDB(t, db)

	if _, err := NextSortOrder(db, provider.ID); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

// TestCreateProviderKeyPendingTestFailsWhenProviderMissing covers both the
// inner provider-fetch error branch and the outer transaction-error branch
// in one shot: key.ProviderID pointing at a provider row that doesn't exist
// makes the initial tx.Select(...).First(&provider) return
// gorm.ErrRecordNotFound, which propagates straight out of the transaction.
func TestCreateProviderKeyPendingTestFailsWhenProviderMissing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	orphanKey := &model.ProviderKey{
		ProviderID: 9999, Label: "orphan", EncryptedKey: "ct", KeyPrefix: "sk-orphan", TestModel: "gpt-4o-mini",
		SortOrder: 1, ManagementStatus: model.ProviderKeyStatusDisabled,
		CreatedAt: now, UpdatedAt: now,
	}

	_, err := CreateProviderKeyPendingTest(db, orphanKey, now)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound when the parent provider doesn't exist, got %v", err)
	}
}

func TestSwapProviderKeyPlaintextFailsWhenKeyMissing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	_, _, _, err := SwapProviderKeyPlaintext(db, 9999, "label", "gpt-4o", "ct", "sk-x", model.ProviderKeyStatusDisabled, now)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound when the key doesn't exist, got %v", err)
	}
}

// TestSwapProviderKeyPlaintextFailsWhenProviderMissing covers the second
// fetch's error branch (the parent provider row itself is missing) by
// disabling FK enforcement just long enough to orphan an existing key —
// something that can never happen through this repository's own normal
// write paths (providers are never deleted), only exercised
// here to reach a defensive branch that guards against DB-level corruption.
func TestSwapProviderKeyPlaintextFailsWhenProviderMissing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		t.Fatalf("disable foreign_keys: %v", err)
	}
	if err := db.Exec("DELETE FROM providers WHERE id = ?", provider.ID).Error; err != nil {
		t.Fatalf("orphan the key by deleting its parent provider: %v", err)
	}

	_, _, _, err := SwapProviderKeyPlaintext(db, key.ID, "label", "gpt-4o", "ct", "sk-x", model.ProviderKeyStatusDisabled, now)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound when the parent provider row is missing, got %v", err)
	}
}

// TestSwapProviderKeyPlaintextFailsOnLabelUniqueConstraint covers the raw
// UPDATE's own error branch with a genuine DB error: renaming one key to a
// label another key under the SAME provider already holds violates
// UNIQUE(provider_id, label).
func TestSwapProviderKeyPlaintextFailsOnLabelUniqueConstraint(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, first := seedProviderWithKey(t, db, "provider-a") // label "primary"
	now := time.Now().UTC().Truncate(time.Second)
	second := &model.ProviderKey{
		ProviderID: provider.ID, Label: "secondary", EncryptedKey: "ct2", KeyPrefix: "sk-def", TestModel: "gpt-4o-mini",
		SortOrder: 2, ManagementStatus: model.ProviderKeyStatusEnabled,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := CreateProviderKeyPendingTest(db, second, now); err != nil {
		t.Fatalf("CreateProviderKeyPendingTest failed: %v", err)
	}

	_, _, _, err := SwapProviderKeyPlaintext(db, first.ID, "secondary", "gpt-4o", "ct-new", "sk-new", model.ProviderKeyStatusDisabled, now)
	if err == nil {
		t.Fatalf("expected a UNIQUE(provider_id, label) constraint error")
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected a genuine constraint-violation error, not gorm.ErrRecordNotFound, got %v", err)
	}
}

func TestCommitProviderKeyPlaintextTestResultReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	testutil.CloseDB(t, db)

	_, err := CommitProviderKeyPlaintextTestResult(db, key.ID, 1, 1, 1, true, model.VerificationStatusPassed, new(0), "gpt-4o-mini", 1, now)
	if err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestCommitProviderKeyRetestResultReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, key := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	testutil.CloseDB(t, db)

	_, err := CommitProviderKeyRetestResult(db, key.ID, 1, 1, true, model.VerificationStatusPassed, new(0), "gpt-4o-mini", 1, now)
	if err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestSwapProviderKeySortOrderFailsWhenCurrentKeyMissing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, _ := seedProviderWithKey(t, db, "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	_, err := SwapProviderKeySortOrder(db, provider.ID, 9999, "up", now)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound when the key doesn't exist under this provider, got %v", err)
	}
}

func TestSwapProviderKeySortOrderSwapsWithNeighborDownDirection(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, first := seedProviderWithKey(t, db, "provider-a") // sort_order=1
	now := time.Now().UTC().Truncate(time.Second)
	second := &model.ProviderKey{
		ProviderID: provider.ID, Label: "secondary", EncryptedKey: "ct2", KeyPrefix: "sk-def", TestModel: "gpt-4o-mini",
		SortOrder: 2, ManagementStatus: model.ProviderKeyStatusEnabled,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := CreateProviderKeyPendingTest(db, second, now); err != nil {
		t.Fatalf("CreateProviderKeyPendingTest failed: %v", err)
	}

	ok, err := SwapProviderKeySortOrder(db, provider.ID, first.ID, "down", now)
	if err != nil {
		t.Fatalf("SwapProviderKeySortOrder failed: %v", err)
	}
	if !ok {
		t.Fatalf("expected swap to succeed")
	}

	keys, err := ListProviderKeysByProvider(db, provider.ID)
	if err != nil {
		t.Fatalf("ListProviderKeysByProvider failed: %v", err)
	}
	if keys[0].ID != second.ID || keys[1].ID != first.ID {
		t.Fatalf("expected order [second, first] after swapping first down, got [%d, %d]", keys[0].ID, keys[1].ID)
	}
}

// TestSwapProviderKeySortOrderReturnsErrorOnNeighborScanCorruption covers
// the neighborErr-is-a-real-error branch (as opposed to
// gorm.ErrRecordNotFound): a directly-inserted row whose sort_order holds
// non-numeric TEXT ("orphan-marker" cannot be parsed as an integer)
// survives SQLite's type-affinity conversion at INSERT time (a NOT NULL
// INTEGER-affinity column happily stores TEXT that doesn't look like a
// number, per SQLite's manifest typing) and, because SQLite orders the TEXT
// storage class above INTEGER/REAL regardless of content, sorts as the
// "down" neighbor of any numeric sort_order — so it wins that Limit(1)
// query, and scanning "orphan-marker" into ProviderKey.SortOrder (an int)
// fails with a genuine driver conversion error, never gorm.ErrRecordNotFound.
func TestSwapProviderKeySortOrderReturnsErrorOnNeighborScanCorruption(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, first := seedProviderWithKey(t, db, "provider-a") // sort_order=1
	now := time.Now().UTC().Truncate(time.Second)

	if err := db.Exec(`
		INSERT INTO provider_keys
			(provider_id, label, encrypted_key, key_prefix, sort_order, test_model,
			 management_status, verification_status, authorized_destination_version,
			 config_version, test_generation, created_at, updated_at)
		VALUES (?, 'corrupt', 'ct', 'sk-corrupt', 'orphan-marker', 'gpt-4o-mini', ?, 0, 1, 1, 1, ?, ?)
	`, provider.ID, model.ProviderKeyStatusDisabled, now, now).Error; err != nil {
		t.Fatalf("insert corrupt sort_order row: %v", err)
	}

	_, err := SwapProviderKeySortOrder(db, provider.ID, first.ID, "down", now)
	if err == nil {
		t.Fatalf("expected a scan error from the corrupted non-numeric sort_order")
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected a genuine scan/conversion error, not gorm.ErrRecordNotFound, got %v", err)
	}
}

// TestSwapProviderKeySortOrderReturnsErrorWhenCurrentUpdateConflicts covers
// the first Updates(...) call's error branch: current's sort_order is about
// to be set to its own negation as an intermediate step, so pre-seeding a
// third key already sitting at that exact negative value collides with
// UNIQUE(provider_id, sort_order) the instant the first UPDATE runs — a
// genuine DB error, never gorm.ErrRecordNotFound. (The two LATER Updates in
// this same function can never hit an analogous conflict: by the same
// uniqueness invariant, the value each of them writes was, until the
// immediately preceding statement, held exclusively by the row being
// updated itself — so no pre-existing third row can ever already occupy it.
// See this test file's package-level report notes.)
func TestSwapProviderKeySortOrderReturnsErrorWhenCurrentUpdateConflicts(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	provider, first := seedProviderWithKey(t, db, "provider-a") // sort_order=1
	now := time.Now().UTC().Truncate(time.Second)
	second := &model.ProviderKey{
		ProviderID: provider.ID, Label: "secondary", EncryptedKey: "ct2", KeyPrefix: "sk-def", TestModel: "gpt-4o-mini",
		SortOrder: 2, ManagementStatus: model.ProviderKeyStatusEnabled,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := CreateProviderKeyPendingTest(db, second, now); err != nil {
		t.Fatalf("CreateProviderKeyPendingTest failed: %v", err)
	}
	// Pre-occupy first.SortOrder's negation (-1) so the swap's own
	// intermediate write to that value collides.
	conflict := &model.ProviderKey{
		ProviderID: provider.ID, Label: "conflict", EncryptedKey: "ct3", KeyPrefix: "sk-conflict", TestModel: "gpt-4o-mini",
		SortOrder: -1, ManagementStatus: model.ProviderKeyStatusEnabled,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(conflict).Error; err != nil {
		t.Fatalf("seed conflicting sort_order=-1 row: %v", err)
	}

	_, err := SwapProviderKeySortOrder(db, provider.ID, first.ID, "up", now)
	if err == nil {
		t.Fatalf("expected a UNIQUE(provider_id, sort_order) constraint error")
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected a genuine constraint-violation error, not gorm.ErrRecordNotFound, got %v", err)
	}
}

func TestProviderKeyFingerprintClaimNeverOverwritesExistingRow(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := ClaimProviderKeyFingerprintIfAbsent(db, "first-writer-probe", now); err != nil {
		t.Fatalf("first claim failed: %v", err)
	}
	if err := ClaimProviderKeyFingerprintIfAbsent(db, "second-writer-probe", now); err != nil {
		t.Fatalf("second claim failed: %v", err)
	}

	fp, err := GetProviderKeyFingerprint(db)
	if err != nil {
		t.Fatalf("GetProviderKeyFingerprint failed: %v", err)
	}
	if fp.EncryptedProbe != "first-writer-probe" {
		t.Fatalf("expected the first writer's probe to be preserved, got %q", fp.EncryptedProbe)
	}
}
