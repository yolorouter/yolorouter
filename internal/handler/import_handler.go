package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/pkg/response"
)

type importModelItemRequest struct {
	ProviderModelName string `json:"provider_model_name" binding:"required,max=200"`
	// The binding only guards transport shape here; an unknown id or an
	// over-long list must reach the service, which skips just that row —
	// the import's per-item best-effort contract. (The create endpoints are
	// the opposite: a bad list rejects the whole request there.)
	OutputModalities []string `json:"output_modalities" binding:"omitempty,max=32,dive,max=32"`
	InputPrice       float64  `json:"input_price" binding:"min=0"`
	OutputPrice      float64  `json:"output_price" binding:"min=0"`
	CacheWritePrice  *float64 `json:"cache_write_price" binding:"omitempty,min=0"`
	CacheReadPrice   *float64 `json:"cache_read_price" binding:"omitempty,min=0"`
	MaxOutput        int      `json:"max_output" binding:"min=0"`
}

// The cap only guards against pathological payloads. It must comfortably hold
// a full upstream catalog: the model-list fetch follows up to 20 pages with no
// per-page cap, and real aggregator catalogs run well past 500 ids.
type importProviderModelsRequest struct {
	Items []importModelItemRequest `json:"items" binding:"required,min=1,max=2000,dive"`
}

type suggestPricesRequest struct {
	Names []string `json:"names" binding:"required,min=1,max=2000,dive,required,max=200"`
}

// PostProviderModelsImport bulk-imports upstream models as model+candidate
// pairs for one provider. Imported mappings are stored disabled and untested,
// then handed to the probe queue, which verifies each one against the real
// upstream and auto-enables the ones that pass.
//
// queue may be nil only in tests that exercise the storage semantics alone;
// the server always wires the real queue.
func PostProviderModelsImport(svc *modeladmin.ModelService, queue *modeladmin.ProbeQueue) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req importProviderModelsRequest
		if !bindJSON(c, &req) {
			return
		}
		items := make([]modeladmin.ImportModelItem, 0, len(req.Items))
		for _, it := range req.Items {
			items = append(items, modeladmin.ImportModelItem{
				ProviderModelName: it.ProviderModelName,
				OutputModalities:  it.OutputModalities,
				InputPrice:        it.InputPrice, OutputPrice: it.OutputPrice,
				CacheWritePrice: it.CacheWritePrice, CacheReadPrice: it.CacheReadPrice,
				MaxOutput: it.MaxOutput,
			})
		}
		result, err := svc.ImportProviderModels(providerID, items, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		if queue != nil {
			ids := make([]uint, 0, len(result.Items))
			for _, item := range result.Items {
				if item.CandidateID != 0 {
					ids = append(ids, item.CandidateID)
				}
			}
			queue.Enqueue(ids...)
		}
		response.Success(c, result)
	}
}

// GetProviderCandidates lists every mapping a provider serves with its
// verification state — the poll target for import progress and the data source
// of the provider detail page's model tab. The consistency dance (queue-state
// stamping, torn-snapshot re-read) lives in the service.
func GetProviderCandidates(svc *modeladmin.ModelService, queue *modeladmin.ProbeQueue) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		list, err := svc.ListProviderCandidatesWithQueueStates(providerID, queue)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"list": list})
	}
}

// PostProviderSuggestPrices is the batch form of GetCandidateSuggestPrice: the
// import dialog prices every fetched row with one request instead of one per
// row. Every requested name gets an entry; an empty source means no match.
func PostProviderSuggestPrices(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providerID, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req suggestPricesRequest
		if !bindJSON(c, &req) {
			return
		}
		prices, err := svc.SuggestCandidatePrices(providerID, req.Names)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"prices": prices})
	}
}
