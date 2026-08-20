package apikey

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func newAPIKeyServiceForTest(t *testing.T) (*APIKeyService, *gorm.DB) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	return NewAPIKeyService(db, testutil.ProviderSecrets()), db
}

func seedModelForAPIKeyTest(t *testing.T, db *gorm.DB, name string) uint {
	t.Helper()
	now := time.Now().UTC()
	m := &model.Model{Name: name, ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	return m.ID
}

func TestCreateAPIKeySucceeds(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "gpt-4o")

	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), Remark: "test key", ModelIDs: []uint{mid}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}
	if !strings.HasPrefix(result.PlaintextKey, "sk-yr-") {
		t.Fatalf("plaintext key missing sk-yr- prefix: %q", result.PlaintextKey)
	}
	if result.APIKey.KeyPrefix != result.PlaintextKey[:apiKeyDisplayChars] {
		t.Fatalf("prefix should be first %d chars of plaintext", apiKeyDisplayChars)
	}
	if len(result.APIKey.ModelIDs) != 1 || result.APIKey.ModelIDs[0] != mid {
		t.Fatalf("whitelist mismatch: %v", result.APIKey.ModelIDs)
	}
	if result.APIKey.DisplayStatus != APIKeyDisplayActive {
		t.Fatalf("expected active display status, got %q", result.APIKey.DisplayStatus)
	}
	// The create response is what the form renders next — it must carry the
	// owner's username like the list/get views do.
	if result.APIKey.OwnerUsername == "" {
		t.Fatalf("create response missing owner_username")
	}

	// The stored row keeps a hash (never the plaintext) and an AES-GCM
	// ciphertext of the plaintext (so it can be revealed again). The hash and
	// the ciphertext must both differ from the plaintext — only the reveal path
	// decrypts the ciphertext back to the plaintext.
	var stored model.APIKey
	if err := db.First(&stored, result.APIKey.ID).Error; err != nil {
		t.Fatalf("load stored key: %v", err)
	}
	if stored.KeyHash == "" || stored.KeyHash == result.PlaintextKey {
		t.Fatalf("key_hash must be a hash, not the plaintext")
	}
	if stored.EncryptedKey == "" || stored.EncryptedKey == result.PlaintextKey {
		t.Fatalf("encrypted_key must be a non-empty ciphertext that is not the plaintext")
	}
}

func TestCreateAPIKeyTreatsZeroLimitAsUnlimited(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	zero := 0
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db),
		ModelIDs: []uint{mid}, RPMLimit: &zero,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	// 0 at create time means "no cap" (same as on the PATCH path) — it must
	// NOT persist as a literal 0 that the gateway would read as "0 allowed".
	var stored model.APIKey
	if err := db.First(&stored, result.APIKey.ID).Error; err != nil {
		t.Fatalf("load stored: %v", err)
	}
	if stored.RPMLimit != nil {
		t.Fatalf("rpm_limit=0 at create must be stored as NULL (no cap), got %d", *stored.RPMLimit)
	}
}

func TestCreateAPIKeyRejectsNonexistentModel(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	_, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db),
		ModelIDs: []uint{999999},
	}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
}

func TestCreateAPIKeyAllowAllModels(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")

	// AllowAllModels wins even if the caller also sends ids — the service owns
	// the invariant, so the join table stays empty regardless of the frontend.
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), AllowAllModels: true, ModelIDs: []uint{mid}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if !result.APIKey.AllowAllModels {
		t.Fatalf("expected AllowAllModels true in view")
	}
	if len(result.APIKey.ModelIDs) != 0 {
		t.Fatalf("all-models key should have no allowlist rows, got %v", result.APIKey.ModelIDs)
	}
	var stored model.APIKey
	if err := db.First(&stored, result.APIKey.ID).Error; err != nil {
		t.Fatalf("load stored: %v", err)
	}
	if !stored.AllowAllModels {
		t.Fatalf("allow_all_models must persist as true")
	}
}

