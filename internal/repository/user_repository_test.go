package repository

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// newBootstrapAdmin builds the shape first-run setup creates: the one
// password-login bootstrap account, admin role, enabled.
func newBootstrapAdmin(username string, now time.Time) *model.User {
	return &model.User{
		Username:     username,
		PasswordHash: "hash",
		Role:         model.RoleAdmin,
		Status:       model.UserStatusEnabled,
		IsLocal:      true,
		IsBootstrap:  true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// newLocalMember builds the shape console provisioning creates: a
// password-login member without the bootstrap flag.
func newLocalMember(username string, now time.Time) *model.User {
	return &model.User{
		Username:     username,
		PasswordHash: "hash",
		Role:         model.RoleMember,
		Status:       model.UserStatusEnabled,
		IsLocal:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// newMemberUser builds the shape external-identity provisioning creates:
// no password, member role, enabled, not local.
func newMemberUser(username string, now time.Time) *model.User {
	return &model.User{
		Username:  username,
		Role:      model.RoleMember,
		Status:    model.UserStatusEnabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestCountBootstrapUsersIsZeroOnFreshDB(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	count, err := CountBootstrapUsers(db)
	if err != nil {
		t.Fatalf("CountBootstrapUsers failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 bootstrap users on fresh db, got %d", count)
	}
}

func TestCreateUserAndFindLocalByUsername(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	user := newBootstrapAdmin("alice", now)
	if err := CreateUser(db, user); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	if user.ID == 0 {
		t.Fatalf("expected CreateUser to populate ID")
	}

	found, err := FindLocalUserByUsername(db, "alice")
	if err != nil {
		t.Fatalf("FindLocalUserByUsername failed: %v", err)
	}
	if found.Username != "alice" || found.PasswordHash != "hash" || found.Role != model.RoleAdmin || !found.IsLocal {
		t.Fatalf("unexpected user: %+v", found)
	}

	count, err := CountBootstrapUsers(db)
	if err != nil {
		t.Fatalf("CountBootstrapUsers failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 bootstrap user, got %d", count)
	}
}

// TestCountBootstrapUsersIgnoresNonBootstrapAccounts is what makes
// CountBootstrapUsers a correct "is setup done" signal: neither
// externally-provisioned accounts nor console-created local members carry
// the flag, so neither may make the setup page disappear on an instance
// whose bootstrap admin was never created. Remove the is_bootstrap filter
// and this goes red.
func TestCountBootstrapUsersIgnoresNonBootstrapAccounts(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateUser(db, newMemberUser("ext-user", now)); err != nil {
		t.Fatalf("CreateUser(member) failed: %v", err)
	}
	if err := CreateUser(db, newLocalMember("console-member", now)); err != nil {
		t.Fatalf("CreateUser(local member) failed: %v", err)
	}

	count, err := CountBootstrapUsers(db)
	if err != nil {
		t.Fatalf("CountBootstrapUsers failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 bootstrap users when no bootstrap account exists, got %d", count)
	}
}

// TestFindLocalUserByUsernameIgnoresNonLocalUsers proves password login
// can never resolve an externally-provisioned account: without the
// is_local filter, a member row would be reachable through the password
// form (and its empty hash relied on as the only line of defense).
func TestFindLocalUserByUsernameIgnoresNonLocalUsers(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateUser(db, newMemberUser("ext-bob", now)); err != nil {
		t.Fatalf("CreateUser(member) failed: %v", err)
	}

	_, err := FindLocalUserByUsername(db, "ext-bob")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound for a non-local username, got: %v", err)
	}
}

func TestFindLocalUserByUsernameReturnsNotFoundForMissingUsername(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	_, err := FindLocalUserByUsername(db, "nobody")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound, got: %v", err)
	}
}

// TestSecondBootstrapUserIsRejectedByDatabase exercises the partial unique
// index on users.is_bootstrap — the database-level replacement for the old
// single-admin singleton column. App-level checks alone are a
// check-then-act race under concurrent first-run setup requests, so the
// constraint itself must hold. Local (password-login) rows without the
// flag are the console-provisioned kind and must coexist freely.
func TestSecondBootstrapUserIsRejectedByDatabase(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateUser(db, newBootstrapAdmin("first", now)); err != nil {
		t.Fatalf("CreateUser(bootstrap) failed: %v", err)
	}
	second := newBootstrapAdmin("second", now)
	if err := CreateUser(db, second); err == nil {
		t.Fatalf("expected the database to reject a second bootstrap user")
	}
	if err := CreateUser(db, newLocalMember("plain-local", now)); err != nil {
		t.Fatalf("expected a local member to coexist with the bootstrap account, got: %v", err)
	}
}

// TestMultipleAdminsAreAllowed pins the multi-account model's key
// difference from the old schema: any number of admin-role users may
// exist, as long as at most one is the bootstrap account.
func TestMultipleAdminsAreAllowed(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateUser(db, newBootstrapAdmin("root", now)); err != nil {
		t.Fatalf("CreateUser(bootstrap admin) failed: %v", err)
	}
	second := newMemberUser("promoted", now)
	second.Role = model.RoleAdmin
	if err := CreateUser(db, second); err != nil {
		t.Fatalf("expected a second (non-local) admin to be allowed, got: %v", err)
	}
}

func TestRecordLoginFailureIncrementsCountAndLocksAtThreshold(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	user := newBootstrapAdmin("bob", now)
	if err := CreateUser(db, user); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	for i := 1; i <= 4; i++ {
		lockedUntil, err := RecordLoginFailure(db, user.ID, now, 5, 15*time.Minute)
		if err != nil {
			t.Fatalf("RecordLoginFailure #%d failed: %v", i, err)
		}
		if lockedUntil != nil {
			t.Fatalf("RecordLoginFailure #%d: expected no lock before reaching threshold, got locked_until=%v", i, lockedUntil)
		}
	}
	got, err := FindUserByID(db, user.ID)
	if err != nil {
		t.Fatalf("FindUserByID failed: %v", err)
	}
	if got.FailedLoginCount != 4 {
		t.Fatalf("expected failed_login_count=4 after 4 failures, got %d", got.FailedLoginCount)
	}
	if got.LockedUntil != nil {
		t.Fatalf("expected no lock before reaching threshold, got locked_until=%v", got.LockedUntil)
	}

	// 5th failure crosses the threshold.
	lockedUntil, err := RecordLoginFailure(db, user.ID, now, 5, 15*time.Minute)
	if err != nil {
		t.Fatalf("RecordLoginFailure #5 failed: %v", err)
	}
	if lockedUntil == nil {
		t.Fatalf("expected RecordLoginFailure to return a non-nil locked_until after the 5th failure")
	}
	got, err = FindUserByID(db, user.ID)
	if err != nil {
		t.Fatalf("FindUserByID failed: %v", err)
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
	user := newBootstrapAdmin("carol", past)
	if err := CreateUser(db, user); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	// Simulate an already-expired lock from a previous round.
	expiredLock := past.Add(15 * time.Minute) // still in the past relative to "now" below
	if err := db.Model(&model.User{}).Where("id = ?", user.ID).
		Updates(map[string]interface{}{"failed_login_count": 5, "locked_until": expiredLock}).Error; err != nil {
		t.Fatalf("seed expired lock failed: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	lockedUntil, err := RecordLoginFailure(db, user.ID, now, 5, 15*time.Minute)
	if err != nil {
		t.Fatalf("RecordLoginFailure failed: %v", err)
	}
	if lockedUntil != nil {
		t.Fatalf("expected no lock immediately after an expired lock's first retry, got %v", lockedUntil)
	}

	got, err := FindUserByID(db, user.ID)
	if err != nil {
		t.Fatalf("FindUserByID failed: %v", err)
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

func TestRecordLoginSuccessResetsCountLockAndStampsLastLogin(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	user := newBootstrapAdmin("dave", now)
	if err := CreateUser(db, user); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	locked := now.Add(15 * time.Minute)
	if err := db.Model(&model.User{}).Where("id = ?", user.ID).
		Updates(map[string]interface{}{"failed_login_count": 5, "locked_until": locked}).Error; err != nil {
		t.Fatalf("seed lock failed: %v", err)
	}

	if err := RecordLoginSuccess(db, user.ID, now); err != nil {
		t.Fatalf("RecordLoginSuccess failed: %v", err)
	}

	got, err := FindUserByID(db, user.ID)
	if err != nil {
		t.Fatalf("FindUserByID failed: %v", err)
	}
	if got.FailedLoginCount != 0 || got.LockedUntil != nil {
		t.Fatalf("expected reset state, got count=%d locked_until=%v", got.FailedLoginCount, got.LockedUntil)
	}
	if got.LastLoginAt == nil || got.LastLoginAt.Sub(now).Abs() > time.Second {
		t.Fatalf("expected last_login_at ~= %v, got %v", now, got.LastLoginAt)
	}
}

func TestFindUserByIDReturnsNotFoundForMissingID(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	_, err := FindUserByID(db, 9999)
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

func TestUpdateUserPasswordHash(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	user := newBootstrapAdmin("erin", now)
	user.PasswordHash = "old"
	if err := CreateUser(db, user); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	if err := UpdateUserPasswordHash(db, user.ID, "new-hash", now.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateUserPasswordHash failed: %v", err)
	}

	got, err := FindUserByID(db, user.ID)
	if err != nil {
		t.Fatalf("FindUserByID failed: %v", err)
	}
	if got.PasswordHash != "new-hash" {
		t.Fatalf("expected updated password hash, got %q", got.PasswordHash)
	}
}
