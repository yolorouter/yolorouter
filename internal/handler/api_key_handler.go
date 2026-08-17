// Package handler provides the API Key management HTTP layer.
package handler

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/service"
	"github.com/yolorouter/yolorouter/pkg/response"
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
	Remark string `json:"remark" binding:"omitempty,max=200"`
	// AllowAllModels lets the key call any enabled model. When true the
	// allowlist is bypassed and model_ids may be empty; when false (the wire
	// default) model_ids must name at least one model — enforced in the service
	// (gin's required_without only checks a slice is non-nil, so an explicit
	// empty [] would slip past a binding tag).
	AllowAllModels bool `json:"allow_all_models"`
	// ModelIDs: dive,required rejects a 0 id (models are 1-indexed
	// autoincrement); the not-empty-when-custom rule is enforced in the service.
	ModelIDs []uint `json:"model_ids" binding:"omitempty,dive,required"`
	// CSP per-key override at create time (value types — false/"" is the
	// wire default meaning "inherit the global setting").
	CustomSystemPromptEnabledOverride bool   `json:"custom_system_prompt_enabled_override"`
	CustomSystemPromptEnabled         bool   `json:"custom_system_prompt_enabled"`
	CustomSystemPrompt                string `json:"custom_system_prompt"`
	// Input-compression per-key override at create time (value types — false
	// is the wire default meaning "inherit the global setting").
	CompressEnabledOverride bool `json:"compress_enabled_override"`
	CompressEnabled         bool `json:"compress_enabled"`
	limitFields
}

// updateAPIKeyRequest is a sparse PATCH: pointer fields distinguish "omitted"
// (nil) from "set to empty/zero". For the numeric limits, a non-nil 0 is the
// sentinel meaning "clear this limit" (no cap). ModelIDs is a non-pointer
// slice: nil/omitted means "leave the whitelist unchanged"; an explicit []
// (non-nil empty slice) means "clear the whitelist". Go's
// encoding/json does distinguish these ([] -> non-nil empty, omitted -> nil),
// and service.UpdateAPIKey keys off `ModelIDs != nil` to tell them apart.
// ExpectedUpdatedAt carries the caller's CAS snapshot (from a prior GET) —
// when present the service/repo pair qualifies the UPDATE with it and a
// mismatch returns 11013 (409). Omitted on legacy callers (EditKeyModal etc.).
type updateAPIKeyRequest struct {
	Remark *string `json:"remark" binding:"omitempty,max=200"`
	// AllowAllModels is a pointer so an omitted field leaves the flag unchanged;
	// switching it on lets the caller send an empty model_ids to clear the
	// now-unused allowlist.
	AllowAllModels *bool  `json:"allow_all_models"`
	ModelIDs       []uint `json:"model_ids" binding:"omitempty,dive,required"`
	// CSP per-key override PATCH fields (pointer — nil means leave unchanged).
	CustomSystemPromptEnabledOverride *bool   `json:"custom_system_prompt_enabled_override"`
	CustomSystemPromptEnabled         *bool   `json:"custom_system_prompt_enabled"`
	CustomSystemPrompt                *string `json:"custom_system_prompt"`
	// Input-compression per-key override PATCH fields (pointer — nil means
	// leave unchanged). When override is set to true, enabled must also be
	// supplied; when set to false, enabled is ignored.
	CompressEnabledOverride *bool `json:"compress_enabled_override"`
	CompressEnabled         *bool `json:"compress_enabled"`
	// ExpectedUpdatedAt is the optimistic-lock CAS token (RFC3339). Optional —
	// omitted means "no CAS" (the legacy behavior for EditKeyModal/CreateKeyModal).
	ExpectedUpdatedAt *time.Time `json:"expected_updated_at" binding:"omitempty"`
	limitFields
}

// parseAPIKeyPagination reads page / page_size query params (1-indexed page,
// 20 rows default, 200 cap). This is the first paginated list endpoint in the
// project — the provider/model lists return full lists — so the helper lives here.
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

