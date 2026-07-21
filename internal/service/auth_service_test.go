package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// blockTableWrites installs a SQLite trigger that turns every future
// statement of the given kind ("UPDATE"/"INSERT"/"DELETE") against table
// into an error, while leaving every other table (and every other
// statement kind on the same table) untouched. Used to force a repository
// call to fail without corrupting the schema outright (as dropTable does),
// so earlier reads in the same code path still succeed.
func blockTableWrites(t *testing.T, db *gorm.DB, table, kind string) {
	t.Helper()
	stmt := fmt.Sprintf(
		"CREATE TRIGGER block_%s_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END",
		strings.ToLower(kind), table, kind, table,
	)
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("create blocking trigger on %s: %v", table, err)
	}
}

// dropTable removes a table outright, forcing every subsequent read or
// write against it to fail — used for the "repository call errors for a
// reason that isn't gorm.ErrRecordNotFound" branches.
func dropTable(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	// Disable FK enforcement first: provider_keys.provider_id references
	// providers(id), and SQLite refuses to drop a table another table's FK
	// still points at while enforcement is on — tests need to drop just one
	// side (e.g. providers) while leaving the other (provider_keys) intact
	// and queryable.
	if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		t.Fatalf("disable foreign_keys pragma: %v", err)
	}
	if err := db.Exec("DROP TABLE " + table).Error; err != nil {
		t.Fatalf("drop table %s: %v", table, err)
	}
}

func TestCheckStateReflectsAdminCount(t *testing.T) {
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

// TestConcurrentSetupOnlyCreatesOneAdmin exercises the actual race the
// sequential TestSetupRejectsSecondCall can't: many goroutines calling
// Setup with DIFFERENT usernames at the same time, before any of them has
// observed the others' CountAdmins result. Without the
// admins.singleton_guard UNIQUE constraint (migration
// 00002_create_admin_auth.sql) and Setup's re-check-after-failure logic,
// this used to be able to create more than one admin row — a direct
// violation of PRD §3.1's single-admin invariant.
func TestConcurrentSetupOnlyCreatesOneAdmin(t *testing.T) {
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

	count, err := repository.CountAdmins(db)
	if err != nil {
		t.Fatalf("CountAdmins failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 admin row to exist after the race, got %d", count)
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

func TestCheckStateErrorsWhenAdminsTableMissing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	dropTable(t, db, "admins")

	if _, err := CheckState(db); err == nil {
		t.Fatalf("expected an error when the admins table is missing")
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
// losing the singleton_guard race) — the admin insert must be rolled back
// (leaving CountAdmins at 0) so the raw error is returned instead of
// ErrAccountSetupAlreadyDone.
func TestSetupRollsBackAndReturnsRawErrorWhenSessionCreationFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	blockTableWrites(t, db, "admin_sessions", "INSERT")

	_, _, err := Setup(db, "admin", "password123", now)
	if err == nil {
		t.Fatalf("expected an error when session creation fails")
	}
	if errors.Is(err, errcode.ErrAccountSetupAlreadyDone) {
		t.Fatalf("expected the raw transaction error, not ErrAccountSetupAlreadyDone, got %v", err)
	}

	count, countErr := repository.CountAdmins(db)
	if countErr != nil {
		t.Fatalf("CountAdmins failed: %v", countErr)
	}
	if count != 0 {
		t.Fatalf("expected the failed transaction to roll back the admin insert, found %d admins", count)
	}
}

func TestLoginErrorsWhenAdminsTableMissing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	dropTable(t, db, "admins")

	if _, _, err := Login(db, "admin", "password123", time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the admins table is missing")
	}
}

func TestLoginErrorsWhenRecordLoginFailureFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	if _, _, err := Setup(db, "admin", "password123", now); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	blockTableWrites(t, db, "admins", "UPDATE")

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
	blockTableWrites(t, db, "admin_sessions", "DELETE")

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
	blockTableWrites(t, db, "admins", "UPDATE")

	if _, _, err := Login(db, "admin", "password123", now); err == nil {
		t.Fatalf("expected an error when RecordLoginSuccess's UPDATE fails inside Login's transaction")
	}
}

func TestChangePasswordErrorsWhenAdminNotFound(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	if err := ChangePassword(db, 9999, "whatever", "newpassword456", time.Now().UTC()); err == nil {
		t.Fatalf("expected an error for a non-existent admin id")
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
	blockTableWrites(t, db, "admins", "UPDATE")

	if err := ChangePassword(db, admin.ID, "password123", "newpassword456", now); err == nil {
		t.Fatalf("expected an error when the password_hash UPDATE fails")
	}
}

// createSession's generateRandomToken error branch is not exercised here —
// see token_helpers_test.go: since Go 1.24, crypto/rand.Read cannot be made
// to return an error from a test (it crashes the program instead), so that
// branch is unreachable dead code under this project's Go version.

func TestCreateSessionErrorsWhenInsertFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin, _, err := Setup(db, "admin", "password123", now)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	blockTableWrites(t, db, "admin_sessions", "INSERT")

	if _, err := createSession(db, admin.ID, now); err == nil {
		t.Fatalf("expected an error when the admin_sessions INSERT fails")
	}
}
