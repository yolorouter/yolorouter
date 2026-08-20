package auth

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func TestCheckStateReflectsLocalUserPresence(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	initialized, err := CheckState(db)
	if err != nil {
		t.Fatalf("CheckState failed: %v", err)
	}
	if initialized {
		t.Fatalf("expected initialized=false on fresh db")
	}

	if _, _, err := Setup(db, "admin", "password123", now); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	initialized, err = CheckState(db)
	if err != nil {
		t.Fatalf("CheckState failed: %v", err)
	}
	if !initialized {
		t.Fatalf("expected initialized=true after Setup")
	}
}

func TestSetupRejectsSecondCall(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	if _, _, err := Setup(db, "admin", "password123", now); err != nil {
		t.Fatalf("first Setup failed: %v", err)
	}

	_, _, err := Setup(db, "someone-else", "password456", now)
	if !errors.Is(err, errcode.ErrAccountSetupAlreadyDone) {
		t.Fatalf("expected ErrAccountSetupAlreadyDone, got: %v", err)
	}
}

// TestConcurrentSetupOnlyCreatesOneLocalUser exercises the actual race
// the sequential TestSetupRejectsSecondCall can't: many goroutines
// calling Setup with DIFFERENT usernames at the same time, before any of
// them has observed the others' CountLocalUsers result. Without the
// partial unique index on users.is_local (migration
// 00023_users_multi_account.sql) and Setup's re-check-after-failure
// logic, this could create more than one local password account — a
// direct violation of the single-local-account invariant.
func TestConcurrentSetupOnlyCreatesOneLocalUser(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	const attempts = 8
	results := make(chan error, attempts)
	for i := range attempts {
		username := fmt.Sprintf("admin-%d", i)
		go func() {
			_, _, err := Setup(db, username, "password123", now)
			results <- err
		}()
	}

	succeeded, alreadyDone := 0, 0
	for range attempts {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, errcode.ErrAccountSetupAlreadyDone):
			alreadyDone++
		default:
			t.Fatalf("unexpected error from concurrent Setup: %v", err)
		}
	}

	if succeeded != 1 {
		t.Fatalf("expected exactly 1 successful Setup out of %d concurrent attempts, got %d", attempts, succeeded)
	}
	if alreadyDone != attempts-1 {
		t.Fatalf("expected the other %d attempts to see ErrAccountSetupAlreadyDone, got %d", attempts-1, alreadyDone)
	}

	count, err := repository.CountLocalUsers(db)
	if err != nil {
		t.Fatalf("CountLocalUsers failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 local user row to exist after the race, got %d", count)
	}
}

