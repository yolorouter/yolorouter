package handler

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/internal/service"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
	"github.com/yolorouter/yolorouter-ce/pkg/response"
)

type createModelRequest struct {
	Name string `json:"name" binding:"required,max=100"`
}

type updateModelRequest struct {
	Name string `json:"name" binding:"required,max=100"`
}

type setModelStatusRequest struct {
	Enabled bool `json:"enabled"`
}

type testMappingRequest struct {
	ProviderID        uint   `json:"provider_id" binding:"required"`
	ProviderModelName string `json:"provider_model_name" binding:"max=200"`
	TestType          string `json:"test_type" binding:"required,oneof=basic streaming function_calling"`
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
}

type candidateReorderRequest struct {
	Direction string `json:"direction" binding:"required,oneof=up down"`
}

type candidateTestRequest struct {
	TestType string `json:"test_type" binding:"required,oneof=basic streaming function_calling"`
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

// writeModelServiceError maps a service-layer sentinel error to the
// project's unified error envelope — mirrors provider_handler.go's
// writeProviderServiceError.
func writeModelServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errcode.ErrModelNotFound):
		response.Error(c, errcode.ModelNotFound, errcode.GetMessage(errcode.ModelNotFound))
	case errors.Is(err, errcode.ErrModelNameTaken):
		response.Error(c, errcode.ModelNameTaken, errcode.GetMessage(errcode.ModelNameTaken))
	case errors.Is(err, errcode.ErrModelCandidateNotFound):
		response.Error(c, errcode.ModelCandidateNotFound, errcode.GetMessage(errcode.ModelCandidateNotFound))
	case errors.Is(err, errcode.ErrModelCandidateProviderTaken):
		response.Error(c, errcode.ModelCandidateProviderTaken, errcode.GetMessage(errcode.ModelCandidateProviderTaken))
	case errors.Is(err, errcode.ErrModelCandidateNotVerified):
		response.Error(c, errcode.ModelCandidateNotVerified, errcode.GetMessage(errcode.ModelCandidateNotVerified))
	case errors.Is(err, errcode.ErrProviderNotFound):
		response.Error(c, errcode.ProviderNotFound, errcode.GetMessage(errcode.ProviderNotFound))
	case errors.Is(err, errcode.ErrProviderNoTestableModel):
		response.Error(c, errcode.ProviderNoTestableModel, errcode.GetMessage(errcode.ProviderNoTestableModel))
	default:
		response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
	}
}

func GetModels(svc *service.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		list, err := svc.ListModels()
		if err != nil {
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"list": list})
	}
}

func PostModel(svc *service.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createModelRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.CreateModel(service.CreateModelInput{Name: req.Name}, timeNow())
		if err != nil {
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func GetModel(svc *service.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		detail, err := svc.GetModelDetail(id)
		if err != nil {
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, detail)
	}
}

func PatchModel(svc *service.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req updateModelRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.UpdateModelNameStatus(id, req.Name, timeNow())
		if err != nil {
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchModelStatus(svc *service.ModelService) gin.HandlerFunc {
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
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PostModelCandidateTestMapping(svc *service.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		modelID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req testMappingRequest
		if !bindJSON(c, &req) {
			return
		}
		result, err := svc.TestCandidateMappingPreview(c.Request.Context(), modelID, req.ProviderID, req.ProviderModelName, req.TestType)
		if err != nil {
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"outcome": int(result.Outcome), "duration_ms": result.DurationMs})
	}
}

func PostModelCandidate(svc *service.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		modelID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req createCandidateRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.CreateModelCandidate(c.Request.Context(), modelID, service.CreateCandidateInput{
			ProviderID: req.ProviderID, ProviderModelName: req.ProviderModelName,
			InputPrice: req.InputPrice, OutputPrice: req.OutputPrice,
			CacheWritePrice: req.CacheWritePrice, CacheReadPrice: req.CacheReadPrice,
			MaxOutput: req.MaxOutput, ManagementStatus: req.ManagementStatus,
		}, timeNow())
		if err != nil {
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchModelCandidate(svc *service.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, candidateID, ok := parseModelAndCandidateIDs(c)
		if !ok {
			return
		}
		var req updateCandidateRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.UpdateModelCandidate(candidateID, service.UpdateCandidateInput{
			ProviderModelName: req.ProviderModelName, InputPrice: req.InputPrice, OutputPrice: req.OutputPrice,
			CacheWritePrice: req.CacheWritePrice, CacheReadPrice: req.CacheReadPrice, MaxOutput: req.MaxOutput,
		}, timeNow())
		if err != nil {
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func PatchModelCandidateOrder(svc *service.ModelService) gin.HandlerFunc {
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
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PatchModelCandidateStatus(svc *service.ModelService) gin.HandlerFunc {
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
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}

func PostModelCandidateTest(svc *service.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, candidateID, ok := parseModelAndCandidateIDs(c)
		if !ok {
			return
		}
		var req candidateTestRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.TestModelCandidate(c.Request.Context(), candidateID, req.TestType, timeNow())
		if err != nil {
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, view)
	}
}

func DeleteModelCandidate(svc *service.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, candidateID, ok := parseModelAndCandidateIDs(c)
		if !ok {
			return
		}
		if err := svc.DeleteModelCandidate(candidateID); err != nil {
			writeModelServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}
