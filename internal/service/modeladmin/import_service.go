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
// summaries the same way).
const (
	ImportStatusCreated  = "created"
	ImportStatusAppended = "appended"
	ImportStatusSkipped  = "skipped"
)

type ImportModelItem struct {
	ProviderModelName string
	InputPrice        float64
	OutputPrice       float64
	CacheWritePrice   *float64
	CacheReadPrice    *float64
	MaxOutput         int
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
// candidate pair for the given provider, best-effort per item: invalid names
// and already-present mappings are skips, not errors. The external model name
// is the upstream name verbatim; a name whose model already exists gets a
// mapping appended (that is the multi-provider failover shape), and the
// provider's existing mappings are left untouched.
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
// a fixed number of statements regardless of batch size — three preloads (by
// name, mapped-model membership, max sort orders) and two bulk inserts — so a
// full-catalog import does not hold the transaction open for thousands of
// sequential round trips against a networked database.
func (s *ModelService) importProviderModelsOnce(providerID uint, items []ImportModelItem, now time.Time) (*ImportProviderModelsResult, error) {
	results := make([]ImportItemResult, len(items))
	// First occurrence of each valid name owns the work; later occurrences are
	// in-batch duplicates and skip as existing (their ModelID is backfilled
	// after the transaction resolves ids).
	firstIndex := make(map[string]int, len(items))
	names := make([]string, 0, len(items))
	for i, item := range items {
		name := strings.TrimSpace(item.ProviderModelName)
		if !isValidModelName(name) {
			results[i] = ImportItemResult{Name: name, Status: ImportStatusSkipped, Reason: BatchSkipReasonInvalid}
			continue
		}
		if _, dup := firstIndex[name]; dup {
			results[i] = ImportItemResult{Name: name, Status: ImportStatusSkipped, Reason: BatchSkipReasonExists}
			continue
		}
		firstIndex[name] = i
		names = append(names, name)
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
				newModels = append(newModels, &model.Model{Name: name, ManagementStatus: model.ModelStatusEnabled, SchedulingMode: model.ModelSchedulingModeFailover, CreatedAt: now, UpdatedAt: now})
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

			newCandidates := make([]*model.ModelCandidate, 0, len(names))
			candidateNames := make([]string, 0, len(names))
			requeueIDs := make([]uint, 0)
			for _, name := range names {
				m := modelsByName[name]
				if existing, ok := mapped[m.ID]; ok {
					skip := ImportItemResult{Name: name, Status: ImportStatusSkipped, Reason: BatchSkipReasonExists, ModelID: m.ID}
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
				item := items[firstIndex[name]]
				newCandidates = append(newCandidates, &model.ModelCandidate{
					ModelID: m.ID, ProviderID: providerID, ProviderModelName: name,
					InputPrice: item.InputPrice, OutputPrice: item.OutputPrice,
					CacheWritePrice: item.CacheWritePrice, CacheReadPrice: item.CacheReadPrice,
					MaxOutput:          item.MaxOutput,
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
				results[firstIndex[name]] = ImportItemResult{Name: name, Status: status, ModelID: c.ModelID, CandidateID: c.ID}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Backfill in-batch duplicates with the model id their first occurrence
	// resolved to, then tally.
	result := &ImportProviderModelsResult{Items: results}
	for i := range results {
		r := &results[i]
		if r.Status == ImportStatusSkipped && r.Reason == BatchSkipReasonExists && r.ModelID == 0 {
			if j, ok := firstIndex[r.Name]; ok {
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
		result[name] = SuggestedPrice{}
	}
	return result, nil
}