func TestSetupIssuesAWorkingSession(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	admin, sessionID, err := Setup(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	if admin.Username != "admin" {
		t.Fatalf("expected username=admin, got %q", admin.Username)
	}
	// Setup must create the account as the local admin, or the whole
	// role/escape-hatch model collapses on first run.
	if admin.Role != model.RoleAdmin || !admin.IsLocal || admin.Status != model.UserStatusEnabled {
		t.Fatalf("expected an enabled local admin, got role=%q is_local=%v status=%d", admin.Role, admin.IsLocal, admin.Status)
	}
	if sessionID == "" {
		t.Fatalf("expected a non-empty session id")
	}

	got, err := Me(db, admin.ID)
	if err != nil {
		t.Fatalf("Me failed: %v", err)
	}
	if got.Username != "admin" {
		t.Fatalf("expected Me to return the newly created admin, got %+v", got)
	}
}

func TestLoginSucceedsWithCorrectPassword(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	if _, _, err := Setup(db, "admin", "password123", now); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	admin, sessionID, err := Login(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if admin.Username != "admin" || sessionID == "" {
		t.Fatalf("unexpected login result: admin=%+v sessionID=%q", admin, sessionID)
	}
}

func TestLoginFailsWithWrongPasswordWithoutRevealingAccountExistence(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	if _, _, err := Setup(db, "admin", "password123", now); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	_, _, err := Login(db, "admin", "wrong-password", now)
	if !errors.Is(err, errcode.ErrAccountInvalidCredentials) {
		t.Fatalf("expected ErrAccountInvalidCredentials for wrong password, got: %v", err)
	}

	_, _, err = Login(db, "no-such-user", "whatever123", now)
	if !errors.Is(err, errcode.ErrAccountInvalidCredentials) {
		t.Fatalf("expected the SAME ErrAccountInvalidCredentials for unknown username, got: %v", err)
	}
}

func TestLoginLocksAfterFiveConsecutiveFailures(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	if _, _, err := Setup(db, "admin", "password123", now); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	for i := 1; i <= 4; i++ {
		_, _, err := Login(db, "admin", "wrong", now)
		if !errors.Is(err, errcode.ErrAccountInvalidCredentials) {
			t.Fatalf("attempt #%d: expected ErrAccountInvalidCredentials, got: %v", i, err)
		}
	}

	_, _, err := Login(db, "admin", "wrong", now)
	var lockedErr *LockedError
	if !errors.As(err, &lockedErr) {
		t.Fatalf("expected *LockedError on the 5th failure, got: %v", err)
	}
	wantUnlock := now.Add(LoginLockDuration)
	if lockedErr.LockedUntil.Sub(wantUnlock).Abs() > time.Second {
		t.Fatalf("expected LockedUntil ~= %v, got %v", wantUnlock, lockedErr.LockedUntil)
	}

	// Even the correct password must be rejected while locked.
	_, _, err = Login(db, "admin", "password123", now.Add(time.Minute))
	if !errors.As(err, &lockedErr) {
		t.Fatalf("expected still-locked error with correct password mid-lockout, got: %v", err)
	}

	// After the lock window passes, the correct password succeeds again.
	_, _, err = Login(db, "admin", "password123", now.Add(16*time.Minute))
	if err != nil {
		t.Fatalf("expected login to succeed after lock expiry, got: %v", err)
	}
}

func TestChangePasswordInvalidatesExistingSessions(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin, sessionID, err := Setup(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	if err := ChangePassword(db, admin.ID, "password123", "newpassword456", now); err != nil {
		t.Fatalf("ChangePassword failed: %v", err)
	}

	// The old session must no longer resolve via a fresh Login+session
	// lookup path — Logout on the now-deleted session id must not error
	// (idempotent), proving it's gone rather than merely untouched.
	if err := Logout(db, sessionID); err != nil {
		t.Fatalf("Logout on already-invalidated session should be a no-op, got: %v", err)
	}

	// Old password no longer works.
	_, _, err = Login(db, "admin", "password123", now)
	if !errors.Is(err, errcode.ErrAccountInvalidCredentials) {
		t.Fatalf("expected old password to be rejected, got: %v", err)
	}
	// New password works.
	if _, _, err := Login(db, "admin", "newpassword456", now); err != nil {
		t.Fatalf("expected new password to work, got: %v", err)
	}
}

func TestChangePasswordRejectsWrongCurrentPassword(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin, _, err := Setup(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	err = ChangePassword(db, admin.ID, "wrong-current", "newpassword456", now)
	if !errors.Is(err, errcode.ErrAccountInvalidCredentials) {
		t.Fatalf("expected ErrAccountInvalidCredentials, got: %v", err)
	}
}

func TestLockedErrorMessageIsAccountLoginLocked(t *testing.T) {
	err := &LockedError{LockedUntil: time.Now()}
	if err.Error() != errcode.ErrorMessages[errcode.AccountLoginLocked] {
		t.Fatalf("expected the AccountLoginLocked message, got %q", err.Error())
	}
}

func TestCheckStateErrorsWhenUsersTableMissing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.DropTable(t, db, "users")

	if _, err := CheckState(db); err == nil {
		t.Fatalf("expected an error when the users table is missing")
	}
}

func TestSetupErrorsWhenPasswordTooLongToHash(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()

	_, _, err := Setup(db, "admin", strings.Repeat("a", 73), now)
	if err == nil {
		t.Fatalf("expected an error for a password bcrypt refuses to hash")
	}
	if errors.Is(err, errcode.ErrAccountSetupAlreadyDone) {
		t.Fatalf("expected a hashing error, not ErrAccountSetupAlreadyDone")
	}
}

// TestSetupRollsBackAndReturnsRawErrorWhenSessionCreationFails exercises
// Setup's txErr path where the transaction genuinely fails (as opposed to
// losing the single-local-user race) — the user insert must be rolled
// back (leaving CountLocalUsers at 0) so the raw error is returned
// instead of ErrAccountSetupAlreadyDone.
func TestSetupRollsBackAndReturnsRawErrorWhenSessionCreationFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	testutil.BlockTableWrites(t, db, "user_sessions", "INSERT")

	_, _, err := Setup(db, "admin", "password123", now)
	if err == nil {
		t.Fatalf("expected an error when session creation fails")
	}
	if errors.Is(err, errcode.ErrAccountSetupAlreadyDone) {
		t.Fatalf("expected the raw transaction error, not ErrAccountSetupAlreadyDone, got %v", err)
	}

	count, countErr := repository.CountLocalUsers(db)
	if countErr != nil {
		t.Fatalf("CountLocalUsers failed: %v", countErr)
	}
	if count != 0 {
		t.Fatalf("expected the failed transaction to roll back the user insert, found %d local users", count)
	}
}

func TestLoginErrorsWhenUsersTableMissing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.DropTable(t, db, "users")

	if _, _, err := Login(db, "admin", "password123", time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the users table is missing")
	}
}

func TestLoginErrorsWhenRecordLoginFailureFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	if _, _, err := Setup(db, "admin", "password123", now); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "users", "UPDATE")

	_, _, err := Login(db, "admin", "wrong-password", now)
	if err == nil {
		t.Fatalf("expected an error when RecordLoginFailure's UPDATE fails")
	}
	var lockedErr *LockedError
	if errors.As(err, &lockedErr) {
		t.Fatalf("expected a raw DB error, not a LockedError, got %v", err)
	}
}

