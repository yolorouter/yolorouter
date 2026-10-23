package modeladmin

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/pricecatalog"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// Per-item outcomes of a bulk import. "created" made a new model plus its
// mapping, "appended" added a mapping to a model that already existed, and
// "skipped" stored nothing — Reason then tells why (BatchSkipReasonExists /
// BatchSkipReasonInvalid, shared with CreateModelsBatch so clients group both
// summaries the same way; BatchSkipReasonModalityMismatch and
// BatchSkipReasonCapabilityMismatch are import-only).
const (
	ImportStatusCreated  = "created"
	ImportStatusAppended = "appended"
	ImportStatusSkipped  = "skipped"
)

// BatchSkipReasonModalityMismatch is import-only: the row declared output
// modalities for a name whose model already exists with a different stored
// declaration. The mapping is not stored — billing and probe selection both
// follow the model row, so appending anyway would silently bill and probe
// against the model the declaration contradicts.
const BatchSkipReasonModalityMismatch = "modality_mismatch"

// BatchSkipReasonCapabilityMismatch is import-only, judged right after the
// modality conflict: the row declared a capability that contradicts what the
// named model's stored row already declares, or submitted a max_output or
// context pairing that breaks the same consistency rule the candidate save
// paths enforce. The row stores nothing — no fill for the slots that agreed,
// no mapping, no requeue — for the same reason a modality conflict does not:
// billing, probing and routing all follow the model row, and a declaration
// the row just contradicted must not be honored silently.
const BatchSkipReasonCapabilityMismatch = "capability_mismatch"

type ImportModelItem struct {
	ProviderModelName string
	// ModelName is the row's optional target model name — the name the model
	// is found or created under when the row declares one, instead of the
	// upstream name. It must pass the same name rules as the create path and
	// an invalid target skips the row; the upstream name stays what the
	// mapping sends upstream and what the result reports.
	ModelName *string
	// InputPrice / OutputPrice are the token slot prices; nil means the row
	// omitted the field, which stores the zero the columns carry today. A
	// negative value is a typo that would bill backwards and skips the row.
	InputPrice      *float64
	OutputPrice     *float64
	CacheWritePrice *float64
	CacheReadPrice  *float64
	MaxOutput       int
	// AudioUnitPrice is the per-million-characters price an audio-only row
	// carries in place of the token slots.
	AudioUnitPrice *float64
	// OutputModalities declares what the imported model produces. Optional:
	// an empty list imports as text-only, the same default every model
	// without a declaration gets. When the import CREATES the model the
	// declaration is stored on it; when the model already exists, an
	// explicit declaration must MATCH the stored one (a mismatch skips the
	// row — the model edit is the only lever that changes a live model's
	// pools, never a bulk import row), while an absent declaration follows
	// whatever the model already says. An invalid list skips the row, the
	// same best-effort treatment an invalid name gets.
	OutputModalities []string
	// InputModalities declares what the model accepts as input. Optional: an
	// absent list leaves the input declaration alone (a created model starts
	// undeclared). When the import CREATES the model the declaration is
	// stored on it with its image-input mirror derived in the same
	// statement; when the model already exists the capability adjudication
	// applies — an undeclared slot is filled, an agreeing declaration is a
	// no-op, a contradicting one skips the row. An invalid list skips the
	// row.
	InputModalities []string
	// ContextLength, SupportsTools, SupportsReasoning and Temperature carry
	// the model-level capability declarations under the same convention: an
	// absent field leaves its slot alone, a declared value fills an
	// undeclared slot on the shared model row, and a declared value that
	// contradicts a stored one skips the row. The tri-state fields accept
	// only "yes" and "no" — a bulk row never clears a stored declaration.
	// Every range is validated per row: a non-positive context, an
	// out-of-range temperature and a negative price or max_output skip the
	// row as invalid instead of failing the batch.
	ContextLength     *int
	SupportsTools     *string
	SupportsReasoning *string
	Temperature       *float64
}

type ImportItemResult struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	ModelID     uint   `json:"model_id,omitempty"`
	CandidateID uint   `json:"candidate_id,omitempty"`
}

type ImportProviderModelsResult struct {
	Items    []ImportItemResult `json:"items"`
	Created  int                `json:"created"`
	Appended int                `json:"appended"`
	Skipped  int                `json:"skipped"`
}