func TestUpdateAPIKeyTogglesAllowAllModels(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	created, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{mid}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Switch to all-models while still sending a populated allowlist — the
	// service must clear it anyway, not trust the caller.
	allow := true
	view, err := svc.UpdateAPIKey(created.APIKey.ID, UpdateAPIKeyInput{
		AllowAllModels: &allow, ModelIDs: []uint{mid},
	}, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("update to allow-all: %v", err)
	}
	if !view.AllowAllModels {
		t.Fatalf("expected AllowAllModels true after update")
	}
	if len(view.ModelIDs) != 0 {
		t.Fatalf("switching to all-models should clear the allowlist, got %v", view.ModelIDs)
	}

	// Switch back to a custom allowlist.
	deny := false
	view2, err := svc.UpdateAPIKey(created.APIKey.ID, UpdateAPIKeyInput{
		AllowAllModels: &deny, ModelIDs: []uint{mid},
	}, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("update to custom: %v", err)
	}
	if view2.AllowAllModels {
		t.Fatalf("expected AllowAllModels false after switching back")
	}
	if len(view2.ModelIDs) != 1 || view2.ModelIDs[0] != mid {
		t.Fatalf("custom allowlist not restored: %v", view2.ModelIDs)
	}
}

func TestCreateAPIKeyRejectsEmptyCustomAllowlist(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	_, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), AllowAllModels: false, ModelIDs: nil}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrAPIKeyEmptyAllowlist) {
		t.Fatalf("expected ErrAPIKeyEmptyAllowlist, got %v", err)
	}
}

func TestUpdateAPIKeyRejectsEmptyCustomAllowlist(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	created, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{mid}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Switching to custom scope while clearing the allowlist must be rejected.
	deny := false
	if _, err := svc.UpdateAPIKey(created.APIKey.ID, UpdateAPIKeyInput{
		AllowAllModels: &deny, ModelIDs: []uint{},
	}, nil, time.Now().UTC()); !errors.Is(err, errcode.ErrAPIKeyEmptyAllowlist) {
		t.Fatalf("expected ErrAPIKeyEmptyAllowlist, got %v", err)
	}

	// The rejected update rolls back — the original allowlist stays intact.
	view, err := svc.GetAPIKey(created.APIKey.ID, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(view.ModelIDs) != 1 || view.ModelIDs[0] != mid {
		t.Fatalf("rejected update must leave the allowlist intact, got %v", view.ModelIDs)
	}
}

func TestUpdateAPIKeyRejectsAllToCustomWithoutModels(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	created, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), AllowAllModels: true}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A sparse PATCH flipping all-models off without supplying any ids would
	// leave a custom key with an empty allowlist — it can call nothing.
	deny := false
	if _, err := svc.UpdateAPIKey(created.APIKey.ID, UpdateAPIKeyInput{
		AllowAllModels: &deny,
	}, nil, time.Now().UTC()); !errors.Is(err, errcode.ErrAPIKeyEmptyAllowlist) {
		t.Fatalf("expected ErrAPIKeyEmptyAllowlist, got %v", err)
	}

	// The rejected transition rolls back — the key stays all-models.
	view, err := svc.GetAPIKey(created.APIKey.ID, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !view.AllowAllModels {
		t.Fatalf("rejected transition must roll back; key should stay all-models")
	}
}

func TestUpdateAPIKeyScopeOmittedLeavesAllowlist(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	created, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{mid}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A remark-only PATCH (no scope flag, no model_ids) must leave the custom
	// allowlist intact — it must never force-clear off a stale flag read.
	remark := "note"
	view, err := svc.UpdateAPIKey(created.APIKey.ID, UpdateAPIKeyInput{Remark: &remark}, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("update remark: %v", err)
	}
	if view.AllowAllModels {
		t.Fatalf("remark-only update must not flip AllowAllModels")
	}
	if len(view.ModelIDs) != 1 || view.ModelIDs[0] != mid {
		t.Fatalf("remark-only update must preserve the allowlist, got %v", view.ModelIDs)
	}
}

func TestGetAPIKeyReturnsWhitelist(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	m1 := seedModelForAPIKeyTest(t, db, "m1")
	m2 := seedModelForAPIKeyTest(t, db, "m2")
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{m1, m2}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	detail, err := svc.GetAPIKey(result.APIKey.ID, nil)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if len(detail.ModelIDs) != 2 {
		t.Fatalf("expected 2 whitelisted models, got %v", detail.ModelIDs)
	}
}

