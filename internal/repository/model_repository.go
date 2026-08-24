// Package repository provides Model / ModelCandidate pure data access.
package repository

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

func FindModelByID(db *gorm.DB, id uint) (*model.Model, error) {
	var m model.Model
	if err := db.Where("id = ?", id).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func FindModelByName(db *gorm.DB, name string) (*model.Model, error) {
	var m model.Model
	if err := db.Where("name = ?", name).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func ListModels(db *gorm.DB) ([]model.Model, error) {
	var models []model.Model
	if err := db.Order("id ASC").Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

func CreateModel(db *gorm.DB, m *model.Model) error {
	return db.Create(m).Error
}

// ModelUpdate carries one PATCH's writes to a model row. Name and Status are
// always written; the optional fields keep "absent, leave the column alone"
// distinguishable from a submitted value. ImageInput needs the explicit
// set-flag because its value is itself tri-state: nil with ImageInputSet=true
// stores NULL (clearing a previous declaration). SchedulingMode has no NULL
// state, so the pointer alone carries set-ness: nil leaves the column
// untouched.
type ModelUpdate struct {
	Name           string
	Status         int
	ImageInputSet  bool
	ImageInput     *bool
	SchedulingMode *model.SchedulingMode
}

// UpdateModel writes every submitted field of u in ONE statement,
// so a concurrent PATCH can never interleave the fields into a row neither
// caller submitted.
func UpdateModel(db *gorm.DB, id uint, u ModelUpdate, now time.Time) error {
	updates := map[string]interface{}{"name": u.Name, "management_status": u.Status, "updated_at": now}
	if u.ImageInputSet {
		updates["supports_image_input"] = u.ImageInput
	}
	if u.SchedulingMode != nil {
		updates["scheduling_mode"] = *u.SchedulingMode
	}
	return db.Model(&model.Model{}).Where("id = ?", id).Updates(updates).Error
}

func ListModelCandidatesByModelID(db *gorm.DB, modelID uint) ([]model.ModelCandidate, error) {
	var candidates []model.ModelCandidate
	if err := db.Preload("Provider").Where("model_id = ?", modelID).Order("sort_order ASC").Find(&candidates).Error; err != nil {
		return nil, err
	}
	return candidates, nil
}

// ListModelCandidatesByModelIDs batches the N+1 that a naive per-model
// candidate lookup would cause when listing models (the same fix used for
// ListProviderKeysByProviderIDs).
func ListModelCandidatesByModelIDs(db *gorm.DB, modelIDs []uint) ([]model.ModelCandidate, error) {
	if len(modelIDs) == 0 {
		return nil, nil
	}
	var candidates []model.ModelCandidate
	if err := db.Preload("Provider").Where("model_id IN ?", modelIDs).Order("model_id ASC, sort_order ASC").Find(&candidates).Error; err != nil {
		return nil, err
	}
	return candidates, nil
}

// ListModelCandidatesByProvider returns every mapping a provider serves, in
// insertion order — the provider-scoped view the import progress poll and the
// provider detail page read from.
func ListModelCandidatesByProvider(db *gorm.DB, providerID uint) ([]model.ModelCandidate, error) {
	var candidates []model.ModelCandidate
	if err := db.Where("provider_id = ?", providerID).Order("id ASC").Find(&candidates).Error; err != nil {
		return nil, err
	}
	return candidates, nil
}

// ListModelsByNames resolves a set of names in one query — the bulk-import
// preload that replaces a per-name FindModelByName loop.
func ListModelsByNames(db *gorm.DB, names []string) ([]model.Model, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var models []model.Model
	if err := db.Where("name IN ?", names).Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

// bulkInsertBatchSize caps rows per INSERT so the widest table stays far below
// SQLite's 32,766 bind-variable limit (ModelCandidate maps ~20 columns; one
// unchunked 2,000-row insert would need ~40,000 variables and fail). The
// batches still run inside whatever transaction the caller opened, so
// atomicity is unchanged.
const bulkInsertBatchSize = 500

// CreateModelsBulk inserts the rows in bounded batches. GORM fills the primary
// keys back on both supported backends; per-row hooks still run.
func CreateModelsBulk(db *gorm.DB, models []*model.Model) error {
	if len(models) == 0 {
		return nil
	}
	return db.CreateInBatches(models, bulkInsertBatchSize).Error
}

// CreateModelCandidatesBulk is CreateModelsBulk for candidates; BeforeCreate
// runs per row, so the folded-name and price-clock guarantees hold for bulk
// inserts too.
func CreateModelCandidatesBulk(db *gorm.DB, candidates []*model.ModelCandidate) error {
	if len(candidates) == 0 {
		return nil
	}
	return db.CreateInBatches(candidates, bulkInsertBatchSize).Error
}

// ListProviderMappingsByModelIDs returns this provider's existing mapping for
// each of the given models, keyed by model id — one membership query instead of
// a per-model existence check. The row carries only id, model_id and
// verification_status: presence answers "already mapped?", and the verification
// status answers "finished, or still waiting on a probe?" (an unfinished
// mapping is what a re-import offers to requeue). A model with no mapping is
// simply absent. UNIQUE(model_id, provider_id) guarantees at most one row per
// key.
func ListProviderMappingsByModelIDs(db *gorm.DB, providerID uint, modelIDs []uint) (map[uint]model.ModelCandidate, error) {
	if len(modelIDs) == 0 {
		return map[uint]model.ModelCandidate{}, nil
	}
	var rows []model.ModelCandidate
	if err := db.Model(&model.ModelCandidate{}).
		Select("id", "model_id", "verification_status").
		Where("provider_id = ? AND model_id IN ?", providerID, modelIDs).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	mapped := make(map[uint]model.ModelCandidate, len(rows))
	for _, r := range rows {
		mapped[r.ModelID] = r
	}
	return mapped, nil
}

// MaxCandidateSortOrders returns each model's current highest sort_order in
// one grouped query; models with no candidates are simply absent (treat as 0).
func MaxCandidateSortOrders(db *gorm.DB, modelIDs []uint) (map[uint]int, error) {
	if len(modelIDs) == 0 {
		return map[uint]int{}, nil
	}
	var rows []struct {
		ModelID  uint
		MaxOrder int
	}
	if err := db.Model(&model.ModelCandidate{}).
		Select("model_id, COALESCE(MAX(sort_order), 0) AS max_order").
		Where("model_id IN ?", modelIDs).Group("model_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	orders := make(map[uint]int, len(rows))
	for _, r := range rows {
		orders[r.ModelID] = r.MaxOrder
	}
	return orders, nil
}

func FindModelCandidateByID(db *gorm.DB, id uint) (*model.ModelCandidate, error) {
	var c model.ModelCandidate
	if err := db.Where("id = ?", id).First(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func NextCandidateSortOrder(db *gorm.DB, modelID uint) (int, error) {
	var maxOrder int
	if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", modelID).
		Select("COALESCE(MAX(sort_order), 0)").Scan(&maxOrder).Error; err != nil {
		return 0, err
	}
	return maxOrder + 1, nil
}

func CreateModelCandidate(db *gorm.DB, c *model.ModelCandidate) error {
	return db.Create(c).Error
}

// UpdateModelCandidate writes the edited fields. priceChanged must be true only
// when one of the four price values actually differs from what is stored: the
// price clock is what the auto-suggest look-up orders by, so stamping it on an
// edit that merely re-sent the same numbers (which every save from the candidate
// form does — it always posts the full record) would let a candidate nobody
// repriced overtake one that was, and resurrect its stale rate.
func UpdateModelCandidate(db *gorm.DB, id uint, providerModelName string, inputPrice, outputPrice float64,
	cacheWritePrice, cacheReadPrice *float64, maxOutput int, resetVerification, priceChanged bool, now time.Time) error {
	updates := map[string]interface{}{
		"provider_model_name": providerModelName,
		// Never written without its source column: a row whose folded copy is
		// stale is silently invisible to the price look-up.
		"provider_model_name_folded": model.FoldModelName(providerModelName),
		"input_price":                inputPrice,
		"output_price":               outputPrice,
		"cache_write_price":          cacheWritePrice,
		"cache_read_price":           cacheReadPrice,
		"max_output":                 maxOutput,
		"updated_at":                 now,
	}
	if resetVerification {
		// Retargeting ends the auto-enable promise with the verification it
		// resets: the promise was made for the OLD mapping and nothing
		// re-enqueues the renamed row — carrying the flag forward would leave
		// an armed, untested row every poller watches while no queue owes it
		// anything. The new target still gets verified: an enable-requesting
		// edit probes (and enables) inline through the explicit commit mode,
		// and a re-import requeues — the latter is the only path that arms
		// afresh.
		updates["auto_enable_on_pass"] = false
	} else {
		// A same-name edit made through THIS binary knowingly preserves the
		// promise, so it re-aligns armed_at with the clock it just bumped —
		// without this, an innocent price edit while the probe is queued
		// would break the alignment and silently forfeit the enable. Writers
		// that mean disable clear the flag instead; writers too old to know
		// these columns leave the row misaligned, which is exactly the
		// conservative outcome their disable needs.
		updates["armed_at"] = gorm.Expr("CASE WHEN auto_enable_on_pass THEN ? ELSE armed_at END", now)
	}
	if priceChanged {
		updates["price_updated_at"] = now
	}
	if resetVerification {
		// The mapping test and capability probes validated the OLD
		// provider_model_name; a new name makes them stale, so clear them —
		// the candidate must be re-tested before it can route or be enabled
		// again. A map-based Updates writes these values (a struct-based one
		// would skip them).
		//
		// Capabilities clear to NULL ("not confirmed"), not false: nothing about
		// the old name's confirmation carries over to a new one, and false would
		// assert a decisive "not supported" that no probe established.
		updates["verification_status"] = model.ModelVerificationStatusUntested
		updates["supports_streaming"] = nil
		updates["supports_function_calling"] = nil
		// The last run's record goes with it: the stored outcome, duration,
		// timestamp and failure reason all describe the old target, and leaving
		// them would show the old name's error next to the new untested one —
		// while a surviving last_tested_at makes the fresh retarget read as
		// "probed but inconclusive" instead of "not probed yet".
		updates["last_test_result"] = nil
		updates["last_test_duration_ms"] = nil
		updates["last_tested_at"] = nil
		updates["last_test_error"] = nil
	}
	return db.Model(&model.ModelCandidate{}).Where("id = ?", id).Updates(updates).Error
}

// DemoteUnverifiedEnabledCandidate is the enabled-implies-verified backstop:
// it disables the row only while it STILL reads enabled-but-unverified, still
// points at the target the caller's edit stored (providerModelName), and its
// clock still matches the caller's snapshot (expectedUpdatedAt) — a competing
// probe that lands a pass in between keeps its result, and a LATER edit
// (even one storing the same name) that moved the row owns its own
// intermediate state, which this stale caller must not knock down. Reports
// whether the demote applied; the caller re-reads either way.
func DemoteUnverifiedEnabledCandidate(db *gorm.DB, id uint, providerModelName string, expectedUpdatedAt time.Time, now time.Time) (bool, error) {
	result := db.Model(&model.ModelCandidate{}).
		Where("id = ? AND provider_model_name = ? AND updated_at = ? AND management_status = ? AND verification_status <> ?",
			id, providerModelName, expectedUpdatedAt, model.ModelCandidateStatusEnabled, model.ModelVerificationStatusPassed).
		Updates(map[string]interface{}{"management_status": model.ModelCandidateStatusDisabled, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// FindLatestCandidatePrice returns the most recently PRICED candidate row for
// the given provider + provider_model_name, used to auto-suggest prices when
// adding a new candidate for the same model (prices follow the provider).
// Returns gorm.ErrRecordNotFound when no such candidate exists — the caller
// then falls back to the built-in seed catalog.
//
// Recency is price_updated_at, not updated_at. Enabling, disabling, retesting
// and probing all bump updated_at without touching a price, so ordering by it
// would let a retest on an old candidate promote its stale rate over a newer
// one. Ties break on id so the answer is deterministic rather than
// storage-order dependent.
//
// The name is matched case-insensitively, matching pricecatalog.Lookup: upstream
// model names are quoted inconsistently ("DeepSeek-V4-Pro" vs "deepseek-v4-pro")
// and a byte-exact match would miss the provider's own negotiated price and fall
// through to the catalog's generic figure — inverting the intended precedence.
// The comparison runs against the stored folded copy rather than a SQL LOWER()
// of the name, because LOWER() is not the same function on both supported
// backends and the same data would otherwise match on one and miss on the other.
// Both sides go through model.FoldModelName, and the predicate stays a plain
// equality the index can seek on — one row read, not a scan of the provider's
// whole catalogue on every keystroke in the candidate form.
//
// Only the columns needed to answer the question are selected; the row is a
// price carrier, not a full candidate, so everything else stays zero-valued and
// must not be relied on.
// FindLatestCandidatePricesByFoldedNames is the batch form of
// FindLatestCandidatePrice: one query for a whole import dialog instead of one
// per row. Keys of the returned map are folded names; a name with no history
// is simply absent. The recency rule is identical to the single look-up
// (price_updated_at DESC, id DESC): rows arrive in that order and the first
// row seen per folded name wins.
func FindLatestCandidatePricesByFoldedNames(db *gorm.DB, providerID uint, foldedNames []string) (map[string]model.ModelCandidate, error) {
	if len(foldedNames) == 0 {
		return map[string]model.ModelCandidate{}, nil
	}
	var rows []model.ModelCandidate
	err := db.
		Select("input_price", "output_price", "cache_write_price", "cache_read_price", "price_updated_at", "provider_model_name_folded").
		Where("provider_id = ? AND provider_model_name_folded IN ?", providerID, foldedNames).
		Order("price_updated_at DESC, id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	latest := make(map[string]model.ModelCandidate, len(rows))
	for _, r := range rows {
		if _, seen := latest[r.ProviderModelNameFolded]; !seen {
			latest[r.ProviderModelNameFolded] = r
		}
	}
	return latest, nil
}

func FindLatestCandidatePrice(db *gorm.DB, providerID uint, providerModelName string) (*model.ModelCandidate, error) {
	folded := model.FoldModelName(providerModelName)
	if folded == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var c model.ModelCandidate
	err := db.
		Select("input_price", "output_price", "cache_write_price", "cache_read_price", "price_updated_at").
		Where("provider_id = ? AND provider_model_name_folded = ?", providerID, folded).
		// Take rather than First: First appends its own ascending primary-key
		// ordering, which would fight the descending tie-breaker here.
		Order("price_updated_at DESC, id DESC").
		Take(&c).Error
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SetModelCandidateManagementStatusIfVerified CAS-guards enabling a candidate on
// verification_status still reading Passed — closing the check-then-act window
// between the service layer's gate check and the write. Unlike the explicit
// disable, it deliberately does NOT advance the probe token: an enable is not
// new evidence about the mapping, and advancing the token would discard an
// in-flight probe's verdict — a decisive failure would vanish and the row
// would keep routing as Passed+Enabled moments after being proven broken. A
// verdict landing after this enable is fresher evidence and must land; a
// failure flipping verification off Passed already stops the gateway routing
// to the row, whatever the toggle says.
func SetModelCandidateManagementStatusIfVerified(db *gorm.DB, id uint, status int, now time.Time) (bool, error) {
	result := db.Model(&model.ModelCandidate{}).
		Where("id = ? AND verification_status = ?", id, model.ModelVerificationStatusPassed).
		Updates(map[string]interface{}{"management_status": status, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// SetModelCandidateManagementStatusAdvancingProbeToken is the explicit-disable
// counterpart: the write may not change the stored value at all (disabling an
// already-disabled imported row), so a value-based guard cannot represent the
// admin's instruction — advancing the probe token is what makes an in-flight
// probe's commit (and its would-be auto-enable) miss. The internal
// reconciliation demote (DemoteUnverifiedEnabledCandidate) deliberately does
// NOT advance the token: it only ever changes the value, and it runs inside
// flows that still need their own probe's token to survive the reload.
func SetModelCandidateManagementStatusAdvancingProbeToken(db *gorm.DB, id uint, status int, probeRunID string, now time.Time) error {
	return db.Model(&model.ModelCandidate{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"management_status": status,
			"last_probe_run_id": probeRunID,
			// An explicit disable revokes the import's standing auto-enable
			// promise: a probe still queued (or in flight) for this row must
			// deliver its verdict without enabling.
			"auto_enable_on_pass": false,
			"updated_at":          now,
		}).Error
}

// ListUnprobedCandidateIDs returns every mapping the queue still OWES a probe
// — untested, no attempt stamp, and carrying the auto-enable promise. The
// probe queue is in-memory and dies with the process, so these rows ARE the
// durable queue: re-enqueueing them at startup turns every way a queued probe
// can be lost (a crash, a shutdown racing an import's enqueue) into a
// self-healing restart instead of a manual re-import. The armed flag is what
// separates them from a manually created candidate saved as disabled: that
// row is untested and unstamped by deliberate choice (store WITHOUT touching
// the upstream), and only import and requeue — the flows that promise a probe
// — arm rows.
// Two shapes are owed something: an untested unstamped armed row is owed a
// PROBE, and a Passed but disabled armed row is owed its ENABLE (the fulfill
// path delivers that one without spending a probe).
func ListUnprobedCandidateIDs(db *gorm.DB) ([]uint, error) {
	var ids []uint
	err := db.Model(&model.ModelCandidate{}).
		Where("auto_enable_on_pass AND ((verification_status = ? AND last_tested_at IS NULL) OR (verification_status = ? AND management_status = ?))",
			model.ModelVerificationStatusUntested, model.ModelVerificationStatusPassed, model.ModelCandidateStatusDisabled).
		Order("id ASC").
		Pluck("id", &ids).Error
	return ids, err
}

// ListCandidateIDsByProbeRunID reports which of the given rows carry the
// given probe token — the requeue flow's way to learn which rows its
// conditional re-arm ACTUALLY hit: a row a concurrent probe settled between
// the caller's read and the UPDATE keeps its own token, and reporting it as
// requeued would enqueue a probe for a row that is done (and whose enable
// promise was never renewed).
func ListCandidateIDsByProbeRunID(db *gorm.DB, ids []uint, probeRunID string) ([]uint, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var hit []uint
	err := db.Model(&model.ModelCandidate{}).
		Where("id IN ? AND last_probe_run_id = ?", ids, probeRunID).
		Order("id ASC").
		Pluck("id", &hit).Error
	return hit, err
}

// FulfillArmedEnableIfVerified settles a Passed row that still carries the
// auto-enable promise: an ALIGNED row (untouched since arming) gets the
// enable it is owed without spending another probe, and either way the flag
// is cleared — a misaligned row was touched since arming by a writer that may
// have been an old binary's explicit disable, so its promise is revoked, not
// deferred. Preconditions are re-checked in the statement. The probe token is
// deliberately NOT advanced — this is an enable, not evidence, and advancing
// would discard an in-flight probe's verdict.
func FulfillArmedEnableIfVerified(db *gorm.DB, id uint, now time.Time) (bool, error) {
	result := db.Model(&model.ModelCandidate{}).
		Where("id = ? AND verification_status = ? AND auto_enable_on_pass", id, model.ModelVerificationStatusPassed).
		Updates(map[string]interface{}{
			"management_status": gorm.Expr(
				"CASE WHEN armed_at IS NOT NULL AND updated_at = armed_at THEN ? ELSE management_status END",
				model.ModelCandidateStatusEnabled,
			),
			"auto_enable_on_pass": false,
			"updated_at":          now,
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// ClearCandidatesProbeResidue resets the last-attempt columns on mappings
// being requeued for a fresh probe, so the rows read "waiting for a probe"
// everywhere — including on instances that cannot see the enqueueing process's
// in-memory queue. The requeue supersedes the old attempt; keeping its stamp
// would let a poller on another instance mistake the requeued row for a
// settled inconclusive one and stop watching mid-probe. Verification and
// management status are untouched.
//
// The predicate re-checks Untested — the state the caller selected these ids
// by — because a concurrent probe can land a decisive verdict between that
// read and this write. Wiping such a row's fresh record would leave a settled
// mapping with no probe history, and (for a pass) nothing would ever restore
// it: the worker skips passed rows.
func ClearCandidatesProbeResidue(db *gorm.DB, ids []uint, probeRunID string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return db.Model(&model.ModelCandidate{}).
		Where("id IN ? AND verification_status = ?", ids, model.ModelVerificationStatusUntested).
		Updates(map[string]interface{}{
			"last_test_result":      nil,
			"last_test_duration_ms": nil,
			"last_tested_at":        nil,
			"last_test_error":       nil,
			// A requeue renews the import's auto-enable promise: the admin
			// explicitly asked for these rows to be probed again. Arming and
			// the row clock are written as the SAME value — that alignment is
			// what the probe commit's enable checks, so this write both
			// renews the promise and marks the row untouched-since-arming.
			"auto_enable_on_pass": true,
			"armed_at":            now,
			"updated_at":          now,
			// The requeue supersedes every probe already in flight: advancing
			// the token here makes their commits miss instead of landing a
			// verdict whose updated_at bump would break the alignment just
			// established — stranding the renewed promise as Passed+Disabled.
			// The requeued run re-reads the row and probes under this token.
			"last_probe_run_id": probeRunID,
		}).Error
}

// CandidateProbeCommit is what to persist after a full probe run (basic plus
// the two capability probes), written in one UPDATE so last_test_result and
// last_tested_at describe the run as a whole. Committing the three probes
// through separate calls would leave those shared columns holding whichever
// probe happened to finish last.
// Every verdict field is nil-means-leave-alone. A probe that did not run, or ran
// and came back inconclusive, is no evidence at all, and writing it would throw
// away a verdict that was previously earned. For verification_status that is not
// cosmetic — the gateway drops a candidate whose status is not Passed from ALL
// routing. Callers classify; this struct only carries what they decided is worth
// persisting.
type CandidateProbeCommit struct {
	VerificationStatus      *int
	SupportsStreaming       *bool
	SupportsFunctionCalling *bool
	LastTestResult          *int
	DurationMs              int64
	// LastTestError is written only when WriteLastTestError is set — a probe
	// that ran overwrites the stored reason (nil on pass clears it), while a
	// probe that never happened leaves the previous reason in place.
	LastTestError      *string
	WriteLastTestError bool
	// EnableOnPass / EnableOnPassWhenArmed / DisableOnFail are the management
	// transitions this run may make, applied in the SAME statement as the
	// verdict. Splitting them into a follow-up write would open a window in
	// which another instance's poll sees "Passed · Disabled" and settles there
	// — with the enable landing moments later and nothing refreshing the view.
	//
	// EnableOnPass (explicit flows: retest, edit) takes effect only when this
	// commit's verdict is Passed AND management_status still equals
	// ExpectedManagementStatus (the value read before the probe): an operator
	// who disabled the row mid-probe keeps their disable.
	//
	// EnableOnPassWhenArmed (the background queue) instead keys the enable to
	// the row's auto_enable_on_pass flag AT COMMIT TIME: the flag is the
	// import's persisted promise, and an admin's explicit disable revokes it
	// by clearing the flag — which a value-based status check could never see
	// when the disable changes no stored value.
	//
	// DisableOnFail takes effect when this commit's verdict is decisive and
	// not Passed — an unverified row must not stay listed as serving traffic.
	EnableOnPass             bool
	EnableOnPassWhenArmed    bool
	ExpectedManagementStatus int
	DisableOnFail            bool
	// ExpectedRowUpdatedAt is the row's updated_at as the prober read it
	// before going upstream. Both enable transitions additionally require it
	// to still hold at commit time: updated_at is the ONE column every writer
	// of this row bumps — including binaries from before this schema existed,
	// whose explicit disable cannot clear auto_enable_on_pass or advance the
	// probe token (it predates both columns). Without this guard a
	// mixed-version rollout lets such a disable be silently overridden by a
	// passing probe's enable. Any row write during the probe therefore
	// forfeits the enable; the verdict still lands, and the operator enables
	// the verified row by hand.
	ExpectedRowUpdatedAt time.Time
}

// CommitModelCandidateProbeResults persists a whole probe run, but only if the
// candidate still points at probedModelName — the mapping the probe actually
// tested — and still carries the probe-run id the caller read before probing
// (expectedProbeRunID, "" for a row never probed). The run's own probeRunID is
// written with the verdict, becoming the next probe's expected baseline.
//
// The name guard is what keeps a slow probe from landing on a configuration it
// never examined: probing takes seconds of upstream round trips, so a concurrent
// edit can retarget the row in the meantime, and an id-only write would then
// stamp the new target with the old target's verdict. The run-id guard closes
// the same window against OTHER PROBES: the background queue and manual retests
// (on any instance) can overlap on one row, and whichever commits second must
// see its guard miss rather than overwrite the fresher verdict — ids, unlike
// timestamps, never collide or order ambiguously, and a writer can recognize
// its own already-applied write by reading the id back. applied is false when
// either guard misses, which callers must treat as "discard this result"
// rather than as an error — whatever moved the row owns the outcome.
//
// Management transitions ride in the same statement (see CandidateProbeCommit)
// so a pass and its auto-enable are one atomic write.
func CommitModelCandidateProbeResults(db *gorm.DB, id uint, probedModelName, expectedProbeRunID, probeRunID string, c CandidateProbeCommit, now time.Time) (bool, error) {
	updates := map[string]interface{}{
		"last_test_result":      c.LastTestResult,
		"last_test_duration_ms": c.DurationMs,
		"last_tested_at":        now,
		"last_probe_run_id":     probeRunID,
		"updated_at":            now,
	}
	if c.VerificationStatus != nil {
		updates["verification_status"] = *c.VerificationStatus
	}
	if c.SupportsStreaming != nil {
		updates["supports_streaming"] = *c.SupportsStreaming
	}
	if c.SupportsFunctionCalling != nil {
		updates["supports_function_calling"] = *c.SupportsFunctionCalling
	}
	if c.WriteLastTestError {
		updates["last_test_error"] = c.LastTestError
	}
	passed := c.VerificationStatus != nil && *c.VerificationStatus == model.ModelVerificationStatusPassed
	decisiveFail := c.VerificationStatus != nil && *c.VerificationStatus != model.ModelVerificationStatusPassed
	switch {
	case c.EnableOnPass && passed:
		// Conditional inside the row, not the predicate: a mid-probe status
		// flip must keep the verdict while losing only the enable. The
		// updated_at leg is the old-writer guard (see ExpectedRowUpdatedAt):
		// a value-preserving disable from a pre-schema binary moves neither
		// the status nor the token, only the row clock.
		updates["management_status"] = gorm.Expr(
			"CASE WHEN management_status = ? AND updated_at = ? THEN ? ELSE management_status END",
			c.ExpectedManagementStatus, c.ExpectedRowUpdatedAt, model.ModelCandidateStatusEnabled,
		)
	case c.EnableOnPassWhenArmed && passed:
		// The flag is read by the SAME statement (SET expressions see the
		// row's old values), so the enable reflects the promise as it stands
		// at write time — a disable that already revoked it wins, and the
		// verdict still lands either way. The alignment leg (updated_at still
		// equal to armed_at, both written together when the row was armed) is
		// the guard old binaries can trip: they cannot clear the flag, but
		// any write bumps updated_at — including one made BEFORE the worker
		// read its baseline, which a caller-supplied expectation could never
		// see.
		updates["management_status"] = gorm.Expr(
			"CASE WHEN auto_enable_on_pass AND armed_at IS NOT NULL AND updated_at = armed_at THEN ? ELSE management_status END",
			model.ModelCandidateStatusEnabled,
		)
	case c.DisableOnFail && decisiveFail:
		updates["management_status"] = model.ModelCandidateStatusDisabled
	}
	// How a pass settles the auto-enable promise depends on the commit mode.
	// The armed mode consumes it outright: an aligned row was just fulfilled,
	// and a MISALIGNED row was touched since arming by a writer that may have
	// been an old binary's explicit disable — the flag is revoked for good,
	// because leaving (or re-aligning) it would let the very next probe
	// reverse a disable that binary's admin already saw acknowledged.
	// Explicit-flow and verdict-only commits leave an unfulfilled promise
	// armed (their forfeits come from concurrent NEW-binary writes, which
	// clear the flag themselves when they mean disable) and consume it only
	// when the enable fired or the row is already on.
	// An armed-mode commit that did NOT pass must keep the promise usable
	// across rounds without erasing a revocation: a failing verdict bumps the
	// row clock, so an ALIGNED row re-aligns armed_at to the same write (the
	// worker's own verdict is not a revocation signal) — while a row that was
	// ALREADY misaligned was touched by someone else since arming, and the
	// promise is revoked rather than left as a dead flag a later write could
	// resurrect.
	if c.EnableOnPassWhenArmed && !passed {
		updates["auto_enable_on_pass"] = gorm.Expr(
			"CASE WHEN auto_enable_on_pass AND updated_at = armed_at THEN ? ELSE ? END", true, false,
		)
		updates["armed_at"] = gorm.Expr(
			"CASE WHEN auto_enable_on_pass AND updated_at = armed_at THEN ? ELSE armed_at END", now,
		)
	}
	if passed {
		alreadyEnabled := model.ModelCandidateStatusEnabled
		switch {
		case c.EnableOnPassWhenArmed:
			updates["auto_enable_on_pass"] = false
		case c.EnableOnPass:
			updates["auto_enable_on_pass"] = gorm.Expr(
				"CASE WHEN (management_status = ? AND updated_at = ?) OR management_status = ? THEN ? ELSE auto_enable_on_pass END",
				c.ExpectedManagementStatus, c.ExpectedRowUpdatedAt, alreadyEnabled, false,
			)
		default:
			// A verdict-only commit (a re-landed discarded verdict, a plain
			// confirmation) carries no enable of its own; the promise it could
			// not fulfill stays armed unless the row is already on.
			updates["auto_enable_on_pass"] = gorm.Expr(
				"CASE WHEN management_status = ? THEN ? ELSE auto_enable_on_pass END",
				alreadyEnabled, false,
			)
		}
	}
	result := db.Model(&model.ModelCandidate{}).
		Where("id = ? AND provider_model_name = ? AND last_probe_run_id = ?", id, probedModelName, expectedProbeRunID).
		Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// SwapModelCandidateSortOrder atomically swaps sort_order between
// candidateID and its immediate neighbor in the given direction, scoped to
// modelID (a candidate's route-chain position is only meaningful within
// its own model). Same intermediate-negative-value pattern
// as SwapProviderKeySortOrder to avoid momentarily violating
// UNIQUE(model_id, sort_order) mid-swap.
func SwapModelCandidateSortOrder(db *gorm.DB, modelID, candidateID uint, direction string) (bool, error) {
	var applied bool
	err := db.Transaction(func(tx *gorm.DB) error {
		var current model.ModelCandidate
		if err := tx.Select("id, sort_order").Where("id = ? AND model_id = ?", candidateID, modelID).First(&current).Error; err != nil {
			return err
		}

		var neighbor model.ModelCandidate
		var neighborErr error
		if direction == "up" {
			neighborErr = tx.Select("id, sort_order").Where("model_id = ? AND sort_order < ?", modelID, current.SortOrder).
				Order("sort_order DESC").Limit(1).First(&neighbor).Error
		} else {
			neighborErr = tx.Select("id, sort_order").Where("model_id = ? AND sort_order > ?", modelID, current.SortOrder).
				Order("sort_order ASC").Limit(1).First(&neighbor).Error
		}
		if errors.Is(neighborErr, gorm.ErrRecordNotFound) {
			applied = false
			return nil
		}
		if neighborErr != nil {
			return neighborErr
		}

		// UpdateColumn on purpose: reordering is pure presentation and must
		// not touch the row clock — Update would auto-bump updated_at, and
		// the auto-enable guard reads updated_at = armed_at, so reordering an
		// armed row mid-probe would silently forfeit its enable.
		const tempOrder = -1
		if err := tx.Model(&model.ModelCandidate{}).Where("id = ?", current.ID).UpdateColumn("sort_order", tempOrder).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.ModelCandidate{}).Where("id = ?", neighbor.ID).UpdateColumn("sort_order", current.SortOrder).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.ModelCandidate{}).Where("id = ?", current.ID).UpdateColumn("sort_order", neighbor.SortOrder).Error; err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func DeleteModelCandidate(db *gorm.DB, id uint) error {
	return db.Where("id = ?", id).Delete(&model.ModelCandidate{}).Error
}

// ListModelCandidatesByProviderID returns every candidate on one provider,
// across all models and management states — the reference list an impact
// preview starts from when the provider is about to be disabled.
func ListModelCandidatesByProviderID(db *gorm.DB, providerID uint) ([]model.ModelCandidate, error) {
	var candidates []model.ModelCandidate
	if err := db.Where("provider_id = ?", providerID).Order("model_id ASC, sort_order ASC").Find(&candidates).Error; err != nil {
		return nil, err
	}
	return candidates, nil
}

// ListModelsByIDs batch-fetches models by id (the same N+1 fix as
// ListModelCandidatesByModelIDs). Empty input returns nil without querying.
func ListModelsByIDs(db *gorm.DB, ids []uint) ([]model.Model, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var models []model.Model
	if err := db.Where("id IN ?", ids).Order("id ASC").Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}
