// Package handler: user directory endpoints.
package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/user"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// GetUsers handles GET /api/admin/users — the full account list for the
// admin's user directory and the "filter by user" dropdowns on the
// statistics pages. Small deployments (company-internal), so no pagination.
func GetUsers(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		users, err := user.ListUsers(db)
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		response.Success(c, gin.H{"users": users})
	}
}

type createUserRequest struct {
	// Same username/password rules as first-run setup — a console-created
	// account is just another local password account.
	Username string `json:"username" binding:"required,min=3,max=32,alnum_dash"`
	// Optional; empty means the username doubles as the display name in
	// the UI. Bound to the users.display_name column width.
	DisplayName string `json:"display_name" binding:"omitempty,max=128"`
	// Optional, informational only — this build sends no mail. Bound to
	// the users.email column width.
	Email    string `json:"email" binding:"omitempty,email,max=255"`
	Password string `json:"password" binding:"required,min=10,alnum_mixed,bcrypt_len"`
}

type updateUserStatusRequest struct {
	// "enabled" | "disabled" on the wire — the numeric status codes are a
	// storage detail the API does not expose for writes.
	Status string `json:"status" binding:"required,oneof=enabled disabled"`
}

type updateUserRoleRequest struct {
	Role string `json:"role" binding:"required,oneof=admin member"`
}

type updateUserProfileRequest struct {
	// Same field rules as account creation — the profile edit only
	// narrows which actor may send them. Pointers keep the sparse-PATCH
	// contract honest: nil means "leave the field alone" (never sent),
	// while an explicit empty string means "clear it". A bare string
	// would fold the two into the same "" and let a single-field patch
	// silently wipe the other column. email_or_empty (not `email`)
	// because `omitempty` treats a non-nil pointer as present and would
	// run the email rule on the clear-value "".
	DisplayName *string `json:"display_name" binding:"omitempty,max=128"`
	Email       *string `json:"email" binding:"omitempty,email_or_empty,max=255"`
}

// writeUserServiceError maps the user-management service sentinels onto
// the admin envelope. Every rule violation is a 4xx with its own code so
// the frontend can show a precise message.
func writeUserServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errcode.ErrAccountSelfOperation):
		middleware.WriteAdminError(c, http.StatusConflict, errcode.AccountSelfOperation)
	case errors.Is(err, errcode.ErrAccountLastAdminProtected):
		middleware.WriteAdminError(c, http.StatusConflict, errcode.AccountLastAdminProtected)
	case errors.Is(err, errcode.ErrAccountBootstrapProtected):
		middleware.WriteAdminError(c, http.StatusConflict, errcode.AccountBootstrapProtected)
	case errors.Is(err, errcode.ErrAccountUsernameTaken):
		middleware.WriteAdminError(c, http.StatusConflict, errcode.AccountUsernameTaken)
	case errors.Is(err, errcode.ErrAccountPasswordResetDenied):
		// 403 rather than 409: the caller is asking for a power their
		// account does not hold, not colliding with another writer.
		middleware.WriteAdminError(c, http.StatusForbidden, errcode.AccountPasswordResetDenied)
	case errors.Is(err, errcode.ErrAccountProfileEditDenied):
		middleware.WriteAdminError(c, http.StatusForbidden, errcode.AccountProfileEditDenied)
	case errors.Is(err, errcode.ErrAccountUserNotFound):
		middleware.WriteAdminError(c, http.StatusNotFound, errcode.AccountUserNotFound)
	default:
		middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
	}
}

// PostUser handles POST /api/admin/users — provisioning a local password
// member from the console. The password travels once, in this request, and
// only its bcrypt hash is stored; nothing about it is echoed back.
func PostUser(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createUserRequest
		if !bindJSON(c, &req) {
			return
		}
		created, err := user.CreateUser(db, user.CreateUserInput{
			Username:    req.Username,
			DisplayName: req.DisplayName,
			Email:       req.Email,
			Password:    req.Password,
		}, time.Now().UTC())
		if err != nil {
			writeUserServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"user": user.SummaryView(created)})
	}
}

type resetPasswordRequest struct {
	Password string `json:"password" binding:"required,min=10,alnum_mixed,bcrypt_len"`
}

// PostUserPasswordReset handles POST /api/admin/users/:id/password — the
// bootstrap administrator replacing another local account's password. The
// password travels once, in this request, and only its bcrypt hash is
// stored; every live session of the target dies with the reset.
func PostUserPasswordReset(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req resetPasswordRequest
		if !bindJSON(c, &req) {
			return
		}
		actorID := c.MustGet(middleware.UserIDKey).(uint)
		if err := user.ResetUserPassword(db, actorID, id, req.Password, time.Now().UTC()); err != nil {
			writeUserServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

// PatchUserProfile handles PATCH /api/admin/users/:id/profile — the
// bootstrap administrator rewriting another account's display name and
// email. Directory information only: nothing about login or permissions
// changes, so the target's sessions sail through untouched.
func PatchUserProfile(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req updateUserProfileRequest
		if !bindJSON(c, &req) {
			return
		}
		actorID := c.MustGet(middleware.UserIDKey).(uint)
		if err := user.UpdateUserProfile(db, actorID, id, req.DisplayName, req.Email, time.Now().UTC()); err != nil {
			writeUserServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

// PatchUserStatus handles PATCH /api/admin/users/:id/status — the
// disable/enable switch of the user directory. Disabling cascades:
// sessions die in the same transaction, and the gateway starts rejecting
// the account's API keys immediately (owner status is checked at key
// auth time, so no key rows are mutated and re-enabling needs no repair).
func PatchUserStatus(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req updateUserStatusRequest
		if !bindJSON(c, &req) {
			return
		}
		status := model.UserStatusEnabled
		if req.Status == "disabled" {
			status = model.UserStatusDisabled
		}
		actorID := c.MustGet(middleware.UserIDKey).(uint)
		if err := user.SetUserStatus(db, actorID, id, status, time.Now().UTC()); err != nil {
			writeUserServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

// PatchUserRole handles PATCH /api/admin/users/:id/role — promotion and
// demotion. Promoting an externally-provisioned account is the one and
// only path to an OAuth-backed administrator.
func PatchUserRole(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req updateUserRoleRequest
		if !bindJSON(c, &req) {
			return
		}
		actorID := c.MustGet(middleware.UserIDKey).(uint)
		if err := user.SetUserRole(db, actorID, id, req.Role, time.Now().UTC()); err != nil {
			writeUserServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}