func TestGetAPIKeyNotFound(t *testing.T) {
	svc, _ := newAPIKeyServiceForTest(t)
	_, err := svc.GetAPIKey(999999, nil)
	if !errors.Is(err, errcode.ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

// TestGetAPIKeyPlaintextRoundTripsTheCreatePlaintext verifies the reveal path
// decrypts back exactly the plaintext handed out at create time — the
// encrypted_key column must round-trip through AES-GCM with the service's
// secrets box. This is the core contract the list-page copy button depends on.
func TestGetAPIKeyPlaintextRoundTripsTheCreatePlaintext(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{mid}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	revealed, err := svc.GetAPIKeyPlaintext(result.APIKey.ID, nil)
	if err != nil {
		t.Fatalf("GetAPIKeyPlaintext: %v", err)
	}
	if revealed != result.PlaintextKey {
		t.Fatalf("revealed plaintext %q does not match the create-time plaintext %q", revealed, result.PlaintextKey)
	}
}

// TestGetAPIKeyPlaintextUnavailableForLegacyRow seeds a row the way pre-00021
// keys look (no encrypted_key) and asserts the reveal path returns the
// dedicated code rather than an empty string or a decrypt error — so the
// frontend can show "this key predates the feature, please create a new one".
func TestGetAPIKeyPlaintextUnavailableForLegacyRow(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	now := time.Now().UTC()
	legacy := &model.APIKey{
		KeyHash: crypto.HashToken("sk-yr-legacy-key"), KeyPrefix: "sk-yr-legacy000",
		Status: model.APIKeyStatusActive, CreatedAt: now, UpdatedAt: now,
		// EncryptedKey intentionally left empty — a pre-00021 row.
	}
	if err := db.Create(legacy).Error; err != nil {
		t.Fatalf("seed legacy key: %v", err)
	}
	_, err := svc.GetAPIKeyPlaintext(legacy.ID, nil)
	if !errors.Is(err, errcode.ErrAPIKeyPlaintextUnavailable) {
		t.Fatalf("expected ErrAPIKeyPlaintextUnavailable for a legacy row, got %v", err)
	}
}

// TestGetAPIKeyPlaintextNotFound mirrors GetAPIKey's not-found path so the
// reveal endpoint surfaces the same 11001 the rest of the resource does.
func TestGetAPIKeyPlaintextNotFound(t *testing.T) {
	svc, _ := newAPIKeyServiceForTest(t)
	_, err := svc.GetAPIKeyPlaintext(999999, nil)
	if !errors.Is(err, errcode.ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

// TestGetAPIKeyPlaintextFailsWithDifferentMasterKey builds a service whose
// secrets box holds a different master key than the one that encrypted the
// row, and asserts the
// reveal surfaces a decrypt error (not a silent wrong plaintext) — the AES-GCM
// auth tag guarantees a tampered/wrong-key ciphertext fails to open.
func TestGetAPIKeyPlaintextFailsWithDifferentMasterKey(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	encryptSvc := NewAPIKeyService(db, testutil.ProviderSecrets())
	result, err := encryptSvc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{mid}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	// A different 32-byte key (mirrors provider_service_test's wrong-key shape).
	otherKey := make([]byte, 32)
	for i := range otherKey {
		otherKey[i] = byte(i + 99)
	}
	decryptSvc := NewAPIKeyService(db, crypto.NewSecretBox(otherKey))
	if _, err := decryptSvc.GetAPIKeyPlaintext(result.APIKey.ID, nil); err == nil {
		t.Fatalf("expected a decrypt error when the masterKey differs, got nil")
	}
}

// A PATCH that touches only one field must leave the other limits intact —
// a full-overwrite PATCH would silently wipe every other limit on a
// one-field update.
func TestUpdateAPIKeySparsePatchLeavesOtherLimits(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	rpm, tpm := 100, 200
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db),
		ModelIDs: []uint{mid}, RPMLimit: &rpm, TPMLimit: &tpm,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	remark := "bob's key"
	view, err := svc.UpdateAPIKey(result.APIKey.ID, UpdateAPIKeyInput{Remark: &remark}, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}
	if view.Remark != "bob's key" {
		t.Fatalf("remark not updated: %q", view.Remark)
	}
	if view.RPMLimit == nil || *view.RPMLimit != 100 || view.TPMLimit == nil || *view.TPMLimit != 200 {
		t.Fatalf("sparse patch wiped other limits: rpm=%v tpm=%v", view.RPMLimit, view.TPMLimit)
	}
}

func TestUpdateAPIKeyClearsLimitWithZeroSentinel(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	rpm := 100
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{mid}, RPMLimit: &rpm}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	zero := 0
	view, err := svc.UpdateAPIKey(result.APIKey.ID, UpdateAPIKeyInput{RPMLimit: &zero}, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}
	if view.RPMLimit != nil {
		t.Fatalf("rpm_limit should be cleared by 0 sentinel, got %d", *view.RPMLimit)
	}
}

func TestUpdateAPIKeyReplacesWhitelist(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	m1 := seedModelForAPIKeyTest(t, db, "m1")
	m2 := seedModelForAPIKeyTest(t, db, "m2")
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{m1}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	view, err := svc.UpdateAPIKey(result.APIKey.ID, UpdateAPIKeyInput{ModelIDs: []uint{m2}}, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}
	if len(view.ModelIDs) != 1 || view.ModelIDs[0] != m2 {
		t.Fatalf("whitelist not replaced: %v", view.ModelIDs)
	}
}

func TestRevokeAPIKeyIdempotent(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{mid}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	now := time.Now().UTC()
	if err := svc.RevokeAPIKey(result.APIKey.ID, nil, now); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := svc.RevokeAPIKey(result.APIKey.ID, nil, now); err != nil {
		t.Fatalf("second revoke should be idempotent, got: %v", err)
	}
	view, err := svc.GetAPIKey(result.APIKey.ID, nil)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if view.DisplayStatus != APIKeyDisplayRevoked {
		t.Fatalf("expected revoked display status, got %q", view.DisplayStatus)
	}
}

func TestRevokeAPIKeyNotFound(t *testing.T) {
	svc, _ := newAPIKeyServiceForTest(t)
	err := svc.RevokeAPIKey(999999, nil, time.Now().UTC())
	if !errors.Is(err, errcode.ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

// CreateAPIKey doesn't reject a past expiry at the service layer (the handler
// does, via validateExpiryFuture); seed a row directly to exercise the runtime
// display-status computation for an expired-but-still-active key.
func TestDisplayStatusExpiredForPastExpiry(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	past := time.Now().UTC().Add(-time.Hour)
	now := time.Now().UTC()
	key := &model.APIKey{
		KeyHash:   crypto.HashToken("sk-yr-seed-value"),
		KeyPrefix: "sk-yr-seed000000",
		Status:    model.APIKeyStatusActive, ExpiresAt: &past,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(key).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}
	view, err := svc.GetAPIKey(key.ID, nil)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if view.DisplayStatus != APIKeyDisplayExpired {
		t.Fatalf("expected expired display status, got %q", view.DisplayStatus)
	}
}

// The status filter must match the runtime-computed display status, not the
// stored active/revoked column. Seed one key per display status and assert
// each status value returns exactly its partition — the SQL predicates in
// applyAPIKeyFilters and computeAPIKeyDisplayStatus must agree on the whole
// precedence (revoked > expired > budget-exhausted > active), not just the
// two simplest cases.
func TestListAPIKeysFiltersByDisplayStatus(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	limit := int64(1000)

	// Active (created via the service so it goes through the normal path).
	if _, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{mid}}, now); err != nil {
		t.Fatalf("CreateAPIKey live: %v", err)
	}
	// The other statuses seed rows directly (the service create path won't
	// produce an expired/revoked/over-budget key). The last seed is BOTH
	// expired and over-budget: precedence (expired > budget) means it must
	// render — and filter — as expired, not budget-exhausted. That's the only
	// case where the SQL guards and the Go if/else ordering could silently
	// disagree, so it's what makes this an equivalence test rather than four
	// isolated predicate checks.
	seeds := []*model.APIKey{
		{KeyHash: crypto.HashToken("sk-yr-expired"), KeyPrefix: "sk-yr-expired00", Status: model.APIKeyStatusActive, ExpiresAt: &past, CreatedAt: now, UpdatedAt: now},
		{KeyHash: crypto.HashToken("sk-yr-revoked"), KeyPrefix: "sk-yr-revoked00", Status: model.APIKeyStatusRevoked, CreatedAt: now, UpdatedAt: now},
		{KeyHash: crypto.HashToken("sk-yr-budget"), KeyPrefix: "sk-yr-budget000", Status: model.APIKeyStatusActive, ExpiresAt: &future, BudgetLimitMicros: &limit, BudgetSpentMicros: limit, CreatedAt: now, UpdatedAt: now},
		{KeyHash: crypto.HashToken("sk-yr-exp-budget"), KeyPrefix: "sk-yr-expbudget", Status: model.APIKeyStatusActive, ExpiresAt: &past, BudgetLimitMicros: &limit, BudgetSpentMicros: limit, CreatedAt: now, UpdatedAt: now},
	}
	for _, k := range seeds {
		if err := db.Create(k).Error; err != nil {
			t.Fatalf("seed key %s: %v", k.KeyPrefix, err)
		}
	}

	// Expected partition sizes: the expired+over-budget key lands in "expired"
	// (precedence), so expired has 2 and budget-exhausted has 1.
	wantCount := map[string]int{
		APIKeyDisplayActive:    1,
		APIKeyDisplayExpired:   2,
		APIKeyDisplayRevoked:   1,
		APIKeyDisplayBudgetHit: 1,
	}
	for status, want := range wantCount {
		list, total, err := svc.ListAPIKeys("", status, nil, 1, 20)
		if err != nil {
			t.Fatalf("ListAPIKeys %s: %v", status, err)
		}
		if int(total) != want || len(list) != want {
			t.Fatalf("status=%s: expected %d keys, got total=%d list=%v", status, want, total, list)
		}
		// Every returned row's computed display status must equal the filter —
		// the SQL partition and the Go computation must agree row-for-row.
		for _, v := range list {
			if v.DisplayStatus != status {
				t.Fatalf("status=%s: returned key %s whose display status is %q", status, v.KeyPrefix, v.DisplayStatus)
			}
		}
	}
}

// --- Input-compression per-key override tests --------------------------------

// TestCreateAPIKeyPersistsCompressFields verifies both compress columns are
// written at create time: a key created with override+enabled must carry those
// exact values in the stored row, and the returned view must surface them.
func TestCreateAPIKeyPersistsCompressFields(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")

	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db),
		ModelIDs:                []uint{mid},
		CompressEnabledOverride: true,
		CompressEnabled:         true,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if !result.APIKey.CompressEnabledOverride || !result.APIKey.CompressEnabled {
		t.Fatalf("view should surface compress fields: override=%v enabled=%v",
			result.APIKey.CompressEnabledOverride, result.APIKey.CompressEnabled)
	}

	var stored model.APIKey
	if err := db.First(&stored, result.APIKey.ID).Error; err != nil {
		t.Fatalf("load stored: %v", err)
	}
	if !stored.CompressEnabledOverride || !stored.CompressEnabled {
		t.Fatalf("stored row should have compress fields set: override=%v enabled=%v",
			stored.CompressEnabledOverride, stored.CompressEnabled)
	}
}

// TestCreateAPIKeyCompressDefaultsToFalse checks that a create call without
// compress fields defaults both to false (the "inherit global" state).
func TestCreateAPIKeyCompressDefaultsToFalse(t *testing.T) {
	svc, _ := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, svc.db, "m1")

	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, svc.db),
		ModelIDs: []uint{mid},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if result.APIKey.CompressEnabledOverride || result.APIKey.CompressEnabled {
		t.Fatalf("compress fields should default to false: override=%v enabled=%v",
			result.APIKey.CompressEnabledOverride, result.APIKey.CompressEnabled)
	}
}

