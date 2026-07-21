// Package handler additions for M4: API Key management HTTP layer. See
// design doc .claude/docs/2026-07-19-m4-apikey-design.md §5.
package handler

import (
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/internal/service"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
	"github.com/yolorouter/yolorouter-ce/pkg/response"
)

// limitFields is the shared limit shape between create and update requests:
// nullable ints where NULL/nil = "no cap" and 0 is the service-layer sentinel
// for "clear this limit". binding min=0 lets the 0 sentinel through.
type limitFields struct {
	ExpiresAt         *time.Time `json:"expires_at" binding:"omitempty"`
	RPMLimit          *int       `json:"rpm_limit" binding:"omitempty,min=0"`
	TPMLimit          *int       `json:"tpm_limit" binding:"omitempty,min=0"`
	ConcurrencyLimit  *int       `json:"concurrency_limit" binding:"omitempty,min=0"`
	BudgetLimitMicros *int64     `json:"budget_limit_micros" binding:"omitempty,min=0"`
}

type createAPIKeyRequest struct {
	OwnerLabel string `json:"owner_label" binding:"omitempty,max=50"`
	Remark     string `json:"remark" binding:"omitempty,max=200"`
	// ModelIDs is required and must have at least one entry (PRD §6.4.3);
	// dive,required rejects a 0 id (models are 1-indexed autoincrement).
	ModelIDs []uint `json:"model_ids" binding:"required,min=1,dive,required"`
	limitFields
}

// updateAPIKeyRequest is a sparse PATCH: pointer fields distinguish "omitted"
// (nil) from "set to empty/zero". For the numeric limits, a non-nil 0 is the
// sentinel meaning "clear this limit" (no cap). ModelIDs is a non-pointer
// slice: nil/omitted means "leave the whitelist unchanged"; an explicit []
// (non-nil empty slice) means "clear the whitelist" (PRD §6.4.7). Go's
// encoding/json does distinguish these ([] -> non-nil empty, omitted -> nil),
// and service.UpdateAPIKey keys off `ModelIDs != nil` to tell them apart.
type updateAPIKeyRequest struct {
	OwnerLabel *string `json:"owner_label" binding:"omitempty,max=50"`
	Remark     *string `json:"remark" binding:"omitempty,max=200"`
	ModelIDs   []uint  `json:"model_ids" binding:"omitempty,dive,required"`
	limitFields
}

// parseAPIKeyPagination reads page / page_size query params (1-indexed page,
// 20 rows default, 200 cap). This is the first paginated list endpoint in the
// project — M2/M3 return full lists — so the helper lives here.
func parseAPIKeyPagination(c *gin.Context) (page, pageSize int) {
	page = 1
	pageSize = 20
	if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(c.Query("page_size")); err == nil && v > 0 && v <= 200 {
		pageSize = v
	}
	return page, pageSize
}

// validateExpiryFuture rejects an expiry that isn't strictly after now (PRD
// §6.4.3 requires the expiry to be later than the current time). nil expiry
// (no expiry) is allowed. Returns false (and writes a 400) on violation.
func validateExpiryFuture(c *gin.Context, expiry *time.Time) bool {
	if expiry != nil && !expiry.After(timeNow()) {
		response.ParamError(c, "expires_at must be in the future")
		return false
	}
	return true
}

func writeAPIKeyServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errcode.ErrAPIKeyNotFound):
		response.Error(c, errcode.APIKeyNotFound, errcode.GetMessage(errcode.APIKeyNotFound))
	case errors.Is(err, errcode.ErrModelNotFound):
		response.Error(c, errcode.ModelNotFound, errcode.GetMessage(errcode.ModelNotFound))
	default:
		response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
	}
}

func GetAPIKeys(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, pageSize := parseAPIKeyPagination(c)
		list, total, err := svc.ListAPIKeys(c.Query("q"), page, pageSize)
		if err != nil {
			writeAPIKeyServiceError(c, err)
			return
		}
		response.PageSuccess(c, total, page, pageSize, list)
	}
}

// PostAPIKey creates a key and returns the plaintext exactly once — the
// "plaintext_key" field is never derivable or re-shown afterwards (PRD §6.4
// KEY-01/04).
func PostAPIKey(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createAPIKeyRequest
		if !bindJSON(c, &req) {
			return
		}
		if !validateExpiryFuture(c, req.ExpiresAt) {
			return
		}
		result, err := svc.CreateAPIKey(service.CreateAPIKeyInput{
			OwnerLabel: req.OwnerLabel, Remark: req.Remark, ModelIDs: req.ModelIDs,
			ExpiresAt: req.ExpiresAt, RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit,
			ConcurrencyLimit: req.ConcurrencyLimit, BudgetLimitMicros: req.BudgetLimitMicros,
		}, timeNow())
		if err != nil {
			writeAPIKeyServiceError(c, err)
			return
		}
		response.Success(c, gin.H{
			"plaintext_key": result.PlaintextKey,
			"api_key":       result.APIKey,
		})
	}
}

func GetAPIKey(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		view, err := svc.GetAPIKey(id)
		if err != nil {
			writeAPIKeyServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchAPIKey(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req updateAPIKeyRequest
		if !bindJSON(c, &req) {
			return
		}
		if !validateExpiryFuture(c, req.ExpiresAt) {
			return
		}
		view, err := svc.UpdateAPIKey(id, service.UpdateAPIKeyInput{
			OwnerLabel: req.OwnerLabel, Remark: req.Remark, ModelIDs: req.ModelIDs,
			ExpiresAt: req.ExpiresAt, RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit,
			ConcurrencyLimit: req.ConcurrencyLimit, BudgetLimitMicros: req.BudgetLimitMicros,
		}, timeNow())
		if err != nil {
			writeAPIKeyServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchAPIKeyRevoke(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		if err := svc.RevokeAPIKey(id, timeNow()); err != nil {
			writeAPIKeyServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}