// validateExpiryFuture rejects an expiry that isn't strictly after now (the
// expiry must be later than the current time). nil expiry
// (no expiry) is allowed. Returns false (and writes a 400) on violation.
func validateExpiryFuture(c *gin.Context, expiry *time.Time) bool {
	if expiry != nil && !expiry.After(timeNow()) {
		response.ParamError(c, "expires_at must be in the future")
		return false
	}
	return true
}

// validAPIKeyStatusFilters is the allowlist for the ?status= filter: empty
// (no filter) plus the four display statuses computeAPIKeyDisplayStatus can
// return. An unknown value is rejected with a 400 rather than silently
// ignored (which would return the unfiltered list) — mirroring the request-log
// handler's status-class allowlist.
var validAPIKeyStatusFilters = map[string]struct{}{
	"":                             {},
	service.APIKeyDisplayActive:    {},
	service.APIKeyDisplayExpired:   {},
	service.APIKeyDisplayRevoked:   {},
	service.APIKeyDisplayBudgetHit: {},
}

func GetAPIKeys(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, pageSize := parseAPIKeyPagination(c)
		status := c.Query("status")
		if _, ok := validAPIKeyStatusFilters[status]; !ok {
			response.ParamError(c, "status must be one of: active, expired, revoked, budget_exhausted")
			return
		}
		var filterUserID *uint
		if !applyUintQueryParam(c, "user_id", func(v uint) { filterUserID = &v }) {
			return
		}
		// A member's list is pinned to their own keys no matter what the
		// query says; admins keep the optional filter.
		userID := middleware.ViewScopeOf(c).Resolve(filterUserID)
		list, total, err := svc.ListAPIKeys(c.Query("q"), status, userID, page, pageSize)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.PageSuccess(c, total, page, pageSize, list)
	}
}

// PostAPIKey creates a key and returns the plaintext once at create time. The
// same plaintext is also persisted as AES-GCM ciphertext, so it remains
// recoverable via GET /api-keys/:id/plaintext (the list-page reveal).
func PostAPIKey(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createAPIKeyRequest
		if !bindJSON(c, &req) {
			return
		}
		if !validateExpiryFuture(c, req.ExpiresAt) {
			return
		}
		if middleware.ViewScopeOf(c).Member && !memberCreateAllowed(c, &req) {
			return
		}
		result, err := svc.CreateAPIKey(service.CreateAPIKeyInput{
			// The session user owns the key they create. RequireSession has
			// already resolved the identity or this handler is unreachable.
			UserID:         c.MustGet(middleware.UserIDKey).(uint),
			Remark:         req.Remark,
			AllowAllModels: req.AllowAllModels, ModelIDs: req.ModelIDs,
			ExpiresAt: req.ExpiresAt, RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit,
			ConcurrencyLimit: req.ConcurrencyLimit, BudgetLimitMicros: req.BudgetLimitMicros,
			CustomSystemPromptEnabledOverride: req.CustomSystemPromptEnabledOverride,
			CustomSystemPromptEnabled:         req.CustomSystemPromptEnabled,
			CustomSystemPrompt:                req.CustomSystemPrompt,
			CompressEnabledOverride:           req.CompressEnabledOverride,
			CompressEnabled:                   req.CompressEnabled,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		denyStorage(c)
		response.Success(c, gin.H{
			"plaintext_key": result.PlaintextKey,
			"api_key":       result.APIKey,
		})
	}
}

// denyStorage tells every cache between here and the browser not to keep this
// response.
//
// Both responses that carry a plaintext key go through it. Without it the
// credential is an ordinary body that a browser's disk cache, or a shared proxy
// keying on the URL, is free to retain — surviving the logout that was supposed
// to end the session, and reachable by whoever gets the machine next. The
// reveal endpoint is a GET, which is the shape caches are most willing to
// store, so it is the one that needs this most; the create response gets it too
// because the reason has nothing to do with the method.
//
// no-store rather than no-cache: no-cache permits storing the response and only
// requires revalidation before reuse, which still leaves the key on disk.
func denyStorage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
}

