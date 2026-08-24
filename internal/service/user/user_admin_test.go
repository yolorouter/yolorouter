package user

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/auth"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// seedBootstrapAdmin inserts the first-run-setup account (the one local
// admin the escape-hatch guards protect), mirroring auth.Setup's output.
func seedBootstrapAdmin(t *testing.T, db *gorm.DB, username string) *model.User {
	t.Helper()
	now := time.Now().UTC()
	u := &model.User{Username: username, Role: model.RoleAdmin, Status: model.UserStatusEnabled,
		IsLocal: true, IsBootstrap: true, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(db, u); err != nil {
		t.Fatalf("seed bootstrap %s: %v", username, err)
	}
	return u
}

// seedDirectoryUser inserts one non-local account and returns it. Sessions
// are created separately where a test needs them.
func seedDirectoryUser(t *testing.T, db *gorm.DB, username, role string, status int) *model.User {
	t.Helper()
	now := time.Now().UTC()
	u := &model.User{Username: username, Role: role, Status: status,
		PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(db, u); err != nil {
		t.Fatalf("seed %s: %v", username, err)
	}
	return u
}

func TestSetUserStatusRefusesSelf(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedDirectoryUser(t, db, "boss", model.RoleAdmin, model.UserStatusEnabled)

	err := SetUserStatus(db, admin.ID, admin.ID, model.UserStatusDisabled, time.Now().UTC())
	if !errors.Is(err, errcode.ErrAccountSelfOperation) {
		t.Fatalf("self-disable: expected ErrAccountSelfOperation, got %v", err)
	}
	if err := SetUserRole(db, admin.ID, admin.ID, model.RoleMember, time.Now().UTC()); !errors.Is(err, errcode.ErrAccountSelfOperation) {
		t.Fatalf("self-demote: expected ErrAccountSelfOperation, got %v", err)
	}
}

func TestSetUserStatusProtectsLastAdmin(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedDirectoryUser(t, db, "boss", model.RoleAdmin, model.UserStatusEnabled)
	other := seedDirectoryUser(t, db, "other", model.RoleAdmin, model.UserStatusEnabled)
	member := seedDirectoryUser(t, db, "worker", model.RoleMember, model.UserStatusEnabled)
	now := time.Now().UTC()

	// With two enabled admins, disabling one is allowed.
	if err := SetUserStatus(db, admin.ID, other.ID, model.UserStatusDisabled, now); err != nil {
		t.Fatalf("disable second admin: %v", err)
	}
	// Now the first admin is the only enabled admin left. Over HTTP the
	// acting admin is always enabled and never the target, so this guard
	// only matters for concurrent transactions — the member's id merely
	// serves as a second actor id here, keeping actor != target while the
	// target is the last enabled admin.
	if err := SetUserStatus(db, member.ID, admin.ID, model.UserStatusDisabled, now); !errors.Is(err, errcode.ErrAccountLastAdminProtected) {
		t.Fatalf("disable last admin: expected ErrAccountLastAdminProtected, got %v", err)
	}
	if err := SetUserRole(db, member.ID, admin.ID, model.RoleMember, now); !errors.Is(err, errcode.ErrAccountLastAdminProtected) {
		t.Fatalf("demote last admin: expected ErrAccountLastAdminProtected, got %v", err)
	}

	// Re-enabling the second admin unblocks both operations.
	if err := SetUserStatus(db, admin.ID, other.ID, model.UserStatusEnabled, now); err != nil {
		t.Fatalf("re-enable second admin: %v", err)
	}
	if err := SetUserRole(db, other.ID, admin.ID, model.RoleMember, now); err != nil {
		t.Fatalf("demote admin with another enabled admin present: %v", err)
	}
}

func TestDemoteDisabledAdminSkipsLastAdminGuard(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedDirectoryUser(t, db, "boss", model.RoleAdmin, model.UserStatusEnabled)
	disabledAdmin := seedDirectoryUser(t, db, "gone", model.RoleAdmin, model.UserStatusDisabled)

	// A disabled admin contributes nothing to the enabled-admin pool, so
	// demoting them must not trip the guard even when only one enabled
	// admin exists.
	if err := SetUserRole(db, admin.ID, disabledAdmin.ID, model.RoleMember, time.Now().UTC()); err != nil {
		t.Fatalf("demote disabled admin: %v", err)
	}
}

func TestDisableDeletesSessionsAndIsReversible(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedDirectoryUser(t, db, "boss", model.RoleAdmin, model.UserStatusEnabled)
	member := seedDirectoryUser(t, db, "worker", model.RoleMember, model.UserStatusEnabled)
	now := time.Now().UTC()
	if err := repository.CreateSession(db, "tok-worker", member.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	if err := SetUserStatus(db, admin.ID, member.ID, model.UserStatusDisabled, now); err != nil {
		t.Fatalf("disable member: %v", err)
	}
	if _, err := repository.FindUserByValidSession(db, "tok-worker", now); err == nil {
		t.Fatalf("disable must delete the target's live sessions")
	}
	stored, err := repository.FindUserByID(db, member.ID)
	if err != nil {
		t.Fatalf("reload member: %v", err)
	}
	if stored.Status != model.UserStatusDisabled {
		t.Fatalf("member status not persisted: %+v", stored)
	}

	// Enable restores the account (sessions stay gone — they must log in
	// again).
	if err := SetUserStatus(db, admin.ID, member.ID, model.UserStatusEnabled, now); err != nil {
		t.Fatalf("re-enable member: %v", err)
	}
	stored, err = repository.FindUserByID(db, member.ID)
	if err != nil {
		t.Fatalf("reload member: %v", err)
	}
	if stored.Status != model.UserStatusEnabled {
		t.Fatalf("member not re-enabled: %+v", stored)
	}
}

func TestSetUserStatusUnknownTarget(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedDirectoryUser(t, db, "boss", model.RoleAdmin, model.UserStatusEnabled)

	if err := SetUserStatus(db, admin.ID, 99999, model.UserStatusDisabled, time.Now().UTC()); !errors.Is(err, errcode.ErrAccountUserNotFound) {
		t.Fatalf("unknown target: expected ErrAccountUserNotFound, got %v", err)
	}
	if err := SetUserRole(db, admin.ID, 99999, model.RoleAdmin, time.Now().UTC()); !errors.Is(err, errcode.ErrAccountUserNotFound) {
		t.Fatalf("unknown target: expected ErrAccountUserNotFound, got %v", err)
	}
}

func TestListUsersCarriesDirectoryAggregates(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	member := seedDirectoryUser(t, db, "worker", model.RoleMember, model.UserStatusEnabled)
	now := time.Now().UTC()

	key := &model.APIKey{KeyHash: "hash-1", KeyPrefix: "sk-yr-1", UserID: member.ID,
		Status: model.APIKeyStatusActive, AllowAllModels: true, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateAPIKey(db, key, nil, now); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	log := &model.RequestLog{RequestID: "req-1", APIKeyID: &key.ID, UserID: &member.ID,
		ModelName: "m", StatusCode: 200, CostMicros: 1234, CostKnown: true, CreatedAt: now}
	if err := repository.CreateRequestLog(db, log); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	views, err := ListUsers(db)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	var got *UserSummaryView
	for i := range views {
		if views[i].ID == member.ID {
			got = &views[i]
		}
	}
	if got == nil {
		t.Fatalf("member missing from directory")
	}
	if got.KeyCount != 1 || got.SpendMicros != 1234 {
		t.Fatalf("directory aggregates wrong: %+v", got)
	}
	if got.Providers == nil {
		t.Fatalf("providers must be an empty list, not null, for JSON consumers")
	}
}

// TestBootstrapAccountCannotBeDisabledOrDemoted: the first-run setup
// account is the documented OAuth-failure escape hatch — no admin may
// take it out, even with other enabled admins around.
func TestBootstrapAccountCannotBeDisabledOrDemoted(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	local := seedBootstrapAdmin(t, db, "boss")
	other := seedDirectoryUser(t, db, "other", model.RoleAdmin, model.UserStatusEnabled)
	now := time.Now().UTC()

	if err := SetUserStatus(db, other.ID, local.ID, model.UserStatusDisabled, now); !errors.Is(err, errcode.ErrAccountBootstrapProtected) {
		t.Fatalf("disable bootstrap account: expected ErrAccountBootstrapProtected, got %v", err)
	}
	if err := SetUserRole(db, other.ID, local.ID, model.RoleMember, now); !errors.Is(err, errcode.ErrAccountBootstrapProtected) {
		t.Fatalf("demote bootstrap account: expected ErrAccountBootstrapProtected, got %v", err)
	}
	stored, err := repository.FindUserByID(db, local.ID)
	if err != nil {
		t.Fatalf("reload bootstrap: %v", err)
	}
	if stored.Status != model.UserStatusEnabled || stored.Role != model.RoleAdmin {
		t.Fatalf("bootstrap account was mutated: %+v", stored)
	}
}

// TestCreateUserProvisionsLocalMember: the console-created account is an
// enabled local member with a working password — the full lifecycle of a
// password account minus the bootstrap protections.
func TestCreateUserProvisionsLocalMember(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedBootstrapAdmin(t, db, "boss")

	created, err := CreateUser(db, CreateUserInput{Username: "carol", DisplayName: "Carol from ops", Password: "correct-horse-1"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	stored, err := repository.FindUserByID(db, created.ID)
	if err != nil {
		t.Fatalf("reload created: %v", err)
	}
	if stored.Username != "carol" || stored.DisplayName != "Carol from ops" ||
		stored.Role != model.RoleMember || stored.Status != model.UserStatusEnabled ||
		!stored.IsLocal || stored.IsBootstrap {
		t.Fatalf("created account has wrong shape: %+v", stored)
	}
	if !auth.CheckPassword(stored.PasswordHash, "correct-horse-1") {
		t.Fatalf("stored hash must verify against the submitted password")
	}
	// The account must actually sign in through the password form.
	user, _, err := auth.Login(db, "carol", "correct-horse-1", time.Now().UTC())
	if err != nil || user.ID != stored.ID {
		t.Fatalf("created account must log in with its password, got user=%v err=%v", user, err)
	}
}

// TestCreateUserUsernameTaken: the name-taken error fires for a collision
// with another console-created account and with an externally-provisioned
// one alike — usernames are unique across the whole directory.
func TestCreateUserUsernameTaken(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedBootstrapAdmin(t, db, "boss")
	seedDirectoryUser(t, db, "carol", model.RoleMember, model.UserStatusEnabled)

	if _, err := CreateUser(db, CreateUserInput{Username: "carol", Password: "correct-horse-1"}, time.Now().UTC()); !errors.Is(err, errcode.ErrAccountUsernameTaken) {
		t.Fatalf("collision with external account: expected ErrAccountUsernameTaken, got %v", err)
	}
	if _, err := CreateUser(db, CreateUserInput{Username: "boss", Password: "correct-horse-1"}, time.Now().UTC()); !errors.Is(err, errcode.ErrAccountUsernameTaken) {
		t.Fatalf("collision with bootstrap account: expected ErrAccountUsernameTaken, got %v", err)
	}
}

// TestCreateUserRecordsEmail: the optional email is stored verbatim for
// the directory — nothing in this build consumes it beyond display.
func TestCreateUserRecordsEmail(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedBootstrapAdmin(t, db, "boss")

	created, err := CreateUser(db, CreateUserInput{Username: "carol", Email: "carol@ops.example", Password: "correct-horse-1"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	stored, err := repository.FindUserByID(db, created.ID)
	if err != nil {
		t.Fatalf("reload created: %v", err)
	}
	if stored.Email != "carol@ops.example" {
		t.Fatalf("email not recorded: %q", stored.Email)
	}
}

// TestResetUserPasswordByBootstrapAdmin: the bootstrap admin rotates a
// console-created member's password — the old password stops working, the
// target's live sessions die, and the new password signs in.
func TestResetUserPasswordByBootstrapAdmin(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	bootstrap := seedBootstrapAdmin(t, db, "boss")
	created, err := CreateUser(db, CreateUserInput{Username: "carol", Password: "correct-horse-1"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	if err := repository.CreateSession(db, "tok-carol-reset", created.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	if err := ResetUserPassword(db, bootstrap.ID, created.ID, "fresh-horse-2", now); err != nil {
		t.Fatalf("ResetUserPassword: %v", err)
	}
	if _, err := repository.FindUserByValidSession(db, "tok-carol-reset", now); err == nil {
		t.Fatalf("reset must kill the target's live sessions")
	}
	if _, _, err := auth.Login(db, "carol", "correct-horse-1", now); !errors.Is(err, errcode.ErrAccountInvalidCredentials) {
		t.Fatalf("old password: expected ErrAccountInvalidCredentials, got %v", err)
	}
	user, _, err := auth.Login(db, "carol", "fresh-horse-2", now)
	if err != nil || user.ID != created.ID {
		t.Fatalf("new password must sign in, got user=%v err=%v", user, err)
	}
}

// TestResetUserPasswordDeniedOutsideItsLane: self, a non-bootstrap admin,
// and an externally-provisioned target all refuse with the same code;
// an unknown target keeps the not-found answer.
func TestResetUserPasswordDeniedOutsideItsLane(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	bootstrap := seedBootstrapAdmin(t, db, "boss")
	promoted := seedDirectoryUser(t, db, "promoted", model.RoleAdmin, model.UserStatusEnabled)
	oauth := seedDirectoryUser(t, db, "ext", model.RoleMember, model.UserStatusEnabled)
	now := time.Now().UTC()

	if err := ResetUserPassword(db, bootstrap.ID, bootstrap.ID, "fresh-horse-2", now); !errors.Is(err, errcode.ErrAccountPasswordResetDenied) {
		t.Fatalf("self-reset: expected ErrAccountPasswordResetDenied, got %v", err)
	}
	if err := ResetUserPassword(db, promoted.ID, oauth.ID, "fresh-horse-2", now); !errors.Is(err, errcode.ErrAccountPasswordResetDenied) {
		t.Fatalf("non-bootstrap actor: expected ErrAccountPasswordResetDenied, got %v", err)
	}
	if err := ResetUserPassword(db, bootstrap.ID, oauth.ID, "fresh-horse-2", now); !errors.Is(err, errcode.ErrAccountPasswordResetDenied) {
		t.Fatalf("non-local target: expected ErrAccountPasswordResetDenied, got %v", err)
	}
	if err := ResetUserPassword(db, bootstrap.ID, 99999, "fresh-horse-2", now); !errors.Is(err, errcode.ErrAccountUserNotFound) {
		t.Fatalf("unknown target: expected ErrAccountUserNotFound, got %v", err)
	}
}

// TestLocalMemberIsFullyManageable: escape-hatch protections key on
// is_bootstrap, not is_local — a console-created local account can be
// promoted, demoted, and disabled like any externally-provisioned one.
// Reverting the guards to is_local reddens the demote and the disable;
// the promote passes either way, since raising someone to admin never
// reaches a guard that only refuses demotions.
func TestLocalMemberIsFullyManageable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedBootstrapAdmin(t, db, "boss")
	created, err := CreateUser(db, CreateUserInput{Username: "dave", Password: "correct-horse-1"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()

	if err := SetUserRole(db, 1, created.ID, model.RoleAdmin, now); err != nil {
		t.Fatalf("promote local member: %v", err)
	}
	if err := SetUserRole(db, 1, created.ID, model.RoleMember, now); err != nil {
		t.Fatalf("demote local member: %v", err)
	}
	if err := SetUserStatus(db, 1, created.ID, model.UserStatusDisabled, now); err != nil {
		t.Fatalf("disable local member: %v", err)
	}
}

// TestSessionCreationGuardedByStatus: the conditional insert refuses to
// mint a session for a disabled account — the race-closing half of the
// disable cascade (a login that read the user as enabled but persists
// its session after the disable committed).
func TestSessionCreationGuardedByStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedDirectoryUser(t, db, "boss", model.RoleAdmin, model.UserStatusEnabled)
	member := seedDirectoryUser(t, db, "worker", model.RoleMember, model.UserStatusEnabled)
	now := time.Now().UTC()

	if err := SetUserStatus(db, admin.ID, member.ID, model.UserStatusDisabled, now); err != nil {
		t.Fatalf("disable member: %v", err)
	}
	err := repository.CreateSession(db, "tok-late-login", member.ID, now.Add(time.Hour), now)
	if !errors.Is(err, errcode.ErrAccountDisabled) {
		t.Fatalf("session for disabled account: expected ErrAccountDisabled, got %v", err)
	}
}

// TestEnableKillsOrphanSessions: a session row that slipped past the
// insert guard while the account was disabled (concurrent transactions
// can interleave that way) must not come back to life on re-enable.
func TestEnableKillsOrphanSessions(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedDirectoryUser(t, db, "boss", model.RoleAdmin, model.UserStatusEnabled)
	member := seedDirectoryUser(t, db, "worker", model.RoleMember, model.UserStatusEnabled)
	now := time.Now().UTC()

	if err := SetUserStatus(db, admin.ID, member.ID, model.UserStatusDisabled, now); err != nil {
		t.Fatalf("disable member: %v", err)
	}
	// Plant the orphan directly, bypassing the guarded insert — this is
	// the row a mid-transaction race would leave behind.
	orphan := &model.UserSession{TokenHash: "orphan-hash", UserID: member.ID,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	if err := db.Create(orphan).Error; err != nil {
		t.Fatalf("plant orphan session: %v", err)
	}

	if err := SetUserStatus(db, admin.ID, member.ID, model.UserStatusEnabled, now); err != nil {
		t.Fatalf("re-enable member: %v", err)
	}
	var count int64
	if err := db.Model(&model.UserSession{}).Where("user_id = ?", member.ID).Count(&count).Error; err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("re-enable must kill orphan sessions, %d survived", count)
	}
}

// TestRoleChangeEndsSessions: promotion and demotion both force a fresh
// login — a live SPA only learns its role at boot, so a preserved
// session would leave the client running with the stale role.
func TestRoleChangeEndsSessions(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	admin := seedDirectoryUser(t, db, "boss", model.RoleAdmin, model.UserStatusEnabled)
	member := seedDirectoryUser(t, db, "worker", model.RoleMember, model.UserStatusEnabled)
	now := time.Now().UTC()
	if err := repository.CreateSession(db, "tok-worker", member.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	if err := SetUserRole(db, admin.ID, member.ID, model.RoleAdmin, now); err != nil {
		t.Fatalf("promote member: %v", err)
	}
	if _, err := repository.FindUserByValidSession(db, "tok-worker", now); err == nil {
		t.Fatalf("role change must end the target's sessions")
	}
}