// importTransactionAttempts bounds the whole-batch retry below. Losing a
// unique-constraint race needs another writer to have won it in the same
// instant, so a couple of re-runs cover realistic contention while still
// terminating.
const importTransactionAttempts = 3

// ImportProviderModels stores each requested upstream model as a model +
// candidate pair for the given provider, best-effort per item: invalid names,
// out-of-domain values and already-present mappings are skips, not errors.
// The external model name is the upstream name verbatim unless the row
// declares a model name of its own; a name whose model already exists gets a
// mapping appended (that is the multi-provider failover shape), and the
// provider's existing mappings are left untouched. Capability declarations
// follow the same per-row contract: they fill what the model never declared,
// agree silently with what it did, and a row whose declarations contradict
// the stored model is refused whole.
//
// All writes run in ONE transaction, for the same reason as CreateModelsBatch:
// a storage failure must not leave half the batch committed behind an error
// response, or the retry would report those rows as already existing.
//
// A transaction that fails on a unique violation is re-run a bounded number of
// times rather than surfaced: every UNIQUE on the touched tables — model name,
// (model_id, provider_id), (model_id, sort_order) — can only fire here when a
// concurrent writer claimed the row between this batch's lookup and its insert,
// and a re-run resolves each one to its intended outcome (skip, append, or a
// fresh sort position) instead of a raw constraint error the caller cannot act
// on. The rolled-back attempt left nothing behind, so re-running is clean.
func (s *ModelService) ImportProviderModels(providerID uint, items []ImportModelItem, now time.Time) (*ImportProviderModelsResult, error) {
	if _, err := repository.FindProviderByID(s.db, providerID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderNotFound
		}
		return nil, err
	}

	for attempt := 1; ; attempt++ {
		result, err := s.importProviderModelsOnce(providerID, items, now)
		if err == nil {
			return result, nil
		}
		// Deadlocks/serialization aborts (Postgres) get the same treatment as
		// unique-violation races: the failed transaction persisted nothing, so
		// re-running resolves the conflict.
		if attempt < importTransactionAttempts && (repository.IsUniqueViolation(err) || repository.IsTxSerializationFailure(err)) {
			continue
		}
		return nil, err
	}
}

