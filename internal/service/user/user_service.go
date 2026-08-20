package user

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// UserSummaryView is the list shape for the admin's user directory and the
// data source behind "filter by user" dropdowns. Deliberately excludes the
// lockout internals and any credential material — the model's json:"-" tags
// already guard those, but this view narrows the wire contract to exactly
// what the admin UI consumes.
type UserSummaryView struct {
	ID          uint       `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name"`
	Email       string     `json:"email"`
	Role        string     `json:"role"`
	Status      int        `json:"status"`
	IsLocal     bool       `json:"is_local"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
	// Directory aggregates: which login providers the account arrived
	// through (empty for the local account), how many API keys it owns,
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
	for _, u := range users {
		providers := agg.ProviderNames[u.ID]
		if providers == nil {
			providers = []string{}
		}
		views = append(views, UserSummaryView{
			ID:          u.ID,
			Username:    u.Username,
			DisplayName: u.DisplayName,
			Email:       u.Email,
			Role:        u.Role,
			Status:      u.Status,
			IsLocal:     u.IsLocal,
			LastLoginAt: u.LastLoginAt,
			CreatedAt:   u.CreatedAt,
			Providers:   providers,
			KeyCount:    agg.KeyCounts[u.ID],
			SpendMicros: agg.SpendMicros[u.ID],
		})
	}
	return views, nil
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
		// The local password account is the documented escape hatch for
		// when OAuth breaks — disabling it would leave the deployment
		// unrepairable without database surgery.
		if status == model.UserStatusDisabled && target.IsLocal {
			return errcode.ErrAccountLocalProtected
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
		// Same escape-hatch rule as SetUserStatus: a demoted local account
		// could still log in after an OAuth outage but couldn't repair
		// anything.
		if role == model.RoleMember && target.IsLocal {
			return errcode.ErrAccountLocalProtected
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