func GetAPIKey(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		view, err := svc.GetAPIKey(id, middleware.ViewScopeOf(c).Owner)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

// GetAPIKeyPlaintext reveals the full plaintext key for the list-page copy
// button. The plaintext_key field name matches PostAPIKey's create response so
// the frontend reuses one code path. Returns 11016 when the key predates the
// encrypted_key column and cannot be recovered.
func GetAPIKeyPlaintext(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		plaintext, err := svc.GetAPIKeyPlaintext(id, middleware.ViewScopeOf(c).Owner)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		denyStorage(c)
		response.Success(c, gin.H{"plaintext_key": plaintext})
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
		scope := middleware.ViewScopeOf(c)
		if scope.Member && !memberPatchAllowed(c, &req) {
			return
		}
		view, err := svc.UpdateAPIKey(id, service.UpdateAPIKeyInput{
			Remark:         req.Remark,
			AllowAllModels: req.AllowAllModels, ModelIDs: req.ModelIDs,
			ExpiresAt: req.ExpiresAt, RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit,
			ConcurrencyLimit: req.ConcurrencyLimit, BudgetLimitMicros: req.BudgetLimitMicros,
			CustomSystemPromptEnabledOverride: req.CustomSystemPromptEnabledOverride,
			CustomSystemPromptEnabled:         req.CustomSystemPromptEnabled,
			CustomSystemPrompt:                req.CustomSystemPrompt,
			CompressEnabledOverride:           req.CompressEnabledOverride,
			CompressEnabled:                   req.CompressEnabled,
			ExpectedUpdatedAt:                 req.ExpectedUpdatedAt,
		}, scope.Owner, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

// The member field boundary, in one place: a member may set remark and
// expires_at — nothing else. Model scope, limits, and the per-key
// overrides are admin-only knobs; a member's key always carries the
// all-models scope. memberCreateAllowed and memberPatchAllowed below are
// the two shape-specific readings of THIS rule (the create request spells
// the knobs as value fields, the patch request as pointers), and the 400
// text comes from memberFieldRuleMsg. Adding a restricted knob means one
// rule edit here plus one condition in each reading — the route-level
// member tests hold the behavior to this rule.
const memberFieldRuleMsg = "members may only set remark and expires_at"

// memberCreateAllowed is the create-side reading of the member field
// boundary above: the check is that the member did not try to send a
// custom scope or any restricted knob — the caller forces
// AllowAllModels=true afterwards, so the wire default's allowlist demand
// never applies to them. Writes a 400 and returns false on violation.
func memberCreateAllowed(c *gin.Context, req *createAPIKeyRequest) bool {
	restricted := len(req.ModelIDs) > 0 ||
		req.RPMLimit != nil || req.TPMLimit != nil || req.ConcurrencyLimit != nil ||
		req.BudgetLimitMicros != nil ||
		req.CustomSystemPromptEnabledOverride || req.CustomSystemPromptEnabled ||
		req.CustomSystemPrompt != "" ||
		req.CompressEnabledOverride || req.CompressEnabled
	if restricted {
		response.ParamError(c, memberFieldRuleMsg)
		return false
	}
	// The wire default AllowAllModels=false would demand an allowlist —
	// members always get the all-models scope instead.
	req.AllowAllModels = true
	return true
}

// memberPatchAllowed is the edit-side reading of the member field
// boundary above: a member may rename (remark) and re-schedule expiry on
// their own keys. Writes a 400 and returns false when a restricted field
// is present.
func memberPatchAllowed(c *gin.Context, req *updateAPIKeyRequest) bool {
	restricted := req.AllowAllModels != nil || len(req.ModelIDs) > 0 ||
		req.RPMLimit != nil || req.TPMLimit != nil || req.ConcurrencyLimit != nil ||
		req.BudgetLimitMicros != nil ||
		req.CustomSystemPromptEnabledOverride != nil || req.CustomSystemPromptEnabled != nil ||
		req.CustomSystemPrompt != nil ||
		req.CompressEnabledOverride != nil || req.CompressEnabled != nil
	if restricted {
		response.ParamError(c, memberFieldRuleMsg)
		return false
	}
	return true
}

func PatchAPIKeyRevoke(svc *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		if err := svc.RevokeAPIKey(id, middleware.ViewScopeOf(c).Owner, timeNow()); err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}
