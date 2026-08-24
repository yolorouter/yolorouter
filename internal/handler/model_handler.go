package handler

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// scheduling_mode carries NO binding tag anywhere below: the service layer
// is its single validator, answering every invalid value with the field's
// own error code rather than the generic bad-request a binding tag would
// produce — and tags cannot reference the model package's constants, so a
// tag would also be a second, driftable copy of the vocabulary.
type createModelRequest struct {
	Name string `json:"name" binding:"required,max=100"`
	// SchedulingMode is optional; the empty value creates the model with
	// the failover default.
	SchedulingMode model.SchedulingMode `json:"scheduling_mode"`
}

type batchCreateModelsRequest struct {
	Names []string `json:"names" binding:"required,min=1,max=200,dive,max=100"`
	// SchedulingMode applies to every model the batch creates (empty = the
	// failover default), matching the single-create endpoint's contract.
	SchedulingMode model.SchedulingMode `json:"scheduling_mode"`
}

type updateModelRequest struct {
	Name string `json:"name" binding:"required,max=100"`
	// ImageInput is the tri-state image-input declaration as a string enum —
	// "yes" / "no" / "unknown" — so "set back to undeclared" (unknown) stays
	// distinguishable from "field absent, don't touch" (nil), which a *bool
	// cannot express through JSON.
	ImageInput *string `json:"image_input" binding:"omitempty,oneof=yes no unknown"`
	// SchedulingMode follows the same nil-means-leave-alone convention as
	// ImageInput: present switches the model's scheduler, absent keeps it. A
	// present empty string is invalid — the service rejects it rather than
	// reading it as the default, which would let a submission that carries
	// nothing silently reset the scheduler.
	SchedulingMode *model.SchedulingMode `json:"scheduling_mode"`
}

type setModelStatusRequest struct {
	Enabled bool `json:"enabled"`
}

type createCandidateRequest struct {
	ProviderID        uint     `json:"provider_id" binding:"required"`
	ProviderModelName string   `json:"provider_model_name" binding:"max=200"`
	InputPrice        float64  `json:"input_price" binding:"min=0"`
	OutputPrice       float64  `json:"output_price" binding:"min=0"`
	CacheWritePrice   *float64 `json:"cache_write_price" binding:"omitempty,min=0"`
	CacheReadPrice    *float64 `json:"cache_read_price" binding:"omitempty,min=0"`
	MaxOutput         int      `json:"max_output" binding:"min=0"`
	ManagementStatus  int      `json:"management_status" binding:"omitempty,oneof=1 2"`
}

type updateCandidateRequest struct {
	ProviderModelName string   `json:"provider_model_name" binding:"max=200"`
	InputPrice        float64  `json:"input_price" binding:"min=0"`
	OutputPrice       float64  `json:"output_price" binding:"min=0"`
	CacheWritePrice   *float64 `json:"cache_write_price" binding:"omitempty,min=0"`
	CacheReadPrice    *float64 `json:"cache_read_price" binding:"omitempty,min=0"`
	MaxOutput         int      `json:"max_output" binding:"min=0"`
	// A pointer so an omitted field stays distinguishable from a request to
	// disable. As a plain int, any client PATCHing only prices would send the
	// zero value and silently take the candidate out of routing.
	ManagementStatus *int `json:"management_status" binding:"omitempty,oneof=1 2"`
}

type candidateReorderRequest struct {
	Direction string `json:"direction" binding:"required,oneof=up down"`
}

// parseModelAndCandidateIDs parses both the ":id" and ":candidateId" path
// segments, writing a 400 and returning ok=false on the first failure —
// mirrors provider_handler.go's parseProviderAndKeyIDs.
func parseModelAndCandidateIDs(c *gin.Context) (modelID, candidateID uint, ok bool) {
	modelID, ok = parseUintParam(c, "id")
	if !ok {
		return 0, 0, false
	}
	candidateID, ok = parseUintParam(c, "candidateId")
	if !ok {
		return 0, 0, false
	}
	return modelID, candidateID, true
}

func GetModels(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		list, err := svc.ListModels()
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"list": list})
	}
}

func PostModel(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createModelRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: req.Name, SchedulingMode: req.SchedulingMode}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

// PostModelsBatch creates several models at once from a list of names,
// skipping (rather than failing on) names that are invalid or already exist —
// the response carries the created models plus a per-name skip summary.
func PostModelsBatch(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req batchCreateModelsRequest
		if !bindJSON(c, &req) {
			return
		}
		result, err := svc.CreateModelsBatch(modeladmin.CreateModelsBatchInput{Names: req.Names, SchedulingMode: req.SchedulingMode}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, result)
	}
}

func GetModel(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		detail, err := svc.GetModelDetail(id)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, detail)
	}
}