// importProviderModelsOnce runs one whole-batch attempt. The database work is
// a fixed number of statements regardless of batch size — four preloads (by
// name, mapped-model membership, max sort orders, and the models' stored
// candidates for the context rule) and two bulk inserts, plus one model-row
// write per row whose declarations land on slots its model never declared —
// so a full-catalog import does not hold the transaction open for thousands
// of sequential round trips against a networked database.
func (s *ModelService) importProviderModelsOnce(providerID uint, items []ImportModelItem, now time.Time) (*ImportProviderModelsResult, error) {
	results := make([]ImportItemResult, len(items))
	// First occurrence of each valid TARGET name owns the work; later
	// occurrences are in-batch duplicates and skip as existing (their ModelID
	// is backfilled after the transaction resolves ids). The target name is
	// the row's declared model name when it declares one and the upstream
	// name otherwise — the name the model row is found or created under. One
	// row per target per batch also keeps UNIQUE(model_id, provider_id)
	// collision-free within the batch.
	firstIndex := make(map[string]int, len(items))
	// firstRef points an in-batch duplicate at the row that owns its target
	// for the post-transaction id backfill — the duplicate's result carries
	// its own upstream name, which is not the lookup key when rows declare
	// model names of their own.
	firstRef := make(map[int]int, len(items))
	resolvedModalities := make(map[string]resolvedModality, len(items))
	resolvedCaps := make(map[string]resolvedCapabilities, len(items))
	names := make([]string, 0, len(items))
	for i, item := range items {
		name := strings.TrimSpace(item.ProviderModelName)
		target := name
		if item.ModelName != nil {
			target = strings.TrimSpace(*item.ModelName)
		}
		if !isValidModelName(target) {
			results[i] = ImportItemResult{Name: name, Status: ImportStatusSkipped, Reason: BatchSkipReasonInvalid}
			continue
		}
		if _, dup := firstIndex[target]; dup {
			results[i] = ImportItemResult{Name: name, Status: ImportStatusSkipped, Reason: BatchSkipReasonExists}
			firstRef[i] = firstIndex[target]
			continue
		}
		// The same validation the single-create path applies, but a bad
		// declaration costs only its own row: the import is best-effort per
		// item, so a typo'd modality skips like a typo'd name would. The
		// validated LIST is kept, not its serialized form: the conflict
		// comparison below is order-insensitive, and serialization would make
		// an identical declaration in a different id order read as a conflict.
		// An absent declaration resolves to the text-only default here; the
		// conflict check below still tells "absent" apart from "declared"
		// through the raw item, so the default never masquerades as a claim.
		resolved := item.OutputModalities
		if len(resolved) == 0 {
			resolved = []string{model.OutputModalityText}
		}
		canonical, err := model.CanonicalOutputModalities(resolved)
		if err != nil {
			results[i] = ImportItemResult{Name: name, Status: ImportStatusSkipped, Reason: BatchSkipReasonInvalid}
			continue
		}
		// Every capability and price value domain is validated here, per row:
		// an out-of-range or misspelled value skips just its own row, the
		// same best-effort treatment a bad name or modality gets.
		caps, ok := resolveImportCapabilities(item)
		if !ok {
			results[i] = ImportItemResult{Name: name, Status: ImportStatusSkipped, Reason: BatchSkipReasonInvalid}
			continue
		}
		// A row that contradicts its own context declaration is refused
		// before anything is stored: the model it names would be created
		// holding a declaration its own submission already breaks, and a
		// per-row skip must leave nothing behind.
		if caps.contextLength != nil && item.MaxOutput > *caps.contextLength {
			results[i] = ImportItemResult{Name: name, Status: ImportStatusSkipped, Reason: BatchSkipReasonCapabilityMismatch}
			continue
		}
		firstIndex[target] = i
		resolvedModalities[target] = resolvedModality{declared: len(item.OutputModalities) > 0, list: resolved, canonical: canonical}
		resolvedCaps[target] = caps
		names = append(names, target)
	}

	if len(names) > 0 {
		err := s.db.Transaction(func(tx *gorm.DB) error {
			existing, err := repository.ListModelsByNames(tx, names)
			if err != nil {
				return err
			}
			modelsByName := make(map[string]*model.Model, len(names))
			for i := range existing {
				modelsByName[existing[i].Name] = &existing[i]
			}

			newModels := make([]*model.Model, 0)
			for _, name := range names {
				if _, ok := modelsByName[name]; ok {
					continue
				}
				caps := resolvedCaps[name]
				newModels = append(newModels, &model.Model{
					Name:              name,
					ManagementStatus:  model.ModelStatusEnabled,
					SchedulingMode:    model.ModelSchedulingModeFailover,
					OutputModalities:  resolvedModalities[name].canonical,
					ContextLength:     caps.contextLength,
					SupportsTools:     caps.supportsTools,
					SupportsReasoning: caps.supportsReasoning,
					Temperature:       caps.temperature,
					InputModalities:   caps.inputCanonical,
					// The mirror boolean derives from the array in the same
					// insert — the one hard rule every writer of
					// input_modalities shares. An undeclared list derives
					// nil, the column's own undeclared value.
					SupportsImageInput: model.DeriveImageInputSupport(caps.inputCanonical),
					CreatedAt:          now, UpdatedAt: now,
				})
			}
			// A unique violation here means a concurrent request claimed a name
			// between the preload and this insert; the whole batch rolls back
			// and the outer retry resolves it.
			if err := repository.CreateModelsBulk(tx, newModels); err != nil {
				return err
			}
			createdNames := make(map[string]bool, len(newModels))
			for _, m := range newModels {
				modelsByName[m.Name] = m
				createdNames[m.Name] = true
			}

			modelIDs := make([]uint, 0, len(names))
			for _, name := range names {
				modelIDs = append(modelIDs, modelsByName[name].ID)
			}
			mapped, err := repository.ListProviderMappingsByModelIDs(tx, providerID, modelIDs)
			if err != nil {
				return err
			}
			maxOrders, err := repository.MaxCandidateSortOrders(tx, modelIDs)
			if err != nil {
				return err
			}
			// One read covers the sibling side of the as-if-enabled context
			// rule applied per row below: each model's already-stored enabled
			// candidates, whose max_output must fit the effective context
			// window. Candidates this batch creates are absent here (and
			// disabled anyway); a row's own submission is checked directly.
			storedCandidates, err := repository.ListModelCandidatesByModelIDs(tx, modelIDs)
			if err != nil {
				return err
			}
			enabledMaxOutputs := make(map[uint][]int, len(modelIDs))
			for _, row := range storedCandidates {
				if row.ManagementStatus != model.ModelCandidateStatusEnabled {
					continue
				}
				enabledMaxOutputs[row.ModelID] = append(enabledMaxOutputs[row.ModelID], row.MaxOutput)
			}

			newCandidates := make([]*model.ModelCandidate, 0, len(names))
			candidateNames := make([]string, 0, len(names))
			requeueIDs := make([]uint, 0)
			for _, name := range names {
				m := modelsByName[name]
				item := items[firstIndex[name]]
				// The result names the row's submitted upstream name — the
				// target name is the model's, which the two only coincide
				// with when the row declares no model name of its own.
				rowName := strings.TrimSpace(item.ProviderModelName)
				caps := resolvedCaps[name]
				// A contradicting declaration outranks every other skip reason
				// for the row, on both the append and the exists branch: the
				// row's understanding of the model is wrong, and reporting
				// anything else (a routine "exists", or an appended mapping)
				// would either hide that or store a mapping whose next probe
				// runs against a modality the admin just disavowed. No requeue
				// either — the probe would use the contradicted declaration.
				// An absent declaration never conflicts: it follows the model
				// row, which is what the requeue recovery path relies on.
				if declaresConflict(m, resolvedModalities[name]) {
					results[firstIndex[name]] = ImportItemResult{
						Name: rowName, Status: ImportStatusSkipped,
						Reason: BatchSkipReasonModalityMismatch, ModelID: m.ID,
					}
					continue
				}
				// Capability adjudication, row-atomic and judged right after
				// the modality conflict: a declaration that contradicts a
				// stored one skips the whole row with nothing written — no
				// fill for the slots that agreed, no mapping, no requeue.
				fill, conflict := adjudicateCapabilities(m, caps)
				if conflict {
					results[firstIndex[name]] = ImportItemResult{
						Name: rowName, Status: ImportStatusSkipped,
						Reason: BatchSkipReasonCapabilityMismatch, ModelID: m.ID,
					}
					continue
				}
				// The as-if-enabled context rule, the same criteria the
				// candidate save paths enforce: a row that submits a non-zero
				// max_output or declares a context is held to max_output
				// within the effective window — against the model's enabled
				// sibling candidates and against the row's own submission,
				// which its mapping would carry the moment a probe enables
				// it.
				if contextConflict(item, caps, m, enabledMaxOutputs[m.ID]) {
					results[firstIndex[name]] = ImportItemResult{
						Name: rowName, Status: ImportStatusSkipped,
						Reason: BatchSkipReasonCapabilityMismatch, ModelID: m.ID,
					}
					continue
				}
				// Declarations that land on slots the model never declared
				// fill them on the shared model row — on the append branch
				// below and the exists branch alike (a row whose mapping is
				// already stored still carries knowledge the model row
				// lacks). The write is column-scoped, not the model PATCH
				// path: the import only read and adjudicated these slots, so
				// riding UpdateModel's unconditional name/status write would
				// let the stale copies it carried revert a concurrent model
				// PATCH in the window between this batch's lookup and write.
				// The deprecated image-input mirror still derives from the
				// submitted input array in the same statement.
				if !fill.empty() {
					if err := repository.UpdateModelCapabilityFill(tx, m.ID, repository.ModelCapabilityFill{
						ContextLengthSet:     fill.contextLength,
						ContextLength:        caps.contextLength,
						SupportsToolsSet:     fill.supportsTools,
						SupportsTools:        caps.supportsTools,
						SupportsReasoningSet: fill.supportsReasoning,
						SupportsReasoning:    caps.supportsReasoning,
						TemperatureSet:       fill.temperature,
						Temperature:          caps.temperature,
						InputModalitiesSet:   fill.inputModalities,
						InputModalities:      caps.inputCanonical,
					}, now); err != nil {
						return err
					}
				}
				if existing, ok := mapped[m.ID]; ok {
					skip := ImportItemResult{Name: rowName, Status: ImportStatusSkipped, Reason: BatchSkipReasonExists, ModelID: m.ID}
					// An existing mapping still waiting on a verdict surfaces its
					// id so the caller can requeue it — re-import is the promised
					// recovery for probes lost to a restart (the queue is not
					// durable). A mapping that already holds a verdict is done;
					// leaving its id out keeps it from being re-probed unasked.
					if existing.VerificationStatus == model.ModelVerificationStatusUntested {
						skip.CandidateID = existing.ID
						requeueIDs = append(requeueIDs, existing.ID)
					}
					results[firstIndex[name]] = skip
					continue
				}
				// Billing mode follows the model row's stored declaration —
				// for a created model that is the declaration this batch just
				// resolved, for an existing one it is whatever the model
				// already says, so a re-import of an image model derives
				// per-image even when the row forgot to re-declare. A model
				// that also serves text keeps token settlement: the mapping
				// carries its chat traffic too, and per-image pricing applies
				// to image requests alone. Image tiers stay empty — the import
				// table carries per-M prices, not a quality-by-size table, so
				// an imported image mapping starts as known-unpriced until an
				// admin fills the tier editor.
				billingMode := model.BillingModeToken
				if m.OutputImageExclusive() {
					billingMode = model.BillingModeImage
				}
				// An audio-only model's mapping bills per character and
				// carries the row's single price; the token slots stay
				// inert under that mode. The exclusive helpers overlap only
				// on a hand-written ["image","audio"] declaration (the
				// pickers collapse exclusive ids to one); audio is checked
				// last, so that shape lands audio — one mapping bills in
				// one mode, and the declaration's owner should not have
				// written two exclusive ids.
				var audioPrice *float64
				if m.OutputAudioExclusive() {
					billingMode = model.BillingModeAudio
					audioPrice = item.AudioUnitPrice
				}
				newCandidates = append(newCandidates, &model.ModelCandidate{
					ModelID: m.ID, ProviderID: providerID, ProviderModelName: rowName,
					InputPrice: priceOrDefault(item.InputPrice), OutputPrice: priceOrDefault(item.OutputPrice),
					CacheWritePrice: item.CacheWritePrice, CacheReadPrice: item.CacheReadPrice,
					MaxOutput:          item.MaxOutput,
					BillingMode:        billingMode,
					AudioUnitPrice:     audioPrice,
					ManagementStatus:   model.ModelCandidateStatusDisabled,
					VerificationStatus: model.ModelVerificationStatusUntested,
					// The import's auto-enable promise, persisted so the queue
					// can honor it at commit time and an explicit disable can
					// revoke it in between. ArmedAt equals this row's
					// UpdatedAt on purpose: the probe commit's enable checks
					// that alignment, and any later write — by any binary
					// version — bumps updated_at and thereby blocks it.
					AutoEnableOnPass: true,
					ArmedAt:          &now,
					// Each model appears once per batch (per-name dedup above),
					// so max+1 cannot collide within the batch itself.
					SortOrder: maxOrders[m.ID] + 1,
					CreatedAt: now, UpdatedAt: now, PriceUpdatedAt: now,
				})
				candidateNames = append(candidateNames, name)
			}
			if err := repository.CreateModelCandidatesBulk(tx, newCandidates); err != nil {
				return err
			}
			// Requeued mappings shed their previous attempt's record inside the
			// same transaction: the fresh probe supersedes it, and the cleared
			// last_tested_at is what keeps pollers on OTHER instances (blind to
			// this process's in-memory queue) treating the row as still waiting
			// rather than settled-inconclusive. The advanced probe token makes
			// every probe already in flight for these rows miss its commit.
			//
			// The clear is conditional (Untested guard), so the response may
			// only report the rows it ACTUALLY re-armed — identified by the
			// requeue token. A row a concurrent probe settled between the
			// read above and this UPDATE keeps its own token; announcing it
			// as requeued would make the caller enqueue a probe for a row
			// that is done and whose enable promise was never renewed.
			requeueToken := newRequeueRunID()
			if err := repository.ClearCandidatesProbeResidue(tx, requeueIDs, requeueToken, now); err != nil {
				return err
			}
			hitIDs, err := repository.ListCandidateIDsByProbeRunID(tx, requeueIDs, requeueToken)
			if err != nil {
				return err
			}
			hit := make(map[uint]bool, len(hitIDs))
			for _, id := range hitIDs {
				hit[id] = true
			}
			for i := range results {
				r := &results[i]
				if r.Status == ImportStatusSkipped && r.CandidateID != 0 && !hit[r.CandidateID] {
					r.CandidateID = 0
				}
			}
			for j, c := range newCandidates {
				name := candidateNames[j]
				status := ImportStatusAppended
				if createdNames[name] {
					status = ImportStatusCreated
				}
				rowName := strings.TrimSpace(items[firstIndex[name]].ProviderModelName)
				results[firstIndex[name]] = ImportItemResult{Name: rowName, Status: status, ModelID: c.ModelID, CandidateID: c.ID}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Backfill in-batch duplicates with the model id the row owning their
	// target resolved to, then tally.
	result := &ImportProviderModelsResult{Items: results}
	for i := range results {
		r := &results[i]
		if r.Status == ImportStatusSkipped && r.Reason == BatchSkipReasonExists && r.ModelID == 0 {
			if j, ok := firstRef[i]; ok {
				r.ModelID = results[j].ModelID
			}
		}
		switch r.Status {
		case ImportStatusCreated:
			result.Created++
		case ImportStatusAppended:
			result.Appended++
		case ImportStatusSkipped:
			result.Skipped++
		}
	}
	return result, nil
}

// SuggestCandidatePrices is the batch form of SuggestCandidatePrice: one call
// per import dialog instead of one per row. Every requested name gets an entry
// — a miss comes back with an empty Source so the form leaves those fields at
// their defaults. The provider is resolved once up front (the single form only
// loads it on a history miss) because the batch caller always needs it for the
// catalog host and a bad provider id should fail loudly, not per-name.
func (s *ModelService) SuggestCandidatePrices(providerID uint, names []string) (map[string]SuggestedPrice, error) {
	provider, err := repository.FindProviderByID(s.db, providerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderNotFound
		}
		return nil, err
	}

	// One query fetches every name's history up front — a full-catalog dialog
	// would otherwise turn this endpoint into an N+1 path against the database.
	requested := make([]string, 0, len(names))
	folded := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		requested = append(requested, name)
		if f := model.FoldModelName(name); f != "" {
			folded = append(folded, f)
		}
	}
	history, err := repository.FindLatestCandidatePricesByFoldedNames(s.db, providerID, folded)
	if err != nil {
		return nil, err
	}

	result := make(map[string]SuggestedPrice, len(requested))
	for _, name := range requested {
		if _, dup := result[name]; dup {
			continue
		}
		if hist, ok := history[model.FoldModelName(name)]; ok {
			// An audio-mode history row suggests its character price; the
			// four token slots are inert under that mode.
			if model.NormalizeBillingMode(hist.BillingMode) == model.BillingModeAudio {
				if hist.AudioUnitPrice != nil {
					result[name] = SuggestedPrice{AudioUnitPrice: hist.AudioUnitPrice, Source: PriceSourceHistory}
				} else {
					result[name] = SuggestedPrice{}
				}
				continue
			}
			result[name] = SuggestedPrice{
				InputPrice:      hist.InputPrice,
				OutputPrice:     hist.OutputPrice,
				CacheWritePrice: hist.CacheWritePrice,
				CacheReadPrice:  hist.CacheReadPrice,
				Source:          PriceSourceHistory,
			}
			continue
		}
		if p, ok := pricecatalog.Lookup(provider.BaseURL, name); ok {
			result[name] = SuggestedPrice{
				InputPrice:       p.Input,
				OutputPrice:      p.Output,
				CacheWritePrice:  p.CacheWrite,
				CacheReadPrice:   p.CacheRead,
				Source:           PriceSourceSeed,
				CatalogUpdatedAt: pricecatalog.UpdatedAt(),
			}
			continue
		}
		if a, ok := pricecatalog.LookupAudio(provider.BaseURL, name); ok {
			result[name] = SuggestedPrice{AudioUnitPrice: &a, Source: PriceSourceSeed, CatalogUpdatedAt: pricecatalog.UpdatedAt()}
			continue
		}
		result[name] = SuggestedPrice{}
	}
	return result, nil
}

// resolvedModality is one row's validated declaration in every form it is
// consumed: the id list for the order-insensitive conflict comparison, the
// canonical JSON for storage on a newly created model, and whether the row
// declared anything at all — the absent-vs-default distinction the conflict
// check keys on (an absent declaration follows the model row and never
// conflicts; a defaulted one must not masquerade as a claim).
type resolvedModality struct {
	declared  bool
	list      []string
	canonical string
}

// declaresConflict reports whether an import row explicitly declares output
// modalities that contradict what the named model's stored row already says.
// Modality is model-global — it decides routing pools, billing mode, and
// which probe verifies the mapping — so an agreeing declaration is redundant
// (fine) while a contradicting one, honored silently, would bill and probe
// against a model the row just disavowed. The model edit is the lever that
// actually changes the pools; never a bulk import row.
func declaresConflict(m *model.Model, rm resolvedModality) bool {
	if !rm.declared {
		return false
	}
	return !sameModalitySet(rm.list, m.OutputModalityList())
}

// sameModalitySet compares two modality lists as sets. The stored
// declaration's id order is not guaranteed to match a freshly submitted
// one — the write path canonicalizes content, not order — so both a
// serialized and an element-wise comparison would flag an identical
// declaration as a conflict.
func sameModalitySet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			return false
		}
	}
	return true
}

