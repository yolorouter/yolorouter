package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

type createProviderRequest struct {
	Name         string `json:"name" binding:"required,min=2,max=50"`
	BaseURL      string `json:"base_url" binding:"required,url,max=255"`
	Note         string `json:"note" binding:"max=200"`
	KeyLabel     string `json:"key_label" binding:"required,min=2,max=30"`
	KeyPlaintext string `json:"key_plaintext" binding:"required,min=8"`
	// TestModel is required: there is no real model mapping yet, so the
	// admin supplies a temporary model name every test call uses.
	TestModel        string `json:"test_model" binding:"required,max=100"`
	ManagementStatus int    `json:"management_status" binding:"omitempty,oneof=1 2"`
	// ProviderType and ProtocolEndpoints are deliberately unconstrained by
	// gin binding tags: the service layer validates them through
	// providerproto's write-path validators so a bad value surfaces as a
	// clean 400 through writeServiceError rather than a gin binding error.
	// Both are optional — omitting ProviderType normalizes to "openai" for
	// backward compatibility.
	ProviderType      string `json:"provider_type"`
	ProtocolEndpoints string `json:"protocol_endpoints"`
}

type updateProviderRequest struct {
	Name    string  `json:"name" binding:"required,min=2,max=50"`
	BaseURL string  `json:"base_url" binding:"required,url,max=255"`
	Note    *string `json:"note" binding:"omitempty,max=200"`
	// ProviderType and ProtocolEndpoints are deliberately unconstrained by
	// gin binding tags, same reasoning as createProviderRequest's: validated
	// in the service layer so a bad value surfaces as a clean 400. Both are
	// *string, not string, to distinguish "field absent from the JSON body"
	// (nil — PATCH semantics, leave unchanged) from "field present, possibly
	// an empty string" (non-nil — authoritative, applied as given): the edit
	// UI always sends protocol_endpoints and legitimately needs to send an
	// empty string to clear all extra endpoints, which a plain string field
	// couldn't distinguish from "not supplied".
	ProviderType      *string `json:"provider_type"`
	ProtocolEndpoints *string `json:"protocol_endpoints"`
}

type setStatusRequest struct {
	Enabled bool `json:"enabled"`
}

type testKeyRequest struct {
	BaseURL string `json:"base_url" binding:"required,url"`
	APIKey  string `json:"api_key" binding:"required"`
	Model   string `json:"model" binding:"required"`
	// ProviderType is optional and, like createProviderRequest's own field,
	// deliberately unconstrained by gin binding tags — an invalid value is
	// simply treated as unset (defaults to openai) rather than surfaced as
	// a binding error, since this is only a preview call before any
	// provider row exists to validate against. It lets an admin test an
	// anthropic (or other non-openai) key before creating the provider.
	ProviderType string `json:"provider_type"`
}

// listModelsRequest mirrors testKeyRequest minus the model: a catalogue
// fetch needs the destination and credential, not a specific model. Same
// preview semantics — no provider row exists yet, so provider_type is
// unconstrained and defaults to openai.
type listModelsRequest struct {
	BaseURL      string `json:"base_url" binding:"required,url"`
	APIKey       string `json:"api_key" binding:"required"`
	ProviderType string `json:"provider_type"`
}

type createKeyRequest struct {
	Label            string `json:"label" binding:"required,min=2,max=30"`
	Plaintext        string `json:"plaintext" binding:"required,min=8"`
	TestModel        string `json:"test_model" binding:"required,max=100"`
	ManagementStatus int    `json:"management_status" binding:"omitempty,oneof=1 2"`
}

type updateKeyRequest struct {
	Label            string  `json:"label" binding:"required,min=2,max=30"`
	Plaintext        *string `json:"plaintext" binding:"omitempty,min=8"`
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
// writing a 400 and returning ok=false on the first failure. PatchProviderKey,
// PatchProviderKeyOrder, PatchProviderKeyStatus, and PostProviderKeyTest each
// repeated this exact pair of parseUintParam calls.
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

func GetProviders(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		list, err := svc.ListProviders()
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"list": list})
	}
}

func GetProvider(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		detail, err := svc.GetProviderDetail(id)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, detail)
	}
}

