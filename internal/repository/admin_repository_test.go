package repository

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
)

func TestCountAdminsIsZeroOnFreshDB(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	count, err := CountAdmins(db)
	if err != nil {
		t.Fatalf("CountAdmins failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 admins on fresh db, got %d", count)
	}
}

func TestCreateAdminAndFindByUsername(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	admin := &model.Admin{Username: "alice", PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := CreateAdmin(db, admin); err != nil {
		t.Fatalf("CreateAdmin failed: %v", err)
	}
	if admin.ID == 0 {
		t.Fatalf("expected CreateAdmin to populate ID")
	}

	found, err := FindAdminByUsername(db, "alice")
	if err != nil {
		t.Fatalf("FindAdminByUsername failed: %v", err)
	}
	if found.Username != "alice" || found.PasswordHash != "hash" {
		t.Fatalf("unexpected admin: %+v", found)
	}

	count, err := CountAdmins(db)
	if err != nil {
		t.Fatalf("CountAdmins failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 admin, got %d", count)
	}
}

func TestFindAdminByUsernameReturnsNotFoundForMissingUsername(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	_, err := FindAdminByUsername(db, "nobody")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound, got: %v", err)
	}
}

func TestRecordLoginFailureIncrementsCountAndLocksAtThreshold(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	admin := &model.Admin{Username: "bob", PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := CreateAdmin(db, admin); err != nil {
		t.Fatalf("CreateAdmin failed: %v", err)
	}

	for i := 1; i <= 4; i++ {
		lockedUntil, err := RecordLoginFailure(db, admin.ID, now, 5, 15*time.Minute)
		if err != nil {
			t.Fatalf("RecordLoginFailure #%d failed: %v", i, err)
		}
		if lockedUntil != nil {
			t.Fatalf("RecordLoginFailure #%d: expected no lock before reaching threshold, got locked_until=%v", i, lockedUntil)
		}
	}
	got, err := FindAdminByID(db, admin.ID)
	if err != nil {
		t.Fatalf("FindAdminByID failed: %v", err)
	}
	if got.FailedLoginCount != 4 {
		t.Fatalf("expected failed_login_count=4 after 4 failures, got %d", got.FailedLoginCount)
	}
	if got.LockedUntil != nil {
		t.Fatalf("expected no lock before reaching threshold, got locked_until=%v", got.LockedUntil)
	}

	// 5th failure crosses the threshold.
	lockedUntil, err := RecordLoginFailure(db, admin.ID, now, 5, 15*time.Minute)
	if err != nil {
		t.Fatalf("RecordLoginFailure #5 failed: %v", err)
	}
	if lockedUntil == nil {
		t.Fatalf("expected RecordLoginFailure to return a non-nil locked_until after the 5th failure")
	}
	got, err = FindAdminByID(db, admin.ID)
	if err != nil {
		t.Fatalf("FindAdminByID failed: %v", err)
	}
	if got.FailedLoginCount != 5 {
		t.Fatalf("expected failed_login_count=5, got %d", got.FailedLoginCount)
	}
	if got.LockedUntil == nil {
		t.Fatalf("expected locked_until to be set after 5th failure")
	}
	wantUnlock := now.Add(15 * time.Minute)
	if got.LockedUntil.Sub(wantUnlock).Abs() > time.Second {
		t.Fatalf("expected locked_until ~= %v, got %v", wantUnlock, *got.LockedUntil)
	}
	if lockedUntil.Sub(*got.LockedUntil).Abs() > time.Second {
		t.Fatalf("expected RETURNING locked_until to match the stored value, got %v vs %v", lockedUntil, got.LockedUntil)
	}
}

func TestRecordLoginFailureAfterExpiredLockStartsFreshCount(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	past := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	admin := &model.Admin{Username: "carol", PasswordHash: "hash", CreatedAt: past, UpdatedAt: past}
	if err := CreateAdmin(db, admin); err != nil {
		t.Fatalf("CreateAdmin failed: %v", err)
	}
	// Simulate an already-expired lock from a previous round.
	expiredLock := past.Add(15 * time.Minute) // still in the past relative to "now" below
	if err := db.Model(&model.Admin{}).Where("id = ?", admin.ID).
		Updates(map[string]interface{}{"failed_login_count": 5, "locked_until": expiredLock}).Error; err != nil {
		t.Fatalf("seed expired lock failed: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	lockedUntil, err := RecordLoginFailure(db, admin.ID, now, 5, 15*time.Minute)
	if err != nil {
		t.Fatalf("RecordLoginFailure failed: %v", err)
	}
	if lockedUntil != nil {
		t.Fatalf("expected no lock immediately after an expired lock's first retry, got %v", lockedUntil)
	}

	got, err := FindAdminByID(db, admin.ID)
	if err != nil {
		t.Fatalf("FindAdminByID failed: %v", err)
	}
	// A single failure right after the old lock expired must NOT
	// immediately re-lock the account — it starts a fresh count of 1.
	if got.FailedLoginCount != 1 {
		t.Fatalf("expected fresh count of 1 after expired lock, got %d", got.FailedLoginCount)
	}
	if got.LockedUntil != nil {
		t.Fatalf("expected no lock immediately after an expired lock's first retry, got %v", *got.LockedUntil)
	}
}

func TestRecordLoginSuccessResetsCountAndLock(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	admin := &model.Admin{Username: "dave", PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := CreateAdmin(db, admin); err != nil {
		t.Fatalf("CreateAdmin failed: %v", err)
	}
	locked := now.Add(15 * time.Minute)
	if err := db.Model(&model.Admin{}).Where("id = ?", admin.ID).
		Updates(map[string]interface{}{"failed_login_count": 5, "locked_until": locked}).Error; err != nil {
		t.Fatalf("seed lock failed: %v", err)
	}

	if err := RecordLoginSuccess(db, admin.ID, now); err != nil {
		t.Fatalf("RecordLoginSuccess failed: %v", err)
	}

	got, err := FindAdminByID(db, admin.ID)
	if err != nil {
		t.Fatalf("FindAdminByID failed: %v", err)
	}
	if got.FailedLoginCount != 0 || got.LockedUntil != nil {
		t.Fatalf("expected reset state, got count=%d locked_until=%v", got.FailedLoginCount, got.LockedUntil)
	}
}

func TestFindAdminByIDReturnsNotFoundForMissingID(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	_, err := FindAdminByID(db, 9999)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound, got %v", err)
	}
}

// TestRecordLoginFailureReturnsErrorWhenDBUnavailable covers the genuine
// (non-not-found) DB error branch: the RETURNING-based raw UPDATE reports
// whatever the driver returns when the connection itself is gone, which
// Scan surfaces as a plain error rather than gorm.ErrRecordNotFound (a raw
// Scan on zero affected rows is not itself an error — RecordLoginFailure
// has no such "not found" case at all, only "the query itself failed").
func TestRecordLoginFailureReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.CloseDB(t, db)

	_, err := RecordLoginFailure(db, 1, time.Now().UTC(), 5, 15*time.Minute)
	if err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestUpdateAdminPasswordHash(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	admin := &model.Admin{Username: "erin", PasswordHash: "old", CreatedAt: now, UpdatedAt: now}
	if err := CreateAdmin(db, admin); err != nil {
		t.Fatalf("CreateAdmin failed: %v", err)
	}

	if err := UpdateAdminPasswordHash(db, admin.ID, "new-hash", now.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateAdminPasswordHash failed: %v", err)
	}

	got, err := FindAdminByID(db, admin.ID)
	if err != nil {
		t.Fatalf("FindAdminByID failed: %v", err)
	}
	if got.PasswordHash != "new-hash" {
		t.Fatalf("expected updated password hash, got %q", got.PasswordHash)
	}
}
