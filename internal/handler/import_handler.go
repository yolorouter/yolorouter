package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/pkg/response"
)

type importModelItemRequest struct {
	ProviderModelName string `json:"provider_model_name" binding:"required,max=200"`
	// The bindings below only guard transport shape here; an unknown id, an
	// over-long list, a negative price or any other out-of-domain value must
	// reach the service, which skips just that row — the import's per-item
	// best-effort contract. (The create endpoints are the opposite: a bad
	// list rejects the whole request there.)
	OutputModalities []string `json:"output_modalities" binding:"omitempty,max=32,dive,max=32"`
	// ModelName is the row's optional target model name — the name the model
	// is found or created under when the row declares one, instead of the
	// upstream name. The binding caps the transport shape; the service
	// applies the same name rules as the create path and skips the row when
	// they fail.
	ModelName *string `json:"model_name" binding:"omitempty,max=200"`
	// InputModalities declares what the model accepts as input. It follows
	// the output list's shape; the vocabulary is the service's to validate.
	InputModalities *[]string `json:"input_modalities" binding:"omitempty,max=32,dive,max=32"`
	// ContextLength / SupportsTools / SupportsReasoning / Temperature carry
	// the model-level capability declarations. None carries a binding tag:
	// every value domain (positive context, the yes/no vocabulary, the
	// temperature range) is row-level service validation, so a bad value
	// skips its own row instead of failing the batch.
	ContextLength     *int     `json:"context_length"`
	SupportsTools     *string  `json:"supports_tools"`
	SupportsReasoning *string  `json:"supports_reasoning"`
	Temperature       *float64 `json:"temperature"`
	// The five price slots and MaxOutput are pointers or untagged on purpose:
	// nil means the row omits the field, and a negative value is a per-row
	// skip the service rules on, never a whole-batch rejection.
	InputPrice      *float64 `json:"input_price"`
	OutputPrice     *float64 `json:"output_price"`
	CacheWritePrice *float64 `json:"cache_write_price"`
	CacheReadPrice  *float64 `json:"cache_read_price"`
	// AudioUnitPrice is the per-million-characters price an audio-only row
	// carries in place of the token slots.
	AudioUnitPrice *float64 `json:"audio_unit_price"`
	MaxOutput      int      `json:"max_output"`
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
			var inputModalities []string
			if it.InputModalities != nil {
				inputModalities = *it.InputModalities
			}
			items = append(items, modeladmin.ImportModelItem{
				ProviderModelName: it.ProviderModelName,
				ModelName:         it.ModelName,
				OutputModalities:  it.OutputModalities,
				InputModalities:   inputModalities,
				InputPrice:        it.InputPrice, OutputPrice: it.OutputPrice,
				CacheWritePrice: it.CacheWritePrice, CacheReadPrice: it.CacheReadPrice,
				AudioUnitPrice:    it.AudioUnitPrice,
				MaxOutput:         it.MaxOutput,
				ContextLength:     it.ContextLength,
				SupportsTools:     it.SupportsTools,
				SupportsReasoning: it.SupportsReasoning,
				Temperature:       it.Temperature,
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