// resolvedCapabilities is one row's validated capability declaration in
// every form the import consumes: the typed values a created model row
// starts with, the comparison inputs the existing-model adjudication keys
// on, and the canonical forms the fill write stores. A nil or absent member
// means the row leaves that slot alone — it neither fills it nor is held
// against it.
type resolvedCapabilities struct {
	contextLength     *int
	supportsTools     *bool
	supportsReasoning *bool
	temperature       *float64
	// inputCanonical is the stored JSON form of the declared input
	// modalities and inputList the folded id list the order-insensitive
	// conflict comparison reads. declared distinguishes a submission from an
	// absent field (the import treats an empty submitted list as absent —
	// there is no clear-to-undeclared on a bulk row).
	inputDeclared  bool
	inputCanonical string
	inputList      []string
}

// importTriState folds one row's tri-state string into the boolean the
// model column stores. The import accepts only the affirmative half of the
// vocabulary — "yes" and "no"; "unknown" means "clear a stored declaration",
// a model-edit concern no bulk row may perform silently, so it is rejected
// like any other out-of-vocabulary value. ok is false then.
func importTriState(s *string) (value *bool, ok bool) {
	if s == nil {
		return nil, true
	}
	switch *s {
	case "yes":
		t := true
		return &t, true
	case "no":
		f := false
		return &f, true
	default:
		return nil, false
	}
}