func TestLoginErrorsWhenDeleteExpiredSessionsFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin, _, err := Setup(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	// A BEFORE DELETE trigger only fires for rows that actually match the
	// DELETE's WHERE clause — DeleteExpiredSessions' error branch can only
	// be exercised if there's at least one already-expired row for it to
	// attempt to delete.
	if err := repository.CreateSession(db, "already-expired", admin.ID, now.Add(-time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed expired session failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "user_sessions", "DELETE")

	if _, _, err := Login(db, "admin", "password123", now); err == nil {
		t.Fatalf("expected an error when DeleteExpiredSessions fails inside Login's transaction")
	}
}

func TestLoginErrorsWhenRecordLoginSuccessFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	if _, _, err := Setup(db, "admin", "password123", now); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "users", "UPDATE")

	if _, _, err := Login(db, "admin", "password123", now); err == nil {
		t.Fatalf("expected an error when RecordLoginSuccess's UPDATE fails inside Login's transaction")
	}
}

func TestChangePasswordErrorsWhenUserNotFound(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	if err := ChangePassword(db, 9999, "whatever", "newpassword456", time.Now().UTC()); err == nil {
		t.Fatalf("expected an error for a non-existent user id")
	}
}

func TestChangePasswordErrorsWhenNewPasswordTooLongToHash(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin, _, err := Setup(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	err = ChangePassword(db, admin.ID, "password123", strings.Repeat("b", 73), now)
	if err == nil {
		t.Fatalf("expected an error for a new password bcrypt refuses to hash")
	}
}

func TestChangePasswordErrorsWhenPasswordHashUpdateFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin, _, err := Setup(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "users", "UPDATE")

	if err := ChangePassword(db, admin.ID, "password123", "newpassword456", now); err == nil {
		t.Fatalf("expected an error when the password_hash UPDATE fails")
	}
}

// CreateSession's crypto.GenerateRandomToken error branch is not exercised
// here — see pkg/crypto's own tests: since Go 1.24, crypto/rand.Read cannot
// be made to return an error from a test (it crashes the program instead),
// so that branch is unreachable dead code under this project's Go version.

func TestCreateSessionErrorsWhenInsertFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin, _, err := Setup(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "user_sessions", "INSERT")

	if _, err := CreateSession(db, admin.ID, now); err == nil {
		t.Fatalf("expected an error when the user_sessions INSERT fails")
	}
}

// TestLoginRejectsDisabledLocalAccount pins Login's status gate — and its
// ordering: the disabled answer only comes after the password check
// passes, so someone without the password still sees the generic
// invalid-credentials error and learns nothing about the account.
func TestLoginRejectsDisabledLocalAccount(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin, _, err := Setup(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", admin.ID).
		Update("status", model.UserStatusDisabled).Error; err != nil {
		t.Fatalf("disable user failed: %v", err)
	}

	_, _, err = Login(db, "admin", "password123", now)
	if !errors.Is(err, errcode.ErrAccountDisabled) {
		t.Fatalf("expected ErrAccountDisabled with the correct password, got: %v", err)
	}

	_, _, err = Login(db, "admin", "wrong-password", now)
	if errors.Is(err, errcode.ErrAccountDisabled) {
		t.Fatalf("wrong password must not reveal the disabled state, got ErrAccountDisabled")
	}
}

// TestLoginNeverMatchesNonLocalAccounts pins the password form's scope:
// an externally-provisioned account (no password, not local) must be
// indistinguishable from a nonexistent account through this path.
func TestLoginNeverMatchesNonLocalAccounts(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	member := &model.User{Username: "ext-user", Role: model.RoleMember, Status: model.UserStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(db, member); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	_, _, err := Login(db, "ext-user", "", now)
	if !errors.Is(err, errcode.ErrAccountInvalidCredentials) {
		t.Fatalf("expected ErrAccountInvalidCredentials for a non-local account, got: %v", err)
	}
}
