package repository

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
)

func seedAdmin(t *testing.T, db *gorm.DB, username string) *model.Admin {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	admin := &model.Admin{Username: username, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := CreateAdmin(db, admin); err != nil {
		t.Fatalf("seedAdmin CreateAdmin failed: %v", err)
	}
	return admin
}

func TestCreateSessionAndFindValid(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedAdmin(t, db, "alice")
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateSession(db, "tok-1", admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	found, err := FindValidSessionByID(db, "tok-1", now)
	if err != nil {
		t.Fatalf("FindValidSessionByID failed: %v", err)
	}
	if found.AdminID != admin.ID {
		t.Fatalf("expected admin_id=%d, got %d", admin.ID, found.AdminID)
	}
}

// TestCreateSessionStoresHashNotRawToken is the actual security property
// hashSessionToken exists for: if the raw token ever ended up in the
// admins_sessions row unchanged, a leaked database file or db:backup
// output would hand out directly-replayable admin sessions. Querying the
// row back out via the model only proves round-trip correctness (already
// covered by TestCreateSessionAndFindValid) — this test reads the raw
// stored column value directly to prove it is NOT the token verbatim, and
// is exactly the expected SHA-256 digest.
func TestCreateSessionStoresHashNotRawToken(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedAdmin(t, db, "dave2")
	now := time.Now().UTC().Truncate(time.Second)

	const rawToken = "super-secret-raw-session-token"
	if err := CreateSession(db, rawToken, admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	var storedID string
	if err := db.Raw(`SELECT id FROM admin_sessions WHERE admin_id = ?`, admin.ID).Scan(&storedID).Error; err != nil {
		t.Fatalf("querying raw stored id failed: %v", err)
	}
	if storedID == rawToken {
		t.Fatalf("expected the stored id to be a hash of the raw token, but it was stored verbatim")
	}
	if storedID != hashSessionToken(rawToken) {
		t.Fatalf("expected stored id to be hashSessionToken(rawToken) = %q, got %q", hashSessionToken(rawToken), storedID)
	}
	if len(storedID) != 64 || strings.ToLower(storedID) != storedID {
		t.Fatalf("expected a 64-char lowercase hex SHA-256 digest, got %q (len %d)", storedID, len(storedID))
	}
}

func TestFindValidSessionByIDRejectsExpiredSession(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedAdmin(t, db, "bob")
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateSession(db, "tok-expired", admin.ID, now.Add(-time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	_, err := FindValidSessionByID(db, "tok-expired", now)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound for expired session, got: %v", err)
	}
}

func TestDeleteSessionIsIdempotent(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedAdmin(t, db, "carol")
	now := time.Now().UTC().Truncate(time.Second)
	if err := CreateSession(db, "tok-del", admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	if err := DeleteSession(db, "tok-del"); err != nil {
		t.Fatalf("first DeleteSession failed: %v", err)
	}
	if err := DeleteSession(db, "tok-del"); err != nil {
		t.Fatalf("second DeleteSession (idempotency) failed: %v", err)
	}

	_, err := FindValidSessionByID(db, "tok-del", now)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected session to be gone, got: %v", err)
	}
}

// TestDeleteAllSessionsForAdminDeletesEveryMatchingSession verifies the
// "All" in DeleteAllSessionsForAdmin: an admin logged in from two browsers
// at once (two session rows, same admin_id) must have both deleted, not
// just one. A prior version of this test used two separate *admin* rows to
// also prove cross-admin isolation, but admins.singleton_guard (migration
// 00002_create_admin_auth.sql) now makes a second admin row impossible to
// construct at all — the WHERE admin_id = ? clause's cross-admin
// correctness is enforced by that constraint, not something a v0.1 test
// can (or needs to) exercise directly.
func TestDeleteAllSessionsForAdminDeletesEveryMatchingSession(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedAdmin(t, db, "alice2")
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateSession(db, "browser-a-tok", admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession(browser A) failed: %v", err)
	}
	if err := CreateSession(db, "browser-b-tok", admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession(browser B) failed: %v", err)
	}

	if err := DeleteAllSessionsForAdmin(db, admin.ID); err != nil {
		t.Fatalf("DeleteAllSessionsForAdmin failed: %v", err)
	}

	if _, err := FindValidSessionByID(db, "browser-a-tok", now); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected browser A's session to be gone, got: %v", err)
	}
	if _, err := FindValidSessionByID(db, "browser-b-tok", now); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected browser B's session to be gone, got: %v", err)
	}
}

// TestDeleteExpiredSessionsRemovesOnlyExpiredRows guards the admin_sessions
// unbounded-growth fix directly: FindValidSessionByID already filters
// expired rows out of query results (TestFindValidSessionByIDRejectsExpiredSession),
// so proving DeleteExpiredSessions actually removes the underlying row —
// not just that it's unreachable via that filtered query — needs a raw
// row count, the same way TestCreateSessionStoresHashNotRawToken reads the
// raw stored column instead of trusting a round trip through the model.
func TestDeleteExpiredSessionsRemovesOnlyExpiredRows(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedAdmin(t, db, "erin")
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateSession(db, "tok-expired", admin.ID, now.Add(-time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession(expired) failed: %v", err)
	}
	if err := CreateSession(db, "tok-valid", admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession(valid) failed: %v", err)
	}

	if err := DeleteExpiredSessions(db, now); err != nil {
		t.Fatalf("DeleteExpiredSessions failed: %v", err)
	}

	var count int64
	if err := db.Model(&model.AdminSession{}).Where("admin_id = ?", admin.ID).Count(&count).Error; err != nil {
		t.Fatalf("counting remaining sessions failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 session row to remain (the non-expired one), got %d", count)
	}
	if _, err := FindValidSessionByID(db, "tok-valid", now); err != nil {
		t.Fatalf("expected the non-expired session to still be valid, got: %v", err)
	}
}
