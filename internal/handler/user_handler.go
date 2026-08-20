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

type updateUserStatusRequest struct {
	// "enabled" | "disabled" on the wire — the numeric status codes are a
	// storage detail the API does not expose for writes.
	Status string `json:"status" binding:"required,oneof=enabled disabled"`
}

type updateUserRoleRequest struct {
	Role string `json:"role" binding:"required,oneof=admin member"`
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
	case errors.Is(err, errcode.ErrAccountLocalProtected):
		middleware.WriteAdminError(c, http.StatusConflict, errcode.AccountLocalProtected)
	case errors.Is(err, errcode.ErrAccountUserNotFound):
		middleware.WriteAdminError(c, http.StatusNotFound, errcode.AccountUserNotFound)
	default:
		middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
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
