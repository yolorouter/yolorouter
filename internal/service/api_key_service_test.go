package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func newAPIKeyServiceForTest(t *testing.T) (*APIKeyService, *gorm.DB) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	return NewAPIKeyService(db), db
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

	result, err := svc.CreateAPIKey(CreateAPIKeyInput{
		OwnerLabel: "alice", Remark: "test key", ModelIDs: []uint{mid},
	}, time.Now().UTC())
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

	// The stored row keeps a hash, never the plaintext — the plaintext must
	// be unrecoverable from the database after create.
	var stored model.APIKey
	if err := db.First(&stored, result.APIKey.ID).Error; err != nil {
		t.Fatalf("load stored key: %v", err)
	}
	if stored.KeyHash == "" || stored.KeyHash == result.PlaintextKey {
		t.Fatalf("key_hash must be a hash, not the plaintext")
	}
}

func TestCreateAPIKeyTreatsZeroLimitAsUnlimited(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	zero := 0
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{
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
	svc, _ := newAPIKeyServiceForTest(t)
	_, err := svc.CreateAPIKey(CreateAPIKeyInput{
		ModelIDs: []uint{999999},
	}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
}

func TestGetAPIKeyReturnsWhitelist(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	m1 := seedModelForAPIKeyTest(t, db, "m1")
	m2 := seedModelForAPIKeyTest(t, db, "m2")
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{ModelIDs: []uint{m1, m2}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	detail, err := svc.GetAPIKey(result.APIKey.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if len(detail.ModelIDs) != 2 {
		t.Fatalf("expected 2 whitelisted models, got %v", detail.ModelIDs)
	}
}

func TestGetAPIKeyNotFound(t *testing.T) {
	svc, _ := newAPIKeyServiceForTest(t)
	_, err := svc.GetAPIKey(999999)
	if !errors.Is(err, errcode.ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

// A PATCH that touches only one field must leave the other limits intact —
// the reference project switched away from full-overwrite PATCH for exactly
// this reason (a one-field PATCH silently wiped the others).
func TestUpdateAPIKeySparsePatchLeavesOtherLimits(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	rpm, tpm := 100, 200
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{
		ModelIDs: []uint{mid}, RPMLimit: &rpm, TPMLimit: &tpm,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	owner := "bob"
	view, err := svc.UpdateAPIKey(result.APIKey.ID, UpdateAPIKeyInput{OwnerLabel: &owner}, time.Now().UTC())
	if err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}
	if view.OwnerLabel != "bob" {
		t.Fatalf("owner not updated: %q", view.OwnerLabel)
	}
	if view.RPMLimit == nil || *view.RPMLimit != 100 || view.TPMLimit == nil || *view.TPMLimit != 200 {
		t.Fatalf("sparse patch wiped other limits: rpm=%v tpm=%v", view.RPMLimit, view.TPMLimit)
	}
}

func TestUpdateAPIKeyClearsLimitWithZeroSentinel(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	rpm := 100
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{ModelIDs: []uint{mid}, RPMLimit: &rpm}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	zero := 0
	view, err := svc.UpdateAPIKey(result.APIKey.ID, UpdateAPIKeyInput{RPMLimit: &zero}, time.Now().UTC())
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
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{ModelIDs: []uint{m1}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	view, err := svc.UpdateAPIKey(result.APIKey.ID, UpdateAPIKeyInput{ModelIDs: []uint{m2}}, time.Now().UTC())
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
	result, err := svc.CreateAPIKey(CreateAPIKeyInput{ModelIDs: []uint{mid}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	now := time.Now().UTC()
	if err := svc.RevokeAPIKey(result.APIKey.ID, now); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := svc.RevokeAPIKey(result.APIKey.ID, now); err != nil {
		t.Fatalf("second revoke should be idempotent, got: %v", err)
	}
	view, err := svc.GetAPIKey(result.APIKey.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if view.DisplayStatus != APIKeyDisplayRevoked {
		t.Fatalf("expected revoked display status, got %q", view.DisplayStatus)
	}
}

func TestRevokeAPIKeyNotFound(t *testing.T) {
	svc, _ := newAPIKeyServiceForTest(t)
	err := svc.RevokeAPIKey(999999, time.Now().UTC())
	if !errors.Is(err, errcode.ErrAPIKeyNotFound) {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

func TestListAPIKeysSearchesByOwnerLabel(t *testing.T) {
	svc, db := newAPIKeyServiceForTest(t)
	mid := seedModelForAPIKeyTest(t, db, "m1")
	if _, err := svc.CreateAPIKey(CreateAPIKeyInput{OwnerLabel: "alice", ModelIDs: []uint{mid}}, time.Now().UTC()); err != nil {
		t.Fatalf("CreateAPIKey alice: %v", err)
	}
	if _, err := svc.CreateAPIKey(CreateAPIKeyInput{OwnerLabel: "bob", ModelIDs: []uint{mid}}, time.Now().UTC()); err != nil {
		t.Fatalf("CreateAPIKey bob: %v", err)
	}
	list, total, err := svc.ListAPIKeys("alice", 1, 20)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].OwnerLabel != "alice" {
		t.Fatalf("expected 1 alice key, got total=%d list=%v", total, list)
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
		KeyHash:   hashToken("sk-yr-seed-value"),
		KeyPrefix: "sk-yr-seed000000",
		Status:    model.APIKeyStatusActive, ExpiresAt: &past,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(key).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}
	view, err := svc.GetAPIKey(key.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if view.DisplayStatus != APIKeyDisplayExpired {
		t.Fatalf("expected expired display status, got %q", view.DisplayStatus)
	}
}