// resolveImportCapabilities validates one row's capability and price value
// domains and folds the declarations into the forms the import consumes:
// a positive context, the yes/no vocabulary, the temperature range the model
// save path enforces, a valid input-modality list, and non-negative prices
// and max_output. ok is false when any submitted value is out of domain;
// the row then skips as invalid, the same per-row treatment a bad name gets.
func resolveImportCapabilities(item ImportModelItem) (caps resolvedCapabilities, ok bool) {
	if item.ContextLength != nil {
		if *item.ContextLength <= 0 {
			return caps, false
		}
		caps.contextLength = item.ContextLength
	}
	var valid bool
	if caps.supportsTools, valid = importTriState(item.SupportsTools); !valid {
		return caps, false
	}
	if caps.supportsReasoning, valid = importTriState(item.SupportsReasoning); !valid {
		return caps, false
	}
	if item.Temperature != nil {
		if *item.Temperature < modelTemperatureMin || *item.Temperature > modelTemperatureMax {
			return caps, false
		}
		caps.temperature = item.Temperature
	}
	if len(item.InputModalities) > 0 {
		// The ids fold the same way the write path folds them, so a sloppy
		// casing cannot turn an agreeing declaration into a conflict; the
		// canonical form validates the folded list and is what gets stored.
		folded := make([]string, 0, len(item.InputModalities))
		for _, raw := range item.InputModalities {
			folded = append(folded, strings.ToLower(strings.TrimSpace(raw)))
		}
		canonical, err := model.CanonicalInputModalities(folded)
		if err != nil {
			return caps, false
		}
		caps.inputDeclared = true
		caps.inputCanonical = canonical
		caps.inputList = folded
	}
	for _, price := range []*float64{item.InputPrice, item.OutputPrice, item.CacheWritePrice, item.CacheReadPrice, item.AudioUnitPrice} {
		if price != nil && *price < 0 {
			return caps, false
		}
	}
	if item.MaxOutput < 0 {
		return caps, false
	}
	return caps, true
}