// TestUpdateAPIKeyCompressOverrideFalseZeroesEnabled checks the override=false
// combination rule: a PATCH that sets override=false must store
// compress_enabled=false even when the caller also sends enabled=true. The
// override is off so the key inherits the global setting and the per-key
// enabled value is meaningless; zeroing it keeps the row clean.
func TestUpdateAPIKeyCompressOverrideFalseZeroesEnabled(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	created, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db),
		ModelIDs:                []uint{mid},
		CompressEnabledOverride: true,
		CompressEnabled:         true,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	overrideFalse := false
	enabledTrue := true
	view, err := svc.UpdateAPIKey(created.APIKey.ID, UpdateAPIKeyInput{
		CompressEnabledOverride: &overrideFalse,
		CompressEnabled:         &enabledTrue,
	}, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if view.CompressEnabledOverride {
		t.Fatalf("override should be false after PATCH, got %v", view.CompressEnabledOverride)
	}
	if view.CompressEnabled {
		t.Fatalf("enabled should be zeroed to false when override=false, got %v", view.CompressEnabled)
	}

	// Verify the columns landed in the DB, not just the view.
	var stored model.APIKey
	if err := db.First(&stored, created.APIKey.ID).Error; err != nil {
		t.Fatalf("load stored: %v", err)
	}
	if stored.CompressEnabled {
		t.Fatalf("DB compress_enabled should be false, got %v", stored.CompressEnabled)
	}
}

