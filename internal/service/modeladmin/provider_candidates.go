package modeladmin

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// ProviderCandidateView is one mapping as seen from its provider: which model
// it serves, what the upstream is asked for, prices, and where verification
// stands — including the persisted failure reason. It is the row shape both
// the import progress poll and the provider detail page's model tab render;
// ModelID travels along because the retest route is model-scoped.
type ProviderCandidateView struct {
	CandidateID       uint     `json:"candidate_id"`
	ModelID           uint     `json:"model_id"`
	ModelName         string   `json:"model_name"`
	ProviderModelName string   `json:"provider_model_name"`
	InputPrice        float64  `json:"input_price"`
	OutputPrice       float64  `json:"output_price"`
	CacheWritePrice   *float64 `json:"cache_write_price"`
	CacheReadPrice    *float64 `json:"cache_read_price"`
	// BillingMode says which quantity settles this mapping ("token" or
	// "image"); ImagePricingTiers is the per-image table for image mode,
	// nil when none is configured. A list column prices an image-billed
	// mapping off these — its per-M token prices are inert under that mode.
	BillingMode       string                   `json:"billing_mode"`
	ImagePricingTiers *model.ImagePricingTiers `json:"image_pricing_tiers"`
	// AudioUnitPrice is the per-million-characters price an audio-billed
	// mapping carries; nil when none is set.
	AudioUnitPrice     *float64   `json:"audio_unit_price"`
	MaxOutput          int        `json:"max_output"`
	ManagementStatus   int        `json:"management_status"`
	VerificationStatus int        `json:"verification_status"`
	LastTestResult     *int       `json:"last_test_result"`
	LastTestedAt       *time.Time `json:"last_tested_at"`
	LastTestError      *string    `json:"last_test_error"`
	// AutoEnableOnPass is the standing probe promise: true means the queue
	// (on some instance — not necessarily the one answering this request)
	// still owes this row a probe outcome, so pollers should keep watching
	// it. An untested unstamped row WITHOUT it was stored that way on
	// purpose and is not pending anything.
	AutoEnableOnPass bool `json:"auto_enable_on_pass"`
	// QueueState is where the probe queue currently holds this mapping —
	// QueueStateQueued / QueueStateProbing — or empty when the queue has
	// nothing for it. Stamped from the live queue by
	// ListProviderCandidatesWithQueueStates; it is what lets the progress
	// view tell "waiting its turn" apart from "being verified right now".
	QueueState string `json:"queue_state"`
}

// ListProviderCandidatesWithQueueStates is ListProviderCandidates plus a
// consistent live-queue stamp on every row. queue may be nil only in tests
// that exercise the storage semantics alone; the server always passes the
// real queue.
//
// A row that looks TORN gets one re-read before the result is returned: a
// worker can commit its verdict and leave the queue between the database read
// and the queue lookup, and the resulting pair — a stale row with no queue
// position — is exactly the shape pollers treat as settled and stop watching.
// (The database is read FIRST on purpose: the mirror-image race — a
// concurrent import enqueueing after a queue-first snapshot — would produce
// the same frozen pair, while with this order it yields a self-healing queued
// row.) Rows genuinely matching the untested-unstamped shape (orphaned, or
// deliberately unprobed) simply come back unchanged.
func (s *ModelService) ListProviderCandidatesWithQueueStates(providerID uint, queue *ProbeQueue) ([]ProviderCandidateView, error) {
	assemble := func() ([]ProviderCandidateView, error) {
		list, err := s.ListProviderCandidates(providerID)
		if err != nil {
			return nil, err
		}
		if queue != nil {
			ids := make([]uint, 0, len(list))
			for _, item := range list {
				ids = append(ids, item.CandidateID)
			}
			states := queue.CandidateQueueStates(ids)
			for i := range list {
				list[i].QueueState = states[list[i].CandidateID]
			}
		}
		return list, nil
	}
	// Taken before the database read: a row that leaves the queue during the
	// request is invisible to the post-read queue lookup, and this snapshot
	// is the only witness that its verdict may postdate the read.
	var preStates map[uint]string
	if queue != nil {
		preStates = queue.SnapshotStates()
	}
	list, err := assemble()
	if err != nil {
		return nil, err
	}
	for _, item := range list {
		if candidateRowLooksTorn(item, preStates[item.CandidateID] != "") {
			return assemble()
		}
	}
	return list, nil
}

// candidateRowLooksTorn reports whether a row's database snapshot may predate
// a verdict that landed during the request. Two shapes qualify: an ARMED
// untested row with no attempt stamp and no queue position (a probe is owed
// but nothing shows it in flight — produced when a worker commits and leaves
// the queue between the database read and the queue lookup), and a row
// showing no queue position now but held by the queue when the request began
// (its stale verdict would read as terminal and stop the pollers, hiding the
// rerun's fresh outcome). The armed leg matters: WITHOUT the promise, that
// same untested-unstamped shape is a lasting legal state (a manual
// save-as-disabled, a revoked promise) — treating it as torn would re-read
// the whole list on every poll for as long as such a row exists.
func candidateRowLooksTorn(item ProviderCandidateView, wasQueued bool) bool {
	if item.QueueState != "" {
		return false
	}
	if wasQueued {
		return true
	}
	return item.AutoEnableOnPass &&
		item.VerificationStatus == model.ModelVerificationStatusUntested && item.LastTestedAt == nil
}

// ListProviderCandidates returns every mapping the provider serves, in
// insertion order (which is also import order, keeping the progress view
// stable across polls). A provider with no mappings yields an empty list; an
// unknown provider id is an error, not an empty result.
func (s *ModelService) ListProviderCandidates(providerID uint) ([]ProviderCandidateView, error) {
	if _, err := repository.FindProviderByID(s.db, providerID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderNotFound
		}
		return nil, err
	}
	candidates, err := repository.ListModelCandidatesByProvider(s.db, providerID)
	if err != nil {
		return nil, err
	}
	modelIDs := make([]uint, 0, len(candidates))
	for _, c := range candidates {
		modelIDs = append(modelIDs, c.ModelID)
	}
	models, err := repository.ListModelsByIDs(s.db, modelIDs)
	if err != nil {
		return nil, err
	}
	namesByID := make(map[uint]string, len(models))
	for _, m := range models {
		namesByID[m.ID] = m.Name
	}

	list := make([]ProviderCandidateView, 0, len(candidates))
	for _, c := range candidates {
		list = append(list, ProviderCandidateView{
			CandidateID:        c.ID,
			ModelID:            c.ModelID,
			ModelName:          namesByID[c.ModelID],
			ProviderModelName:  c.ProviderModelName,
			InputPrice:         c.InputPrice,
			OutputPrice:        c.OutputPrice,
			CacheWritePrice:    c.CacheWritePrice,
			CacheReadPrice:     c.CacheReadPrice,
			BillingMode:        model.NormalizeBillingMode(c.BillingMode),
			ImagePricingTiers:  model.ParseImagePricingTiers(c.ImagePricingTiers),
			AudioUnitPrice:     c.AudioUnitPrice,
			MaxOutput:          c.MaxOutput,
			ManagementStatus:   c.ManagementStatus,
			VerificationStatus: c.VerificationStatus,
			LastTestResult:     c.LastTestResult,
			LastTestedAt:       c.LastTestedAt,
			LastTestError:      c.LastTestError,
			AutoEnableOnPass:   c.AutoEnableOnPass,
		})
	}
	return list, nil
}