// capabilityFill marks which slots a row's declaration landed on for the
// model row to fill. A declaration only ever WRITES a slot the model never
// declared — an agreeing one is the same no-op an absent field is, and a
// contradicting one is a conflict the row is refused for.
type capabilityFill struct {
	contextLength     bool
	supportsTools     bool
	supportsReasoning bool
	temperature       bool
	inputModalities   bool
}

func (f capabilityFill) empty() bool {
	return !f.contextLength && !f.supportsTools && !f.supportsReasoning && !f.temperature && !f.inputModalities
}

// adjudicateCapabilities compares one row's validated capability declaration
// against the named model's stored row and reports how the row may proceed.
// A declaration on a slot the model never declared fills it; one that agrees
// with the stored value is redundant (no extra write); one that contradicts
// a STORED declaration conflicts — the model edit is the only lever that
// changes a live model's declarations, never a bulk import row, and a
// conflict refuses the whole row with nothing written. A just-created model
// row already carries the row's own declarations, so every slot agrees by
// construction and the fill comes back empty.
func adjudicateCapabilities(m *model.Model, caps resolvedCapabilities) (fill capabilityFill, conflict bool) {
	if caps.contextLength != nil {
		if m.ContextLength == nil {
			fill.contextLength = true
		} else if *m.ContextLength != *caps.contextLength {
			return capabilityFill{}, true
		}
	}
	if caps.supportsTools != nil {
		if m.SupportsTools == nil {
			fill.supportsTools = true
		} else if *m.SupportsTools != *caps.supportsTools {
			return capabilityFill{}, true
		}
	}
	if caps.supportsReasoning != nil {
		if m.SupportsReasoning == nil {
			fill.supportsReasoning = true
		} else if *m.SupportsReasoning != *caps.supportsReasoning {
			return capabilityFill{}, true
		}
	}
	if caps.temperature != nil {
		if m.Temperature == nil {
			fill.temperature = true
		} else if *m.Temperature != *caps.temperature {
			return capabilityFill{}, true
		}
	}
	if caps.inputDeclared {
		stored := m.InputModalityList()
		if len(stored) == 0 {
			fill.inputModalities = true
		} else if !sameModalitySet(caps.inputList, stored) {
			return capabilityFill{}, true
		}
	}
	return fill, false
}

// contextConflict reports whether the row breaks the as-if-enabled context
// rule — the same criteria the candidate save paths enforce (max_output
// within the declared context window; equal passes; an undeclared context
// pins nothing). The rule arms when the row submits a non-zero max_output
// or declares a context length, and then holds against the model's enabled
// sibling candidates and against the row's own submission, which its
// mapping would carry the moment a probe enables it. The effective context
// is the row's declaration when it declares one (adjudication has already
// proven it agrees with any stored one), otherwise the stored one.
func contextConflict(item ImportModelItem, caps resolvedCapabilities, m *model.Model, siblingMaxOutputs []int) bool {
	if item.MaxOutput == 0 && caps.contextLength == nil {
		return false
	}
	effective := caps.contextLength
	if effective == nil {
		effective = m.ContextLength
	}
	if effective == nil {
		return false
	}
	for _, maxOutput := range siblingMaxOutputs {
		if maxOutput > *effective {
			return true
		}
	}
	return item.MaxOutput > *effective
}

// priceOrDefault unwraps a submitted token-slot price, treating an absent
// one as the zero the columns store today. (Carrying "unpriced" as its own
// state is later work; the pointers exist so the transport can say "absent"
// without the service reading a submitted 0.)
func priceOrDefault(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
