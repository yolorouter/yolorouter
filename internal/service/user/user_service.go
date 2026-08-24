package user

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/auth"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// UserSummaryView is the list shape for the admin's user directory and the
// data source behind "filter by user" dropdowns. Deliberately excludes the
// lockout internals and any credential material — the model's json:"-" tags
// already guard those, but this view narrows the wire contract to exactly
// what the admin UI consumes.
type UserSummaryView struct {
	ID          uint   `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	Status      int    `json:"status"`
	IsLocal     bool   `json:"is_local"`
	// IsBootstrap marks the first-run setup account: the escape hatch that
	// can never be disabled or demoted, rendered as a badge in the admin
	// UI with its row's actions hidden.
	IsBootstrap bool       `json:"is_bootstrap"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
	// Directory aggregates: which login providers the account arrived
	// through (empty for local accounts), how many API keys it owns,
	// and its lifetime known spend.
	Providers   []string `json:"providers"`
	KeyCount    int64    `json:"key_count"`
	SpendMicros int64    `json:"spend_micros"`
}

// ListUsers returns every account for the admin UI.
func ListUsers(db *gorm.DB) ([]UserSummaryView, error) {
	users, err := repository.ListUsers(db)
	if err != nil {
		return nil, err
	}
	agg, err := repository.LoadUserDirectoryAggregates(db)
	if err != nil {
		return nil, err
	}
	views := make([]UserSummaryView, 0, len(users))
	for i := range users {
		view := SummaryView(&users[i])
		if providers := agg.ProviderNames[users[i].ID]; providers != nil {
			view.Providers = providers
		}
		view.KeyCount = agg.KeyCounts[users[i].ID]
		view.SpendMicros = agg.SpendMicros[users[i].ID]
		views = append(views, view)
	}
	return views, nil
}

// CreateUserInput names the fields a console-provisioned account is
// created from — the same shape the frontend's createUser posts.
type CreateUserInput struct {
	Username    string
	DisplayName string
	// Email is informational only: this build sends no mail, so the
	// address is recorded for the directory, nothing else.
	Email    string
	Password string
}

// CreateUser provisions one local password account from the console:
// an enabled member that signs in with username+password and can be
// disabled or promoted afterwards like any other account. Only the
// bootstrap account (first-run setup) is exempt from that lifecycle, and
// only setup mints one — everything created here is is_bootstrap=false.
//
// The returned user carries the password hash; callers serialize through
// UserSummaryView and never emit it.
func CreateUser(db *gorm.DB, input CreateUserInput, now time.Time) (*model.User, error) {
	hash, err := auth.HashPassword(input.Password)
	if err != nil {
		return nil, err
	}
	user := &model.User{
		Username:     input.Username,
		PasswordHash: hash,
		DisplayName:  input.DisplayName,
		Email:        input.Email,
		Role:         model.RoleMember,
		Status:       model.UserStatusEnabled,
		IsLocal:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := repository.CreateUser(db, user); err != nil {
		// users.username is UNIQUE across local and externally
		// provisioned accounts alike, so a collision with either is the
		// same "pick another name" outcome.
		if repository.IsUniqueViolation(err) {
			return nil, errcode.ErrAccountUsernameTaken
		}
		return nil, err
	}
	return user, nil
}

// SummaryView narrows a model.User to the directory wire shape. Directory
// aggregates (providers/keys/spend) are zero here — they only make sense
// for the listed view, which fills them from the aggregate query.
func SummaryView(u *model.User) UserSummaryView {
	return UserSummaryView{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Email:       u.Email,
		Role:        u.Role,
		Status:      u.Status,
		IsLocal:     u.IsLocal,
		IsBootstrap: u.IsBootstrap,
		LastLoginAt: u.LastLoginAt,
		CreatedAt:   u.CreatedAt,
		Providers:   []string{},
	}
}

// ResetUserPassword replaces another account's password on behalf of the
// bootstrap administrator — the one account the schema pins as the
// deployment's anchor. Promoted admins manage roles and status but never
// credentials: concentrating credential rotation on the permanent anchor
// keeps a promoted-and-rogue admin from taking over every account. The
// remaining refusals share the same code because they are the same
// boundary seen from different sides: nobody resets their own password
// here (the change-password entry does that), and an externally
// provisioned account has no password to reset.
//
// Every live session of the target dies with the reset, in the same
// transaction as the hash change — the old password must stop working
// immediately, not at next navigation.
func ResetUserPassword(db *gorm.DB, actorID, targetID uint, newPassword string, now time.Time) error {
	if actorID == targetID {
		return errcode.ErrAccountPasswordResetDenied
	}
	return db.Transaction(func(tx *gorm.DB) error {
		actor, err := repository.FindUserByID(tx, actorID)
		if err != nil {
			return err
		}
		if !actor.IsBootstrap {
			return errcode.ErrAccountPasswordResetDenied
		}
		target, err := repository.FindUserByID(tx, targetID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errcode.ErrAccountUserNotFound
			}
			return err
		}
		if !target.IsLocal {
			return errcode.ErrAccountPasswordResetDenied
		}
		hash, err := auth.HashPassword(newPassword)
		if err != nil {
			return err
		}
		if err := repository.UpdateUserPasswordHash(tx, targetID, hash, now); err != nil {
			return err
		}
		return repository.DeleteAllSessionsForUser(tx, targetID)
	})
}

