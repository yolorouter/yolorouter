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

func seedUser(t *testing.T, db *gorm.DB, username string) *model.User {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	user := newBootstrapAdmin(username, now)
	if err := CreateUser(db, user); err != nil {
		t.Fatalf("seedUser CreateUser failed: %v", err)
	}
	return user
}

func TestCreateSessionAndFindUser(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	user := seedUser(t, db, "alice")
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateSession(db, "tok-1", user.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	found, err := FindUserByValidSession(db, "tok-1", now)
	if err != nil {
		t.Fatalf("FindUserByValidSession failed: %v", err)
	}
	if found.ID != user.ID || found.Username != "alice" || found.Role != model.RoleAdmin {
		t.Fatalf("expected session to resolve to the seeded user, got %+v", found)
	}
}

// TestFindUserByValidSessionReturnsDisabledUserAsIs pins the contract the
// middleware depends on: this lookup must NOT filter on user status, so
// a disabled account's session resolves to the user (and the middleware
// can answer with an explicit account-disabled response instead of a
// generic invalid-session 401). Adding a status filter here goes red.
func TestFindUserByValidSessionReturnsDisabledUserAsIs(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	user := seedUser(t, db, "muted")
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateSession(db, "tok-disabled", user.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", user.ID).
		Update("status", model.UserStatusDisabled).Error; err != nil {
		t.Fatalf("disable user failed: %v", err)
	}

	found, err := FindUserByValidSession(db, "tok-disabled", now)
	if err != nil {
		t.Fatalf("FindUserByValidSession failed: %v", err)
	}
	if found.Status != model.UserStatusDisabled {
		t.Fatalf("expected the disabled status to be returned as-is, got %d", found.Status)
	}
}

// TestCreateSessionStoresHashNotRawToken is the actual security property
// hashSessionToken exists for: if the raw token ever ended up in the
// user_sessions row unchanged, a leaked database file or db:backup
// output would hand out directly-replayable sessions. Querying the
// row back out via the model only proves round-trip correctness (already
// covered by TestCreateSessionAndFindUser) — this test reads the raw
// stored column value directly to prove it is NOT the token verbatim, and
// is exactly the expected SHA-256 digest.
func TestCreateSessionStoresHashNotRawToken(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	user := seedUser(t, db, "dave2")
	now := time.Now().UTC().Truncate(time.Second)

	const rawToken = "super-secret-raw-session-token"
	if err := CreateSession(db, rawToken, user.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	var storedID string
	if err := db.Raw(`SELECT id FROM user_sessions WHERE user_id = ?`, user.ID).Scan(&storedID).Error; err != nil {
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

func TestFindUserByValidSessionRejectsExpiredSession(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	user := seedUser(t, db, "bob")
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateSession(db, "tok-expired", user.ID, now.Add(-time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	_, err := FindUserByValidSession(db, "tok-expired", now)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound for expired session, got: %v", err)
	}
}

func TestDeleteSessionIsIdempotent(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	user := seedUser(t, db, "carol")
	now := time.Now().UTC().Truncate(time.Second)
	if err := CreateSession(db, "tok-del", user.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	if err := DeleteSession(db, "tok-del"); err != nil {
		t.Fatalf("first DeleteSession failed: %v", err)
	}
	if err := DeleteSession(db, "tok-del"); err != nil {
		t.Fatalf("second DeleteSession (idempotency) failed: %v", err)
	}

	_, err := FindUserByValidSession(db, "tok-del", now)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected session to be gone, got: %v", err)
	}
}

// TestDeleteAllSessionsForUserLeavesOtherUsersAlone verifies both halves
// of DeleteAllSessionsForUser: a user logged in from two browsers at
// once (two session rows, same user_id) has both deleted, while another
// user's session survives. The old single-admin schema made the
// cross-account half impossible to even construct; multi-account makes
// it a real invariant worth pinning.
func TestDeleteAllSessionsForUserLeavesOtherUsersAlone(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	target := seedUser(t, db, "alice2")
	other := newMemberUser("bystander", now)
	if err := CreateUser(db, other); err != nil {
		t.Fatalf("CreateUser(other) failed: %v", err)
	}

	if err := CreateSession(db, "browser-a-tok", target.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession(browser A) failed: %v", err)
	}
	if err := CreateSession(db, "browser-b-tok", target.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession(browser B) failed: %v", err)
	}
	if err := CreateSession(db, "other-tok", other.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession(other user) failed: %v", err)
	}

	if err := DeleteAllSessionsForUser(db, target.ID); err != nil {
		t.Fatalf("DeleteAllSessionsForUser failed: %v", err)
	}

	if _, err := FindUserByValidSession(db, "browser-a-tok", now); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected browser A's session to be gone, got: %v", err)
	}
	if _, err := FindUserByValidSession(db, "browser-b-tok", now); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected browser B's session to be gone, got: %v", err)
	}
	if _, err := FindUserByValidSession(db, "other-tok", now); err != nil {
		t.Fatalf("expected the other user's session to survive, got: %v", err)
	}
}

// TestDeleteExpiredSessionsRemovesOnlyExpiredRows guards the user_sessions
// unbounded-growth fix directly: FindUserByValidSession already filters
// expired rows out of query results (TestFindUserByValidSessionRejectsExpiredSession),
// so proving DeleteExpiredSessions actually removes the underlying row —
// not just that it's unreachable via that filtered query — needs a raw
// row count, the same way TestCreateSessionStoresHashNotRawToken reads the
// raw stored column instead of trusting a round trip through the model.
func TestDeleteExpiredSessionsRemovesOnlyExpiredRows(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	user := seedUser(t, db, "erin2")
	now := time.Now().UTC().Truncate(time.Second)

	if err := CreateSession(db, "tok-expired", user.ID, now.Add(-time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession(expired) failed: %v", err)
	}
	if err := CreateSession(db, "tok-valid", user.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession(valid) failed: %v", err)
	}

	if err := DeleteExpiredSessions(db, now); err != nil {
		t.Fatalf("DeleteExpiredSessions failed: %v", err)
	}

	var count int64
	if err := db.Model(&model.UserSession{}).Where("user_id = ?", user.ID).Count(&count).Error; err != nil {
		t.Fatalf("counting remaining sessions failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 session row to remain (the non-expired one), got %d", count)
	}
	if _, err := FindUserByValidSession(db, "tok-valid", now); err != nil {
		t.Fatalf("expected the non-expired session to still be valid, got: %v", err)
	}
}
