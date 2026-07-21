package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/service"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

type createProviderRequest struct {
	Name         string `json:"name" binding:"required,min=2,max=50"`
	BaseURL      string `json:"base_url" binding:"required,url,max=255"`
	Note         string `json:"note" binding:"max=200"`
	KeyLabel     string `json:"key_label" binding:"required,min=2,max=30"`
	KeyPlaintext string `json:"key_plaintext" binding:"required,min=20"`
	// TestModel is required: M2 has no real model mapping yet, so the
	// admin supplies a temporary model name every test call uses (PRD §6.2.8).
	TestModel        string `json:"test_model" binding:"required,max=100"`
	ManagementStatus int    `json:"management_status" binding:"omitempty,oneof=1 2"`
}

type updateProviderRequest struct {
	Name    string `json:"name" binding:"required,min=2,max=50"`
	BaseURL string `json:"base_url" binding:"required,url,max=255"`
	Note    string `json:"note" binding:"max=200"`
}

type setStatusRequest struct {
	Enabled bool `json:"enabled"`
}

type testKeyRequest struct {
	BaseURL string `json:"base_url" binding:"required,url"`
	APIKey  string `json:"api_key" binding:"required"`
	Model   string `json:"model" binding:"required"`
}

type createKeyRequest struct {
	Label            string `json:"label" binding:"required,min=2,max=30"`
	Plaintext        string `json:"plaintext" binding:"required,min=20"`
	TestModel        string `json:"test_model" binding:"required,max=100"`
	ManagementStatus int    `json:"management_status" binding:"omitempty,oneof=1 2"`
}

type updateKeyRequest struct {
	Label            string  `json:"label" binding:"required,min=2,max=30"`
	Plaintext        *string `json:"plaintext" binding:"omitempty,min=20"`
	TestModel        string  `json:"test_model" binding:"required,max=100"`
	ManagementStatus *int    `json:"management_status" binding:"omitempty,oneof=1 2"`
}

type reorderRequest struct {
	Direction string `json:"direction" binding:"required,oneof=up down"`
}

// parseUintParam parses a numeric path parameter (":id"/":keyId"), writing a
// 400 and returning ok=false on failure. Uses response.ErrorStatus with an
// explicit http.StatusBadRequest rather than response.Error(c,
// errcode.InvalidParam, ...) even though pkg/response.httpStatusForCode now
// special-cases InvalidParam to 400 — this was the second independent call
// site to hit that exact bug (bindJSON in auth_handler.go hit it first,
// which is why the fix now lives in httpStatusForCode itself instead of
// being a per-caller workaround); being explicit here costs nothing and
// doesn't depend on that fix staying in place.
func parseUintParam(c *gin.Context, name string) (uint, bool) {
	raw := c.Param(name)
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		response.ErrorStatus(c, http.StatusBadRequest, errcode.InvalidParam, errcode.GetMessage(errcode.InvalidParam))
		return 0, false
	}
	return uint(v), true
}

// parseProviderAndKeyIDs parses both the ":id" and ":keyId" path segments,
// writing a 400 and returning ok=false on the first failure — a /simplify
// simplification-review finding: PatchProviderKey, PatchProviderKeyOrder,
// PatchProviderKeyStatus, and PostProviderKeyTest each repeated this exact
// pair of parseUintParam calls.
func parseProviderAndKeyIDs(c *gin.Context) (providerID, keyID uint, ok bool) {
	providerID, ok = parseUintParam(c, "id")
	if !ok {
		return 0, 0, false
	}
	keyID, ok = parseUintParam(c, "keyId")
	if !ok {
		return 0, 0, false
	}
	return providerID, keyID, true
}