func PostProvider(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createProviderRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.CreateProvider(c.Request.Context(), provider.CreateProviderInput{
			Name: req.Name, BaseURL: req.BaseURL, Note: req.Note,
			KeyLabel: req.KeyLabel, KeyPlaintext: req.KeyPlaintext, TestModel: req.TestModel,
			ManagementStatus:  req.ManagementStatus,
			ProviderType:      req.ProviderType,
			ProtocolEndpoints: req.ProtocolEndpoints,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchProvider(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req updateProviderRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.UpdateProvider(id, provider.UpdateProviderInput{
			Name: req.Name, BaseURL: req.BaseURL, Note: req.Note,
			ProviderType:      req.ProviderType,
			ProtocolEndpoints: req.ProtocolEndpoints,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchProviderStatus(svc *provider.ProviderService) gin.HandlerFunc {
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
			writeServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PostProviderTestKey(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req testKeyRequest
		if !bindJSON(c, &req) {
			return
		}
		result, err := svc.TestKeyPreview(c.Request.Context(), req.BaseURL, req.APIKey, req.Model, req.ProviderType)
		if err != nil {
			// Same fix as writeServiceError's default branch:
			// this call site was
			// missed, still leaking the raw client-call error (e.g. "too
			// many concurrent provider test calls in flight") verbatim.
			response.Error(c, errcode.ProviderTestFailed, errcode.GetMessage(errcode.ProviderTestFailed))
			return
		}
		response.Success(c, gin.H{"outcome": int(result.Outcome), "duration_ms": result.DurationMs, "detail": result.Detail})
	}
}

func PostProviderListModels(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req listModelsRequest
		if !bindJSON(c, &req) {
			return
		}
		result, err := svc.ListModelsPreview(c.Request.Context(), req.BaseURL, req.APIKey, req.ProviderType)
		if err != nil {
			// The client itself refused the call (e.g. concurrency cap) — not a
			// real outcome. Mirror PostProviderTestKey's generic mapping rather
			// than leak the raw client-call error.
			response.Error(c, errcode.ProviderTestFailed, errcode.GetMessage(errcode.ProviderTestFailed))
			return
		}
		respondModelCatalogue(c, result)
	}
}

// respondModelCatalogue writes the JSON body shared by both model-catalogue
// endpoints: a nil slice is normalized to [] so the body is always a list
// (the frontend iterates it unconditionally), and only models + outcome are
// surfaced — the picker shows the categorized outcome, not the per-fetch
// detail/duration a credential test carries.
func respondModelCatalogue(c *gin.Context, result providerclient.ListModelsResult) {
	models := result.Models
	if models == nil {
		models = []string{}
	}
	response.Success(c, gin.H{
		"models":  models,
		"outcome": int(result.Outcome),
	})
}

// GetProviderListModels fetches the model catalogue for an already-stored
// provider (GET .../providers/:id/models) using one of its server-side keys —
// the by-id counterpart to the stateless preview above. A non-success outcome
// (including "no usable key") comes back as 200 with an empty list, letting
// the picker fall back to manual entry.
func GetProviderListModels(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		result, err := svc.ListModelsForProvider(c.Request.Context(), providerID)
		if err != nil {
			if errors.Is(err, errcode.ErrProviderNotFound) {
				response.Error(c, errcode.ProviderNotFound, errcode.GetMessage(errcode.ProviderNotFound))
				return
			}
			response.Error(c, errcode.ProviderTestFailed, errcode.GetMessage(errcode.ProviderTestFailed))
			return
		}
		respondModelCatalogue(c, result)
	}
}

func PostProviderKey(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req createKeyRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.CreateProviderKey(c.Request.Context(), providerID, provider.CreateKeyInput{
			Label: req.Label, Plaintext: req.Plaintext, TestModel: req.TestModel, ManagementStatus: req.ManagementStatus,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchProviderKey(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, keyID, ok := parseProviderAndKeyIDs(c)
		if !ok {
			return
		}
		var req updateKeyRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.UpdateProviderKey(c.Request.Context(), providerID, keyID, provider.UpdateKeyInput{
			Label: req.Label, Plaintext: req.Plaintext, TestModel: req.TestModel, ManagementStatus: req.ManagementStatus,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchProviderKeyOrder(svc *provider.ProviderService) gin.HandlerFunc {
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
			writeServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PatchProviderKeyStatus(svc *provider.ProviderService) gin.HandlerFunc {
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
			writeServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PostProviderKeyTest(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, keyID, ok := parseProviderAndKeyIDs(c)
		if !ok {
			return
		}
		view, err := svc.TestProviderKey(c.Request.Context(), providerID, keyID, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PostProviderKeysTestAll(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		results, err := svc.TestAllProviderKeys(c.Request.Context(), providerID, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"results": results})
	}
}

// timeNow is the handler layer's single clock. It is a package variable so
// tests can pin it: every service call that involves a time window or a
// stamped mutation receives this value explicitly, and nothing below the
// handler reads the wall clock for query windowing.
var timeNow = func() time.Time { return time.Now().UTC() }

// GetProviderImpact returns the models that depend on the provider, for the
// disable confirm dialog.
func GetProviderImpact(svc *provider.ProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		view, err := svc.GetProviderImpact(id, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}
