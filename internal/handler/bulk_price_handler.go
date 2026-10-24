package handler

import (
	"encoding/json"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/audit"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// bulkPriceRequest is the bulk-price body. candidate_ids binds on its RAW
// length (the same cap discipline as the import); the service deduplicates
// repeats, so the binding cap is a transport guard, not a per-row contract.
// operation carries the oneof so an unknown verb is a 400 at the door — a
// quietly ignored operation word would apply a price change nobody asked
// for. dry_run is a REQUIRED pointer: nil (field absent or null) is a 400,
// because a missing dry_run must never default into a write, and a plain
// bool binding would make that confusion the default. factor stays untagged:
// its rules are conditional on the operation, which no tag can express.
type bulkPriceRequest struct {
	CandidateIDs []uint   `json:"candidate_ids" binding:"required,min=1,max=2000"`
	Operation    string   `json:"operation" binding:"required,oneof=scale apply_suggested"`
	Factor       *float64 `json:"factor"`
	DryRun       *bool    `json:"dry_run" binding:"required"`
}

// PostModelsBulkPrice scales or suggestion-resets many candidate prices in
// one server-side transaction. The service computes every per-row outcome
// against the rows as they exist now (a dry run preview never becomes the
// apply's target snapshot), so the response always describes a batch that
// was just recomputed, not one replayed from the preview.
func PostModelsBulkPrice(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req bulkPriceRequest
		if !bindJSON(c, &req) {
			return
		}
		// The factor contract is operation-conditional: scale must carry a
		// positive factor (a zero or negative one would zero or invert
		// prices), apply_suggested must carry none — any non-nil value, zero
		// included, is a malformed request rather than a hint.
		switch req.Operation {
		case modeladmin.BulkPriceOperationScale:
			if req.Factor == nil {
				response.ParamError(c, "factor is required for the scale operation")
				return
			}
			if *req.Factor <= 0 {
				response.ParamError(c, "factor must be greater than zero")
				return
			}
		case modeladmin.BulkPriceOperationApplySuggested:
			if req.Factor != nil {
				response.ParamError(c, "apply_suggested must not carry a factor")
				return
			}
		}
		result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
			CandidateIDs: req.CandidateIDs,
			Operation:    req.Operation,
			Factor:       req.Factor,
			DryRun:       req.DryRun,
		}, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		audit.SetDetail(c, bulkPriceAuditDetail(req, result))
		response.Success(c, result)
	}
}

// bulkPriceAuditSampleRow is one changed row in the audit detail's sample:
// identity plus the full before/after write-slot record, so the trail
// answers "what did this batch do to this row" without re-deriving slots
// from a billing mode.
type bulkPriceAuditSampleRow struct {
	CandidateID  uint                      `json:"candidate_id"`
	ModelName    string                    `json:"model_name"`
	ProviderName string                    `json:"provider_name"`
	Before       modeladmin.BulkPriceSlots `json:"before"`
	After        modeladmin.BulkPriceSlots `json:"after"`
}

// bulkPriceAuditDetailDoc is the FIXED detail shape every bulk-price audit
// row carries: the operation and its dry-run flag, the raw requested count,
// the applied/skipped tally, and the first few changed rows verbatim. Fixed
// so tests and trail readers can assert it; bounded so a 2000-row batch
// cannot turn the detail column into blob storage.
type bulkPriceAuditDetailDoc struct {
	Operation string                    `json:"operation"`
	DryRun    bool                      `json:"dry_run"`
	Requested int                       `json:"requested"`
	Applied   int                       `json:"applied"`
	Skipped   int                       `json:"skipped"`
	Sample    []bulkPriceAuditSampleRow `json:"sample"`
}

// bulkPriceAuditDetail serializes the batch's audit document. A dry run
// publishes the same shape with dry_run=true and would-apply counts — the
// flag IS the preview marker; a preview is real console traffic and lands
// its row like any other write.
func bulkPriceAuditDetail(req bulkPriceRequest, result *modeladmin.BulkPriceResult) string {
	detail := bulkPriceAuditDetailDoc{
		Operation: req.Operation,
		DryRun:    req.DryRun != nil && *req.DryRun,
		Requested: len(req.CandidateIDs),
		Applied:   result.Applied,
		Skipped:   result.Skipped,
		Sample:    []bulkPriceAuditSampleRow{},
	}
	for _, item := range result.Items {
		if len(detail.Sample) == modeladmin.BulkPriceAuditSampleCap {
			break
		}
		if !item.Changed {
			continue
		}
		detail.Sample = append(detail.Sample, bulkPriceAuditSampleRow{
			CandidateID:  item.CandidateID,
			ModelName:    item.ModelName,
			ProviderName: item.ProviderName,
			Before:       item.Before,
			After:        item.After,
		})
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		// The detail is an enrichment of an already-captured row, never a
		// gate on it: a marshal failure (impossible for this shape) parks no
		// document rather than failing the request.
		return ""
	}
	return string(raw)
}