func PatchModel(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req updateModelRequest
		if !bindJSON(c, &req) {
			return
		}
		// Both fields go through ONE service call (one UPDATE statement), so
		// a rejected rename persists nothing and concurrent PATCHes cannot
		// interleave name and declaration into a row neither admin submitted.
		var imageInput *bool
		if req.ImageInput != nil {
			switch *req.ImageInput {
			case "yes":
				t := true
				imageInput = &t
			case "no":
				f := false
				imageInput = &f
			}
		}
		view, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name:           req.Name,
			ImageInputSet:  req.ImageInput != nil,
			ImageInput:     imageInput,
			SchedulingMode: req.SchedulingMode,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchModelStatus(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req setModelStatusRequest
		if !bindJSON(c, &req) {
			return
		}
		if err := svc.SetModelStatus(id, req.Enabled, timeNow()); err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

// GetCandidateSuggestPrice returns a price to pre-fill when adding a candidate
// for a provider+model, checking the provider's own history first then the
// built-in seed catalog. provider_id and provider_model_name come from query
// params because this is a look-up the candidate form fires on model-name
// select, not a candidate-scoped path. An empty Source means nothing matched
// and the form stays at its default.
func GetCandidateSuggestPrice(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, ok := requireUintQuery(c, "provider_id")
		if !ok {
			return
		}
		modelName := strings.TrimSpace(c.Query("provider_model_name"))
		if modelName == "" {
			response.ParamError(c, "provider_model_name is required")
			return
		}
		view, err := svc.SuggestCandidatePrice(providerID, modelName)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PostModelCandidate(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		modelID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req createCandidateRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.CreateModelCandidate(c.Request.Context(), modelID, modeladmin.CreateCandidateInput{
			ProviderID: req.ProviderID, ProviderModelName: req.ProviderModelName,
			InputPrice: req.InputPrice, OutputPrice: req.OutputPrice,
			CacheWritePrice: req.CacheWritePrice, CacheReadPrice: req.CacheReadPrice,
			MaxOutput: req.MaxOutput, ManagementStatus: req.ManagementStatus,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchModelCandidate(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, candidateID, ok := parseModelAndCandidateIDs(c)
		if !ok {
			return
		}
		var req updateCandidateRequest
		if !bindJSON(c, &req) {
			return
		}
		result, err := svc.UpdateModelCandidate(c.Request.Context(), candidateID, modeladmin.UpdateCandidateInput{
			ProviderModelName: req.ProviderModelName, InputPrice: req.InputPrice, OutputPrice: req.OutputPrice,
			CacheWritePrice: req.CacheWritePrice, CacheReadPrice: req.CacheReadPrice, MaxOutput: req.MaxOutput,
			ManagementStatus: req.ManagementStatus,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, result)
	}
}

func PatchModelCandidateOrder(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		modelID, candidateID, ok := parseModelAndCandidateIDs(c)
		if !ok {
			return
		}
		var req candidateReorderRequest
		if !bindJSON(c, &req) {
			return
		}
		if err := svc.ReorderModelCandidate(modelID, candidateID, req.Direction); err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PatchModelCandidateStatus(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, candidateID, ok := parseModelAndCandidateIDs(c)
		if !ok {
			return
		}
		var req setModelStatusRequest
		if !bindJSON(c, &req) {
			return
		}
		if err := svc.SetCandidateStatus(candidateID, req.Enabled, timeNow()); err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

// PostModelCandidateTest re-probes a stored candidate. It takes no test_type:
// one retest covers the basic mapping and both capabilities, so an operator
// never has to pick which probe to run.
func PostModelCandidateTest(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, candidateID, ok := parseModelAndCandidateIDs(c)
		if !ok {
			return
		}
		view, applied, err := svc.RetestModelCandidate(c.Request.Context(), candidateID, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		// applied=false means a concurrent probe or edit won the commit race and
		// the view reflects the competitor's result — the client needs the flag
		// to avoid announcing another probe's outcome as this retest's.
		//
		// The embedded view keeps the candidate's fields at the TOP level of
		// data alongside the nested form: this endpoint used to return the
		// bare candidate, and browser tabs loaded before a server upgrade
		// still read it that way — for them a shape-only change would turn
		// every successful retest into a warning.
		response.Success(c, struct {
			*modeladmin.CandidateView
			Candidate *modeladmin.CandidateView `json:"candidate"`
			Applied   bool                      `json:"applied"`
		}{CandidateView: view, Candidate: view, Applied: applied})
	}
}

// PostModelCandidateTestAndCreate probes a mapping and stores it only if the
// result allows what the caller asked for, so the admin UI can report the
// verdicts without a manual test step and without leaving a broken mapping
// behind when enablement was requested but the mapping does not work.
func PostModelCandidateTestAndCreate(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		modelID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req createCandidateRequest
		if !bindJSON(c, &req) {
			return
		}
		result, err := svc.TestAndCreateCandidate(c.Request.Context(), modelID, modeladmin.CreateCandidateInput{
			ProviderID: req.ProviderID, ProviderModelName: req.ProviderModelName,
			InputPrice: req.InputPrice, OutputPrice: req.OutputPrice,
			CacheWritePrice: req.CacheWritePrice, CacheReadPrice: req.CacheReadPrice,
			MaxOutput: req.MaxOutput, ManagementStatus: req.ManagementStatus,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, result)
	}
}

func DeleteModelCandidate(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, candidateID, ok := parseModelAndCandidateIDs(c)
		if !ok {
			return
		}
		if err := svc.DeleteModelCandidate(candidateID); err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

// GetModelImpact returns what disabling or renaming the model touches, for
// the confirm dialogs and the impact tab.
func GetModelImpact(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		view, err := svc.GetModelImpact(id, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}