// TestUpdateAPIKeyCompressOverrideTrueRequiresEnabled checks that setting
// override=true without supplying enabled is rejected -- the override has no
// meaning without an explicit on/off value to override to.
func TestUpdateAPIKeyCompressOverrideTrueRequiresEnabled(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	created, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db),
		ModelIDs: []uint{mid},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	overrideTrue := true
	_, err = svc.UpdateAPIKey(created.APIKey.ID, UpdateAPIKeyInput{
		CompressEnabledOverride: &overrideTrue,
	}, nil, time.Now().UTC())
	if !errors.Is(err, errcode.ErrCompressEnabledRequired) {
		t.Fatalf("expected ErrCompressEnabledRequired, got %v", err)
	}
}

// TestUpdateAPIKeyCompressOverrideTrueWithEnabled checks the happy path:
// override=true + enabled=true stores both columns as-is.
func TestUpdateAPIKeyCompressOverrideTrueWithEnabled(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	created, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db),
		ModelIDs: []uint{mid},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	overrideTrue := true
	enabledTrue := true
	view, err := svc.UpdateAPIKey(created.APIKey.ID, UpdateAPIKeyInput{
		CompressEnabledOverride: &overrideTrue,
		CompressEnabled:         &enabledTrue,
	}, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !view.CompressEnabledOverride || !view.CompressEnabled {
		t.Fatalf("both compress fields should be true: override=%v enabled=%v",
			view.CompressEnabledOverride, view.CompressEnabled)
	}
}

