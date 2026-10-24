package modeladmin

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/pricecatalog"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// ErrBulkPriceInvalid rejects a bulk-price request whose contract is broken:
// an id list outside the bounds, a missing dry_run, an unknown operation, a
// scale request without a positive factor, or an apply_suggested request
// that carries one. The HTTP handler pre-validates the same rules so console
// callers get the 400 parameter envelope from the binding path; this
// sentinel is the direct-caller defense, which the handler's service-error
// table maps onto the same envelope.
var ErrBulkPriceInvalid = errors.New("invalid bulk price request")

// The two bulk operations. Anything else is rejected, not defaulted: a
// quietly ignored operation word would apply (or skip) a price change the
// admin did not ask for.
const (
	BulkPriceOperationScale          = "scale"
	BulkPriceOperationApplySuggested = "apply_suggested"
)

// Why a row in a bulk-price batch skipped. Each value names a different
// follow-up: the id does not exist, the row has no price to scale, its
// billing mode has no scalar write slots, no suggestion matched it, or the
// operation computed exactly the stored prices.
const (
	BulkPriceSkipNotFound               = "not_found"
	BulkPriceSkipUnpriced               = "unpriced"
	BulkPriceSkipUnsupportedBillingMode = "unsupported_billing_mode"
	BulkPriceSkipNoSuggestion           = "no_suggestion"
	BulkPriceSkipUnchanged              = "unchanged"
)

// BulkPriceMaxCandidates mirrors the request binding's raw-length cap: the
// console selects from one page of the pricing view, and the cap only guards
// against pathological payloads (same rationale as the import's 2000).
const BulkPriceMaxCandidates = 2000

// BulkPriceAuditSampleCap bounds the audit detail's sample: the first few
// changed rows are enough to identify a batch in the trail; every row's
// outcome is already in the response.
const BulkPriceAuditSampleCap = 5

// BulkPriceSlots is one row's write-slot snapshot, per its billing mode: a
// token row carries the four token prices (the char slot null — not a token
// write slot), an audio row carries only the character price (the token
// slots zero/null — inert under that mode). Before/after pairs therefore
// always compare like with like.
type BulkPriceSlots struct {
	InputPrice      float64  `json:"input_price"`
	OutputPrice     float64  `json:"output_price"`
	CacheWritePrice *float64 `json:"cache_write_price"`
	CacheReadPrice  *float64 `json:"cache_read_price"`
	AudioUnitPrice  *float64 `json:"audio_unit_price"`
}

// BulkPriceItem is one row's outcome. Before/After hold the row's write
// slots as they were and as the operation would leave them; Changed says a
// write slot actually moved; SkipReason names why nothing happened (empty
// when it did).
type BulkPriceItem struct {
	CandidateID  uint           `json:"candidate_id"`
	ModelName    string         `json:"model_name"`
	ProviderName string         `json:"provider_name"`
	Before       BulkPriceSlots `json:"before"`
	After        BulkPriceSlots `json:"after"`
	Changed      bool           `json:"changed"`
	SkipReason   string         `json:"skip_reason,omitempty"`
}

// BulkPriceResult is the batch outcome: one item per distinct requested id
// (repeats are deduplicated server-side, so one row is never scaled twice),
// with applied/skipped as the would-apply/would-skip split under a preview
// and the written/skipped split under an apply.
type BulkPriceResult struct {
	Items   []BulkPriceItem `json:"items"`
	Applied int             `json:"applied"`
	Skipped int             `json:"skipped"`
}

// BulkPriceInput is one bulk-price request. DryRun is a pointer because a
// missing field and an explicit false are different intents — omitting it
// must never mean "write", and a plain bool would make exactly that
// confusion the default. Factor follows the same discipline: nil means the
// field was not sent, which is the apply_suggested contract, while any
// non-nil value under apply_suggested is a rejection.
type BulkPriceInput struct {
	CandidateIDs []uint
	Operation    string
	Factor       *float64
	DryRun       *bool
}

