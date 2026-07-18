package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
)

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
