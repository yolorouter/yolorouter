package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// customSystemPromptResponse is the handler-facing response DTO with explicit
// json tags. The neutral settings.CustomSystemPromptSetting DTO intentionally
// has no json tags; response.Success serializes whatever it is given, so this
// wrapper fixes the wire field names (enabled/text/version).
type customSystemPromptResponse struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
	Version int64  `json:"version"`
}

// putCustomSystemPromptRequest uses pointers so absent fields can be detected
// and rejected. A partial body must not silently clear the prompt.
type putCustomSystemPromptRequest struct {
	Enabled *bool   `json:"enabled"`
	Text    *string `json:"text"`
	Version *int64  `json:"version"`
}

// inputCompressionResponse is the handler-facing response DTO with explicit
// json tags. There is no neutral DTO to carry (the service returns a bare
// bool + version), so this wrapper fixes the wire field names (enabled/version).
type inputCompressionResponse struct {
	Enabled bool  `json:"enabled"`
	Version int64 `json:"version"`
}

// putInputCompressionRequest mirrors putCustomSystemPromptRequest: pointers
// make absent fields distinguishable from zero values, so a partial body
// cannot silently flip the switch off.
type putInputCompressionRequest struct {
	Enabled *bool  `json:"enabled"`
	Version *int64 `json:"version"`
}

// GetCustomSystemPrompt returns the authoritative global state (DB read,
// bypassing the cache) so the admin always sees the committed value.
func GetCustomSystemPrompt(svc *systemsettings.SystemSettingsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		s, ver, err := svc.GetCustomSystemPrompt(c.Request.Context())
		if err != nil {
			response.InternalError(c, err.Error())
			return
		}
		response.Success(c, customSystemPromptResponse{Enabled: s.Enabled, Text: s.Text, Version: ver})
	}
}

// PutCustomSystemPrompt validates + CAS-updates the global state. version is
// required (optimistic lock); enabled/text must both be present (pointers) so
// a partial body can't silently clear the prompt. A CAS miss returns 409.
func PutCustomSystemPrompt(svc *systemsettings.SystemSettingsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req putCustomSystemPromptRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.ParamError(c, err.Error())
			return
		}
		if req.Enabled == nil || req.Text == nil || req.Version == nil || *req.Version < 1 {
			response.ParamError(c, "enabled, text and version (>=1) are all required")
			return
		}
		s, ver, err := svc.UpdateCustomSystemPrompt(c.Request.Context(), *req.Version, *req.Enabled, *req.Text)
		if err != nil {
			switch {
			case errors.Is(err, errcode.ErrCustomSystemPromptConflict):
				// 409 is not produced by httpStatusForCode's range mapping; set it explicitly.
				response.ErrorStatus(c, http.StatusConflict, errcode.CustomSystemPromptConflict, errcode.GetMessage(errcode.CustomSystemPromptConflict))
			case errors.Is(err, errcode.ErrCustomSystemPromptTooLong):
				response.Error(c, errcode.CustomSystemPromptTooLong, errcode.GetMessage(errcode.CustomSystemPromptTooLong))
			case errors.Is(err, errcode.ErrCustomSystemPromptEmpty):
				response.Error(c, errcode.CustomSystemPromptEmpty, errcode.GetMessage(errcode.CustomSystemPromptEmpty))
			default:
				response.InternalError(c, err.Error())
			}
			return
		}
		response.Success(c, customSystemPromptResponse{Enabled: s.Enabled, Text: s.Text, Version: ver})
	}
}

// GetInputCompression returns the authoritative global switch state (DB read,
// bypassing the cache) so the admin always sees the committed value.
func GetInputCompression(svc *systemsettings.SystemSettingsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		enabled, ver, err := svc.GetInputCompressionForHandler(c.Request.Context())
		if err != nil {
			response.InternalError(c, err.Error())
			return
		}
		response.Success(c, inputCompressionResponse{Enabled: enabled, Version: ver})
	}
}

// PutInputCompression validates + CAS-updates the global switch. version is
// required (optimistic lock); enabled must be present (pointer) so a partial
// body can't silently flip the switch. A CAS miss returns 409 with the
// InputCompressionConflict code (11014), distinct from the CSP conflict code
// (11012) so the frontend can route retries to the right setting.
func PutInputCompression(svc *systemsettings.SystemSettingsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req putInputCompressionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.ParamError(c, err.Error())
			return
		}
		if req.Enabled == nil || req.Version == nil || *req.Version < 1 {
			response.ParamError(c, "enabled and version (>=1) are both required")
			return
		}
		enabled, ver, err := svc.UpdateInputCompression(c.Request.Context(), *req.Version, *req.Enabled)
		if err != nil {
			if errors.Is(err, errcode.ErrInputCompressionConflict) {
				// 409 is not produced by httpStatusForCode's range mapping; set it explicitly.
				response.ErrorStatus(c, http.StatusConflict, errcode.InputCompressionConflict, errcode.GetMessage(errcode.InputCompressionConflict))
				return
			}
			response.InternalError(c, err.Error())
			return
		}
		response.Success(c, inputCompressionResponse{Enabled: enabled, Version: ver})
	}
}

type visionFallbackResponse struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Version int64  `json:"version"`
}

// putVisionFallbackRequest: pointers make absent fields distinguishable from
// zero values, so a partial body cannot silently clear the model (= disable
// the feature) or the prompt.
type putVisionFallbackRequest struct {
	Model   *string `json:"model"`
	Prompt  *string `json:"prompt"`
	Version *int64  `json:"version"`
}

// GetVisionFallback returns the authoritative global state (DB read,
// bypassing the cache) so the admin always sees the committed value.
func GetVisionFallback(svc *systemsettings.SystemSettingsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		s, ver, err := svc.GetVisionFallbackForHandler(c.Request.Context())
		if err != nil {
			response.InternalError(c, err.Error())
			return
		}
		response.Success(c, visionFallbackResponse{Model: s.Model, Prompt: s.Prompt, Version: ver})
	}
}

// PutVisionFallback validates + CAS-updates the pair. version is required
// (optimistic lock); model and prompt must both be present (pointers). A CAS
// miss returns 409 with its own code so the frontend can route retries; an
// unknown model name is a 400-class validation error.
func PutVisionFallback(svc *systemsettings.SystemSettingsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req putVisionFallbackRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.ParamError(c, err.Error())
			return
		}
		if req.Model == nil || req.Prompt == nil || req.Version == nil || *req.Version < 1 {
			response.ParamError(c, "model, prompt and version (>=1) are all required")
			return
		}
		s, ver, err := svc.UpdateVisionFallback(c.Request.Context(), *req.Version, strings.TrimSpace(*req.Model), *req.Prompt)
		if err != nil {
			switch {
			case errors.Is(err, errcode.ErrVisionFallbackConflict):
				// 409 is not produced by httpStatusForCode's range mapping; set it explicitly.
				response.ErrorStatus(c, http.StatusConflict, errcode.VisionFallbackConflict, errcode.GetMessage(errcode.VisionFallbackConflict))
			case errors.Is(err, errcode.ErrVisionFallbackModelUnknown):
				response.Error(c, errcode.VisionFallbackModelUnknown, errcode.GetMessage(errcode.VisionFallbackModelUnknown))
			default:
				response.InternalError(c, err.Error())
			}
			return
		}
		response.Success(c, visionFallbackResponse{Model: s.Model, Prompt: s.Prompt, Version: ver})
	}
}