// SetUserStatus enables or disables one account, acting on behalf of
// actorID. Rules, all enforced here rather than in the handler:
//
//   - You cannot change your own account (ErrAccountSelfOperation) — a
//     mis-click must never lock the acting admin out mid-session.
//   - Disabling the last enabled administrator is refused
//     (ErrAccountLastAdminProtected); the count runs inside the same
//     transaction as the write so two concurrent disables cannot both
//     see "another admin remains".
//   - Disabling deletes every live session of the target immediately —
//     "disabled" must mean logged out now, not at next navigation. The
//     target's API keys are not mutated: the gateway rejects keys owned
//     by a disabled account at auth time, so re-enabling restores them
//     without any state to repair.
func SetUserStatus(db *gorm.DB, actorID, targetID uint, status int, now time.Time) error {
	if actorID == targetID {
		return errcode.ErrAccountSelfOperation
	}
	return db.Transaction(func(tx *gorm.DB) error {
		target, err := repository.FindUserByID(tx, targetID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errcode.ErrAccountUserNotFound
			}
			return err
		}
		if target.Status == status {
			return nil
		}
		// The bootstrap account is the documented escape hatch for when
		// external login breaks — disabling it would leave the deployment
		// unrepairable without database surgery. Admin-created local
		// accounts are deliberately not covered: they are ordinary
		// members, free to disable like any other.
		if status == model.UserStatusDisabled && target.IsBootstrap {
			return errcode.ErrAccountBootstrapProtected
		}
		if status == model.UserStatusDisabled && target.Role == model.RoleAdmin {
			remaining, err := repository.CountEnabledAdminsExcluding(tx, targetID)
			if err != nil {
				return err
			}
			if remaining == 0 {
				return errcode.ErrAccountLastAdminProtected
			}
		}
		if err := repository.UpdateUserStatus(tx, targetID, status, now); err != nil {
			return err
		}
		// Sessions die on BOTH transitions. Disable: "disabled" must mean
		// logged out now. Enable: a login that raced the disable can have
		// inserted a session after the disable's deletion (the insert-time
		// guard in repository.CreateSession narrows but cannot fully close
		// that window under concurrent transactions) — such a session was
		// never usable while disabled, and deleting here guarantees it
		// does not silently come back to life; re-enabled accounts start
		// from a fresh login.
		return repository.DeleteAllSessionsForUser(tx, targetID)
	})
}

// SetUserRole promotes or demotes one account, acting on behalf of
// actorID. Same self-operation rule as SetUserStatus, and demoting the
// last enabled administrator is refused for the same lockout reason.
// Promotion is how an externally-provisioned (OAuth) account becomes an
// administrator — there is no separate path.
func SetUserRole(db *gorm.DB, actorID, targetID uint, role string, now time.Time) error {
	if actorID == targetID {
		return errcode.ErrAccountSelfOperation
	}
	return db.Transaction(func(tx *gorm.DB) error {
		target, err := repository.FindUserByID(tx, targetID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errcode.ErrAccountUserNotFound
			}
			return err
		}
		if target.Role == role {
			return nil
		}
		// Same escape-hatch rule as SetUserStatus: a demoted bootstrap
		// account could still log in after an OAuth outage but couldn't
		// repair anything. A local-but-not-bootstrap account may be demoted
		// freely (promoted back the usual way if needed).
		if role == model.RoleMember && target.IsBootstrap {
			return errcode.ErrAccountBootstrapProtected
		}
		if role == model.RoleMember && target.Role == model.RoleAdmin && target.Status == model.UserStatusEnabled {
			remaining, err := repository.CountEnabledAdminsExcluding(tx, targetID)
			if err != nil {
				return err
			}
			if remaining == 0 {
				return errcode.ErrAccountLastAdminProtected
			}
		}
		if err := repository.UpdateUserRole(tx, targetID, role, now); err != nil {
			return err
		}
		// A live SPA only learns its role at boot/login: without this, a
		// promoted member stays locked out by the frontend router and a
		// demoted admin keeps admin navigation into a wall of 403s until a
		// hard refresh. Ending the sessions forces a fresh login that
		// picks up the new role — the same reconciliation disable uses.
		return repository.DeleteAllSessionsForUser(tx, targetID)
	})
}