// TestUpdateAPIKeyCompressSparsePatchLeavesOverride checks that a lone
// enabled patch (override pointer nil) writes only the enabled column and
// leaves the override column untouched -- the sparse-PATCH contract.
func TestUpdateAPIKeyCompressSparsePatchLeavesOverride(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	created, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db),
		ModelIDs:                []uint{mid},
		CompressEnabledOverride: true,
		CompressEnabled:         false,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Sparse PATCH: only enabled, override should stay true.
	enabledTrue := true
	view, err := svc.UpdateAPIKey(created.APIKey.ID, UpdateAPIKeyInput{
		CompressEnabled: &enabledTrue,
	}, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !view.CompressEnabledOverride {
		t.Fatalf("override should be untouched (true), got false")
	}
	if !view.CompressEnabled {
		t.Fatalf("enabled should be patched to true, got false")
	}
}

// TestListAndGetAPIKeySurfacesCompressFields checks that both the list and
// detail endpoints surface the compress fields in the view.
func TestListAndGetAPIKeySurfacesCompressFields(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db),
		ModelIDs:                []uint{mid},
		CompressEnabledOverride: true,
		CompressEnabled:         true,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Detail view.
	detail, err := svc.GetAPIKey(result.APIKey.ID, nil)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if !detail.CompressEnabledOverride || !detail.CompressEnabled {
		t.Fatalf("detail view should surface compress: override=%v enabled=%v",
			detail.CompressEnabledOverride, detail.CompressEnabled)
	}

	// List view.
	list, _, err := svc.ListAPIKeys("", "", nil, 1, 20)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	var found bool
	for _, v := range list {
		if v.ID == result.APIKey.ID {
			found = true
			if !v.CompressEnabledOverride || !v.CompressEnabled {
				t.Fatalf("list view should surface compress: override=%v enabled=%v",
					v.CompressEnabledOverride, v.CompressEnabled)
			}
		}
	}
	if !found {
		t.Fatalf("created key not found in list")
	}
}