func validateBulkPriceInput(in BulkPriceInput) error {
	if len(in.CandidateIDs) == 0 || len(in.CandidateIDs) > BulkPriceMaxCandidates {
		return fmt.Errorf("%w: candidate_ids must hold 1 to %d ids", ErrBulkPriceInvalid, BulkPriceMaxCandidates)
	}
	if in.DryRun == nil {
		return fmt.Errorf("%w: dry_run is required (true previews, false applies)", ErrBulkPriceInvalid)
	}
	switch in.Operation {
	case BulkPriceOperationScale:
		if in.Factor == nil {
			return fmt.Errorf("%w: scale requires a factor", ErrBulkPriceInvalid)
		}
		if *in.Factor <= 0 {
			return fmt.Errorf("%w: scale factor must be greater than zero", ErrBulkPriceInvalid)
		}
	case BulkPriceOperationApplySuggested:
		if in.Factor != nil {
			return fmt.Errorf("%w: apply_suggested must not carry a factor", ErrBulkPriceInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown operation %q", ErrBulkPriceInvalid, in.Operation)
	}
	return nil
}

// BulkPriceCandidates adjusts many candidate prices in one shot: scale
// multiplies each row's own write slots by a factor, apply_suggested
// overwrites them from the suggestion sources (the provider's price history,
// then the built-in catalog). A dry run computes the whole per-row outcome
// and writes nothing; an apply runs the same computation against the rows as
// they exist inside ONE transaction, so the batch lands whole or not at all
// and a price changed between a preview and its apply is what the operation
// actually sees.
//
// Rows the operation has nothing to say about (unknown ids, unpriced rows
// under scale, image/video tier rows, rows without any valid suggestion,
// rows whose computed prices equal the stored ones) are skipped with a
// reason rather than failing the batch — the same best-effort-per-row
// contract the import applies, because one bad row must not bury the
// operator's intent for the rest.
func (s *ModelService) BulkPriceCandidates(in BulkPriceInput, now time.Time) (*BulkPriceResult, error) {
	if err := validateBulkPriceInput(in); err != nil {
		return nil, err
	}
	if *in.DryRun {
		entries, err := s.bulkPricePlan(s.db, in)
		if err != nil {
			return nil, err
		}
		return tallyBulkPriceEntries(entries), nil
	}
	var result *BulkPriceResult
	txErr := s.db.Transaction(func(tx *gorm.DB) error {
		entries, err := s.bulkPricePlan(tx, in)
		if err != nil {
			return err
		}
		for i := range entries {
			w := entries[i].write
			if w == nil {
				continue
			}
			if w.mode == model.BillingModeAudio {
				err = repository.UpdateModelCandidateAudioPrice(tx, w.candidateID, w.audioPrice, w.priceChanged, w.pricesDeclared, now)
			} else {
				err = repository.UpdateModelCandidateTokenPrices(tx, w.candidateID, w.inputPrice, w.outputPrice,
					w.cacheWrite, w.cacheRead, w.priceChanged, w.pricesDeclared, now)
			}
			if err != nil {
				return err
			}
		}
		result = tallyBulkPriceEntries(entries)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return result, nil
}

// bulkPriceWrite is the one-row write an applied entry issues: the new slot
// values of the row's billing mode, whether they really moved (the price
// clock only follows a real change), and the declaration fact the service
// re-derived for the written slots.
type bulkPriceWrite struct {
	candidateID    uint
	mode           string
	inputPrice     float64
	outputPrice    float64
	cacheWrite     *float64
	cacheRead      *float64
	audioPrice     *float64
	priceChanged   bool
	pricesDeclared bool
}

// bulkPricePlanEntry pairs one result row with the write it produced — nil
// for every skipped row, which is also the applied/skipped tally's key.
type bulkPricePlanEntry struct {
	item  BulkPriceItem
	write *bulkPriceWrite
}

// bulkPricePlan computes the whole batch outcome against db — the live
// database for a preview, the open transaction for an apply, which is what
// makes the apply act on current rows rather than a snapshot. Repeated ids
// are deduplicated first-occurrence-wins so one row is never applied twice.
func (s *ModelService) bulkPricePlan(db *gorm.DB, in BulkPriceInput) ([]bulkPricePlanEntry, error) {
	seen := make(map[uint]bool, len(in.CandidateIDs))
	ids := make([]uint, 0, len(in.CandidateIDs))
	for _, id := range in.CandidateIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	candidates, err := repository.ListModelCandidatesByIDs(db, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[uint]model.ModelCandidate, len(candidates))
	for _, c := range candidates {
		byID[c.ID] = c
	}
	modelIDs := make([]uint, 0, len(candidates))
	modelSeen := make(map[uint]bool, len(candidates))
	for _, c := range candidates {
		if !modelSeen[c.ModelID] {
			modelSeen[c.ModelID] = true
			modelIDs = append(modelIDs, c.ModelID)
		}
	}
	modelRows, err := repository.ListModelsByIDs(db, modelIDs)
	if err != nil {
		return nil, err
	}
	modelNames := make(map[uint]string, len(modelRows))
	for i := range modelRows {
		modelNames[modelRows[i].ID] = modelRows[i].Name
	}

	// apply_suggested resolves its suggestions per provider against the same
	// db handle as the plan, so the apply resolves them inside its own
	// transaction too.
	suggestions := map[uint]bulkSuggestion{}
	if in.Operation == BulkPriceOperationApplySuggested {
		byProvider := map[uint][]model.ModelCandidate{}
		for _, c := range candidates {
			mode := model.NormalizeBillingMode(c.BillingMode)
			if mode == model.BillingModeToken || mode == model.BillingModeAudio {
				byProvider[c.ProviderID] = append(byProvider[c.ProviderID], c)
			}
		}
		for _, rows := range byProvider {
			provider := rows[0].Provider
			if provider == nil {
				// No provider row to read a base_url from: suggestion
				// resolution cannot run, so those rows simply stay unresolved
				// (no_suggestion) rather than failing the batch.
				continue
			}
			resolved, err := resolveBulkSuggestions(db, provider, rows)
			if err != nil {
				return nil, err
			}
			for id, sug := range resolved {
				suggestions[id] = sug
			}
		}
	}

	entries := make([]bulkPricePlanEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, s.bulkPriceEntry(in, id, byID[id], byID[id].Provider, modelNames[byID[id].ModelID], suggestions))
	}
	return entries, nil
}

// bulkPriceEntry computes one row's outcome: its before/after write slots,
// whether anything changed, and the write to issue — or the reason nothing
// did.
func (s *ModelService) bulkPriceEntry(
	in BulkPriceInput, id uint, c model.ModelCandidate, provider *model.Provider, modelName string,
	suggestions map[uint]bulkSuggestion,
) bulkPricePlanEntry {
	entry := bulkPricePlanEntry{item: BulkPriceItem{CandidateID: id, ModelName: modelName}}
	if provider != nil {
		entry.item.ProviderName = provider.Name
	}
	if c.ID == 0 {
		entry.item.SkipReason = BulkPriceSkipNotFound
		return entry
	}
	mode := model.NormalizeBillingMode(c.BillingMode)
	switch mode {
	case model.BillingModeToken:
		entry.item.Before = BulkPriceSlots{
			InputPrice: c.InputPrice, OutputPrice: c.OutputPrice,
			CacheWritePrice: c.CacheWritePrice, CacheReadPrice: c.CacheReadPrice,
		}
	case model.BillingModeAudio:
		entry.item.Before = BulkPriceSlots{AudioUnitPrice: c.AudioUnitPrice}
	default:
		// Image/video tier rows have no scalar write slots: neither operation
		// touches them, and the reason names the mode, not a missing
		// suggestion (they never reach the no_suggestion endpoint).
		entry.item.SkipReason = BulkPriceSkipUnsupportedBillingMode
		return entry
	}

	var after BulkPriceSlots
	switch in.Operation {
	case BulkPriceOperationScale:
		switch mode {
		case model.BillingModeToken:
			// Scale has no meaning for a row that holds no price: every slot
			// would stay 0/null and the outcome would be a write that changes
			// nothing, announced as success.
			if !c.PricesDeclared {
				entry.item.SkipReason = BulkPriceSkipUnpriced
				return entry
			}
			after = scaleTokenSlots(entry.item.Before, *in.Factor)
		case model.BillingModeAudio:
			if !c.PricesDeclared || c.AudioUnitPrice == nil {
				entry.item.SkipReason = BulkPriceSkipUnpriced
				return entry
			}
			scaled := *c.AudioUnitPrice * *in.Factor
			after = BulkPriceSlots{AudioUnitPrice: &scaled}
		}
	case BulkPriceOperationApplySuggested:
		sug, found := suggestions[id]
		if !found {
			entry.item.SkipReason = BulkPriceSkipNoSuggestion
			return entry
		}
		switch mode {
		case model.BillingModeToken:
			after = BulkPriceSlots{InputPrice: sug.input, OutputPrice: sug.output,
				CacheWritePrice: sug.cacheWrite, CacheReadPrice: sug.cacheRead}
		case model.BillingModeAudio:
			after = BulkPriceSlots{AudioUnitPrice: sug.audio}
		}
	}

	entry.item.After = after
	entry.item.Changed = bulkSlotsChanged(mode, entry.item.Before, after)
	if !entry.item.Changed {
		entry.item.SkipReason = BulkPriceSkipUnchanged
		return entry
	}

	// The declaration fact re-derives from the slots the write carries, by
	// the same per-mode rule every writer shares. Both operations write a
	// row's COMPLETE slot set for its mode (both token slots, or the char
	// slot), so presentness is unconditional for an applied row — an
	// apply_suggested fill is exactly what flips an unpriced row to declared.
	var declared bool
	if mode == model.BillingModeAudio {
		declared = pricesDeclaredForBilling(model.BillingModeAudio, false, false, after.AudioUnitPrice)
	} else {
		declared = pricesDeclaredForBilling(model.BillingModeToken, true, true, nil)
	}
	entry.write = &bulkPriceWrite{
		candidateID:    id,
		mode:           mode,
		inputPrice:     after.InputPrice,
		outputPrice:    after.OutputPrice,
		cacheWrite:     after.CacheWritePrice,
		cacheRead:      after.CacheReadPrice,
		audioPrice:     after.AudioUnitPrice,
		priceChanged:   entry.item.Changed,
		pricesDeclared: declared,
	}
	return entry
}

// scaleTokenSlots multiplies the four token slots by the factor. Null cache
// slots stay null — "this row has no such price" is a fact, not a zero.
func scaleTokenSlots(before BulkPriceSlots, factor float64) BulkPriceSlots {
	after := BulkPriceSlots{InputPrice: before.InputPrice * factor, OutputPrice: before.OutputPrice * factor}
	if before.CacheWritePrice != nil {
		v := *before.CacheWritePrice * factor
		after.CacheWritePrice = &v
	}
	if before.CacheReadPrice != nil {
		v := *before.CacheReadPrice * factor
		after.CacheReadPrice = &v
	}
	return after
}

// bulkSlotsChanged applies the full-write-slot criterion for a billing mode:
// a token row changes when any of its four token prices moved (a cache-only
// move counts), an audio row when its character price moved.
func bulkSlotsChanged(mode string, before, after BulkPriceSlots) bool {
	if mode == model.BillingModeAudio {
		return !sameOptionalPrice(before.AudioUnitPrice, after.AudioUnitPrice)
	}
	return before.InputPrice != after.InputPrice ||
		before.OutputPrice != after.OutputPrice ||
		!sameOptionalPrice(before.CacheWritePrice, after.CacheWritePrice) ||
		!sameOptionalPrice(before.CacheReadPrice, after.CacheReadPrice)
}

// tallyBulkPriceEntries folds the plan into the response: one item per
// distinct id, in request order, with the applied/skipped split read off the
// presence of a write.
func tallyBulkPriceEntries(entries []bulkPricePlanEntry) *BulkPriceResult {
	result := &BulkPriceResult{Items: make([]BulkPriceItem, 0, len(entries))}
	for _, e := range entries {
		result.Items = append(result.Items, e.item)
		if e.write != nil {
			result.Applied++
		} else {
			result.Skipped++
		}
	}
	return result
}

// bulkSuggestion is one row's resolved apply_suggested target: the slot
// values the row would be overwritten with, already restricted to the row's
// billing mode. found=false means neither source matched (no_suggestion).
type bulkSuggestion struct {
	found      bool
	input      float64
	output     float64
	cacheWrite *float64
	cacheRead  *float64
	audio      *float64
}

// resolveBulkSuggestions resolves the apply_suggested target for every row
// of one provider, from the two sources the single-candidate suggester
// reads — the provider's own price history first, then the built-in catalog
// — but under the bulk criterion, which is deliberately stricter than the
// form-prefill look-up:
//
//   - the row's own stored price is NOT history for itself (a row must never
//     suggest from itself, or "reset to suggestion" would be a no-op for
//     every already-priced row and an unpriced row would 0-price-hit itself);
//   - a history hit must bill in the SAME mode as the target (a token row is
//     never priced from an audio row or vice versa);
//   - its primary slots must not all be zero (an all-zero row is unpriced
//     history, not a price to copy).
//
// A history row passing all three wins wholesale: its cache prices ride
// along, nulls included — the row takes over the source's full record, the
// same cache-full-record semantics the single-row PATCH applies. Otherwise
// the catalog answers for the row's mode alone (token prices for token rows,
// the audio price for audio rows); a catalog miss leaves the row without a
// suggestion.
func resolveBulkSuggestions(db *gorm.DB, provider *model.Provider, targets []model.ModelCandidate) (map[uint]bulkSuggestion, error) {
	foldedSeen := map[string]bool{}
	foldedNames := make([]string, 0, len(targets))
	for _, t := range targets {
		f := model.FoldModelName(t.ProviderModelName)
		if f == "" || foldedSeen[f] {
			continue
		}
		foldedSeen[f] = true
		foldedNames = append(foldedNames, f)
	}
	history, err := repository.FindCandidatePriceHistoryByFoldedNames(db, provider.ID, foldedNames)
	if err != nil {
		return nil, err
	}
	historyByFolded := map[string][]model.ModelCandidate{}
	for _, h := range history {
		// The query selects the stored folded copy, not the raw name — key on
		// exactly what was queried by.
		historyByFolded[h.ProviderModelNameFolded] = append(historyByFolded[h.ProviderModelNameFolded], h)
	}

	out := make(map[uint]bulkSuggestion, len(targets))
	for _, t := range targets {
		mode := model.NormalizeBillingMode(t.BillingMode)
		folded := model.FoldModelName(t.ProviderModelName)
		var sug bulkSuggestion
		for _, h := range historyByFolded[folded] {
			if h.ID == t.ID {
				continue
			}
			if model.NormalizeBillingMode(h.BillingMode) != mode {
				continue
			}
			if mode == model.BillingModeAudio {
				if h.AudioUnitPrice == nil || *h.AudioUnitPrice == 0 {
					continue
				}
				sug = bulkSuggestion{found: true, audio: h.AudioUnitPrice}
			} else {
				if h.InputPrice == 0 && h.OutputPrice == 0 {
					continue
				}
				sug = bulkSuggestion{found: true, input: h.InputPrice, output: h.OutputPrice,
					cacheWrite: h.CacheWritePrice, cacheRead: h.CacheReadPrice}
			}
			break
		}
		if !sug.found {
			if mode == model.BillingModeAudio {
				if a, ok := pricecatalog.LookupAudio(provider.BaseURL, t.ProviderModelName); ok {
					price := a
					sug = bulkSuggestion{found: true, audio: &price}
				}
			} else if p, ok := pricecatalog.Lookup(provider.BaseURL, t.ProviderModelName); ok {
				sug = bulkSuggestion{found: true, input: p.Input, output: p.Output,
					cacheWrite: p.CacheWrite, cacheRead: p.CacheRead}
			}
		}
		if sug.found {
			out[t.ID] = sug
		}
	}
	return out, nil
}