// writeProviderServiceError maps a service-layer sentinel error to the
// project's unified error envelope — mirrors auth_handler.go's convention
// of a single dispatch table rather than repeating this switch at every
// call site.
func writeProviderServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errcode.ErrProviderNotFound):
		response.Error(c, errcode.ProviderNotFound, errcode.GetMessage(errcode.ProviderNotFound))
	case errors.Is(err, errcode.ErrProviderNameTaken):
		response.Error(c, errcode.ProviderNameTaken, errcode.GetMessage(errcode.ProviderNameTaken))
	case errors.Is(err, errcode.ErrProviderKeyNotFound):
		response.Error(c, errcode.ProviderKeyNotFound, errcode.GetMessage(errcode.ProviderKeyNotFound))
	case errors.Is(err, errcode.ErrProviderKeyLabelTaken):
		response.Error(c, errcode.ProviderKeyLabelTaken, errcode.GetMessage(errcode.ProviderKeyLabelTaken))
	case errors.Is(err, errcode.ErrProviderKeyNotVerified):
		response.Error(c, errcode.ProviderKeyNotVerified, errcode.GetMessage(errcode.ProviderKeyNotVerified))
	case errors.Is(err, errcode.ErrProviderKeyNeedsReentry):
		response.Error(c, errcode.ProviderKeyNeedsReentry, errcode.GetMessage(errcode.ProviderKeyNeedsReentry))
	case errors.Is(err, errcode.ErrProviderKeyTooShort):
		// validatePlaintextLength (provider_service.go) wraps this sentinel —
		// Gin's own binding tags currently make that check unreachable via
		// HTTP, but without this case a future change (a looser binding tag,
		// a new non-HTTP caller) would silently fall to the default 500
		// branch instead of the intended 400.
		response.Error(c, errcode.ProviderKeyTooShort, errcode.GetMessage(errcode.ProviderKeyTooShort))
	default:
		// A max-effort code-review round flagged the previous err.Error()
		// here as leaking internal details (wrapped crypto/gorm error text)
		// to the client — every other handler (see auth_handler.go) always
		// substitutes a fixed generic message for unmatched errors instead.
		response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
	}
}

func GetProviders(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		list, err := svc.ListProviders()
		if err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"list": list})
	}
}

func GetProvider(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		detail, err := svc.GetProviderDetail(id)
		if err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, detail)
	}
}

func PostProvider(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createProviderRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.CreateProvider(c.Request.Context(), service.CreateProviderInput{
			Name: req.Name, BaseURL: req.BaseURL, Note: req.Note,
			KeyLabel: req.KeyLabel, KeyPlaintext: req.KeyPlaintext, TestModel: req.TestModel,
			ManagementStatus: req.ManagementStatus,
		}, timeNow())
		if err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchProvider(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req updateProviderRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.UpdateProvider(id, service.UpdateProviderInput{Name: req.Name, BaseURL: req.BaseURL, Note: req.Note}, timeNow())
		if err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchProviderStatus(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req setStatusRequest
		if !bindJSON(c, &req) {
			return
		}
		if err := svc.SetProviderStatus(id, req.Enabled, timeNow()); err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PostProviderTestKey(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req testKeyRequest
		if !bindJSON(c, &req) {
			return
		}
		result, err := svc.TestKeyPreview(c.Request.Context(), req.BaseURL, req.APIKey, req.Model)
		if err != nil {
			// Same fix as writeProviderServiceError's default branch (round
			// 2): a max-effort code-review round found this call site was
			// missed, still leaking the raw client-call error (e.g. "too
			// many concurrent provider test calls in flight") verbatim.
			response.Error(c, errcode.ProviderTestFailed, errcode.GetMessage(errcode.ProviderTestFailed))
			return
		}
		response.Success(c, gin.H{"outcome": int(result.Outcome), "duration_ms": result.DurationMs})
	}
}

func PostProviderKey(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req createKeyRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.CreateProviderKey(c.Request.Context(), providerID, service.CreateKeyInput{
			Label: req.Label, Plaintext: req.Plaintext, TestModel: req.TestModel, ManagementStatus: req.ManagementStatus,
		}, timeNow())
		if err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchProviderKey(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, keyID, ok := parseProviderAndKeyIDs(c)
		if !ok {
			return
		}
		var req updateKeyRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.UpdateProviderKey(c.Request.Context(), providerID, keyID, service.UpdateKeyInput{
			Label: req.Label, Plaintext: req.Plaintext, TestModel: req.TestModel, ManagementStatus: req.ManagementStatus,
		}, timeNow())
		if err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchProviderKeyOrder(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, keyID, ok := parseProviderAndKeyIDs(c)
		if !ok {
			return
		}
		var req reorderRequest
		if !bindJSON(c, &req) {
			return
		}
		if err := svc.ReorderProviderKey(providerID, keyID, req.Direction, timeNow()); err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PatchProviderKeyStatus(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, keyID, ok := parseProviderAndKeyIDs(c)
		if !ok {
			return
		}
		var req setStatusRequest
		if !bindJSON(c, &req) {
			return
		}
		if err := svc.SetProviderKeyStatus(providerID, keyID, req.Enabled, timeNow()); err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PostProviderKeyTest(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, keyID, ok := parseProviderAndKeyIDs(c)
		if !ok {
			return
		}
		view, err := svc.TestProviderKey(c.Request.Context(), providerID, keyID, timeNow())
		if err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PostProviderKeysTestAll(svc *service.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		results, err := svc.TestAllProviderKeys(c.Request.Context(), providerID, timeNow())
		if err != nil {
			writeProviderServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"results": results})
	}
}

func timeNow() time.Time { return time.Now().UTC() }
