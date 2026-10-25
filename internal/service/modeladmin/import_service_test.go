package modeladmin_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// Upstream catalogs routinely use slash-namespaced model ids (Qwen/Qwen3-...),
// and bulk import keeps the external name equal to the upstream name, so the
// model-name rules must accept them.
func TestCreateModelAcceptsSlashedName(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "Qwen/Qwen3-235B-A22B"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("expected a slash-namespaced model name to be accepted, got %v", err)
	}
	if view.Name != "Qwen/Qwen3-235B-A22B" {
		t.Fatalf("expected the name stored verbatim, got %q", view.Name)
	}
}

func seedEnabledProvider(t *testing.T, db *gorm.DB, name string) *model.Provider {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{
		Name: name, ProviderType: "openai", BaseURL: "https://example.invalid",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed provider failed: %v", err)
	}
	return p
}

func TestImportProviderModelsCreatesModelAndDisabledCandidate(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")

	cacheWrite := 0.5
	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "deepseek-ai/DeepSeek-V4", InputPrice: ptrFloat(2), OutputPrice: ptrFloat(8), CacheWritePrice: &cacheWrite, MaxOutput: 8192},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 item result, got %+v", result)
	}
	item := result.Items[0]
	if item.Status != modeladmin.ImportStatusCreated {
		t.Fatalf("expected status created, got %+v", item)
	}
	if item.ModelID == 0 || item.CandidateID == 0 {
		t.Fatalf("expected model and candidate ids in the result, got %+v", item)
	}

	var m model.Model
	if err := db.First(&m, item.ModelID).Error; err != nil {
		t.Fatalf("imported model not stored: %v", err)
	}
	if m.Name != "deepseek-ai/DeepSeek-V4" || m.ManagementStatus != model.ModelStatusEnabled {
		t.Fatalf("expected an enabled model named after the upstream id, got %+v", m)
	}

	var c model.ModelCandidate
	if err := db.First(&c, item.CandidateID).Error; err != nil {
		t.Fatalf("imported candidate not stored: %v", err)
	}
	if c.ModelID != m.ID || c.ProviderID != p.ID || c.ProviderModelName != "deepseek-ai/DeepSeek-V4" {
		t.Fatalf("candidate not linked to the imported model/provider: %+v", c)
	}
	if c.InputPrice != 2 || c.OutputPrice != 8 || c.CacheWritePrice == nil || *c.CacheWritePrice != 0.5 || c.CacheReadPrice != nil || c.MaxOutput != 8192 {
		t.Fatalf("candidate prices not stored as submitted: %+v", c)
	}
	// Disabled + untested is what keeps the row out of routing until the probe
	// queue verifies it: the gateway only routes enabled candidates whose
	// verification has passed.
	if c.ManagementStatus != model.ModelCandidateStatusDisabled || c.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected a disabled+untested candidate, got status=%d verification=%d", c.ManagementStatus, c.VerificationStatus)
	}
}

func TestImportProviderModelsAppendsCandidateToExistingModel(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	pA := seedEnabledProvider(t, db, "prov-a")
	pB := seedEnabledProvider(t, db, "prov-b")

	// The model already exists and is mapped to provider A; importing the same
	// name for provider B must append a second candidate, not create a model.
	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "glm-5"}, now)
	if err != nil {
		t.Fatalf("seed CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: pA.ID, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
	}, now); err != nil {
		t.Fatalf("seed CreateModelCandidate failed: %v", err)
	}

	result, err := svc.ImportProviderModels(pB.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "glm-5", InputPrice: ptrFloat(3), OutputPrice: ptrFloat(12)},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if result.Items[0].Status != modeladmin.ImportStatusAppended {
		t.Fatalf("expected status appended, got %+v", result.Items[0])
	}
	if result.Appended != 1 || result.Created != 0 || result.Skipped != 0 {
		t.Fatalf("summary counts must mirror the per-item statuses, got %+v", result)
	}
	if result.Items[0].ModelID != view.ID {
		t.Fatalf("expected the existing model to be reused, got %+v", result.Items[0])
	}
	var count int64
	if err := db.Model(&model.Model{}).Where("name = ?", "glm-5").Count(&count).Error; err != nil {
		t.Fatalf("count models failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one glm-5 model row, got %d", count)
	}
	var candidates int64
	if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", view.ID).Count(&candidates).Error; err != nil {
		t.Fatalf("count candidates failed: %v", err)
	}
	if candidates != 2 {
		t.Fatalf("expected the model to end up with two candidates, got %d", candidates)
	}
}

func TestImportProviderModelsSkipsExistingMappingInvalidNameAndInBatchDuplicate(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "already-mapped"}, now)
	if err != nil {
		t.Fatalf("seed CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p.ID, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
	}, now); err != nil {
		t.Fatalf("seed CreateModelCandidate failed: %v", err)
	}

	// The 151-char name pins the public-model length limit as a PER-ITEM skip:
	// bulk import publishes the upstream id as the model name (capped at 100),
	// and an over-long id must not fail the whole batch.
	overlong := strings.Repeat("x", 151)
	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "already-mapped", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
		{ProviderModelName: "bad name!", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
		{ProviderModelName: "fresh-one", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
		{ProviderModelName: "fresh-one", InputPrice: ptrFloat(9), OutputPrice: ptrFloat(9)},
		{ProviderModelName: overlong, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	statuses := make([]string, 0, len(result.Items))
	reasons := make([]string, 0, len(result.Items))
	for _, it := range result.Items {
		statuses = append(statuses, it.Status)
		reasons = append(reasons, it.Reason)
	}
	want := []string{
		modeladmin.ImportStatusSkipped, modeladmin.ImportStatusSkipped,
		modeladmin.ImportStatusCreated, modeladmin.ImportStatusSkipped,
		modeladmin.ImportStatusSkipped,
	}
	for i, w := range want {
		if statuses[i] != w {
			t.Fatalf("item %d: want status %q, got %q (%+v)", i, w, statuses[i], result.Items)
		}
	}
	if reasons[0] != modeladmin.BatchSkipReasonExists {
		t.Fatalf("existing mapping must skip as exists, got %q", reasons[0])
	}
	if reasons[1] != modeladmin.BatchSkipReasonInvalid {
		t.Fatalf("invalid name must skip as invalid, got %q", reasons[1])
	}
	if reasons[3] != modeladmin.BatchSkipReasonExists {
		t.Fatalf("in-batch duplicate must skip as exists, got %q", reasons[3])
	}
	if reasons[4] != modeladmin.BatchSkipReasonInvalid {
		t.Fatalf("an over-long name must skip as invalid per item, got %q", reasons[4])
	}
	if result.Created != 1 || result.Appended != 0 || result.Skipped != 4 {
		t.Fatalf("summary counts must mirror the per-item statuses, got created=%d appended=%d skipped=%d",
			result.Created, result.Appended, result.Skipped)
	}
}

// Re-importing a name whose mapping exists but was never decisively probed
// must surface that mapping's id so the caller can requeue it — this is the
// promised recovery path for probes lost to a restart (the queue is not
// durable; the candidate rows are the state of record). A mapping that already
// holds a verdict is genuinely done and stays id-less, so nothing re-probes it
// behind the operator's back.
func TestImportProviderModelsReturnsUnfinishedMappingIDsForRequeue(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")

	// One unfinished (untested) mapping, one settled (passed+enabled) mapping.
	unfinishedModel, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "unfinished-model"}, now)
	if err != nil {
		t.Fatalf("seed CreateModel failed: %v", err)
	}
	unfinished, err := svc.CreateModelCandidate(context.Background(), unfinishedModel.ID, modeladmin.CreateCandidateInput{
		ProviderID: p.ID, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
	}, now)
	if err != nil {
		t.Fatalf("seed unfinished candidate failed: %v", err)
	}
	// The unfinished mapping carries the residue of an earlier inconclusive
	// attempt. The requeue must clear it: last_tested_at is what pollers on
	// OTHER instances (which cannot see this instance's in-memory queue) use
	// to tell "attempted, settled" from "waiting for its probe" — leaving the
	// stale stamp would make them declare the requeued row done mid-probe.
	staleDetail := "HTTP 429: rate limited"
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", unfinished.ID).
		Updates(map[string]interface{}{"last_tested_at": now.Add(-time.Hour), "last_test_error": staleDetail}).Error; err != nil {
		t.Fatalf("seed stale attempt residue failed: %v", err)
	}
	settledModel, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "settled-model"}, now)
	if err != nil {
		t.Fatalf("seed CreateModel failed: %v", err)
	}
	settled, err := svc.CreateModelCandidate(context.Background(), settledModel.ID, modeladmin.CreateCandidateInput{
		ProviderID: p.ID, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
	}, now)
	if err != nil {
		t.Fatalf("seed settled candidate failed: %v", err)
	}
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", settled.ID).
		Update("verification_status", model.ModelVerificationStatusPassed).Error; err != nil {
		t.Fatalf("seed settled verdict failed: %v", err)
	}

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "unfinished-model", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
		{ProviderModelName: "settled-model", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if result.Items[0].Status != modeladmin.ImportStatusSkipped || result.Items[0].CandidateID != unfinished.ID {
		t.Fatalf("expected the unfinished mapping to skip WITH its candidate id %d for requeue, got %+v", unfinished.ID, result.Items[0])
	}
	if result.Items[1].Status != modeladmin.ImportStatusSkipped || result.Items[1].CandidateID != 0 {
		t.Fatalf("expected the settled mapping to skip WITHOUT a candidate id, got %+v", result.Items[1])
	}

	var requeued model.ModelCandidate
	if err := db.First(&requeued, unfinished.ID).Error; err != nil {
		t.Fatalf("load requeued candidate: %v", err)
	}
	if requeued.LastTestedAt != nil || requeued.LastTestError != nil {
		t.Fatalf("expected the requeue to clear the stale attempt residue, got tested_at=%v error=%v",
			requeued.LastTestedAt, requeued.LastTestError)
	}
}

// Two writers can read the same next sort_order for one model and the loser
// hits UNIQUE(model_id, sort_order); the import must re-run the batch instead
// of surfacing a raw constraint error for a self-healing race.
func TestImportProviderModelsRetriesWhenSortOrderRaceLosesOnce(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")

	fired := false
	if err := db.Callback().Create().Before("gorm:create").Register("sort_order_race_once", func(tx *gorm.DB) {
		if destIsCandidateInsert(tx.Statement.Dest) && !fired {
			fired = true
			_ = tx.AddError(errors.New("UNIQUE constraint failed: model_candidates.model_id, model_candidates.sort_order"))
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "raced-model", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
	}, now)
	if err != nil {
		t.Fatalf("expected the import to retry past a one-off sort_order race, got %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Status != modeladmin.ImportStatusCreated {
		t.Fatalf("expected the retried batch to succeed, got %+v", result)
	}
}

func TestImportProviderModelsProviderNotFound(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.ImportProviderModels(9999, []modeladmin.ImportModelItem{
		{ProviderModelName: "some-model", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
	}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

// destModelNames extracts the names of models being inserted regardless of
// whether the insert is a single row or a bulk statement — the import path is
// free to use either shape.
func destModelNames(dest any) []string {
	switch v := dest.(type) {
	case *model.Model:
		return []string{v.Name}
	case []*model.Model:
		out := make([]string, 0, len(v))
		for _, m := range v {
			out = append(out, m.Name)
		}
		return out
	case *[]*model.Model:
		return destModelNames(*v)
	}
	return nil
}

// destIsCandidateInsert reports whether the statement inserts candidates,
// single-row or bulk.
func destIsCandidateInsert(dest any) bool {
	switch dest.(type) {
	case *model.ModelCandidate, []*model.ModelCandidate, *[]*model.ModelCandidate:
		return true
	}
	return false
}

// A storage failure part-way through must roll back the whole import: a batch
// half-committed behind a total-failure response would make the retry report
// those rows as already existing.
func TestImportProviderModelsRollsBackOnMidBatchFailure(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")

	if err := db.Callback().Create().Before("gorm:create").Register("fail_on_boom_import", func(tx *gorm.DB) {
		for _, name := range destModelNames(tx.Statement.Dest) {
			if name == "boom" {
				_ = tx.AddError(errors.New("injected storage failure"))
			}
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	_, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "alpha", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
		{ProviderModelName: "boom", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
	}, now)
	if err == nil {
		t.Fatal("expected the injected storage failure to surface")
	}
	var count int64
	if err := db.Model(&model.Model{}).Where("name = ?", "alpha").Count(&count).Error; err != nil {
		t.Fatalf("count models failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected alpha rolled back with the failed batch, still stored %d row(s)", count)
	}
}

func TestSuggestCandidatePricesBatchReturnsHistoryAndEmpty(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedProviderWithBaseURL(t, providerService, "deepseek", catalogSeededHost)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	now := time.Now().UTC()
	// History for the name the catalog ALSO carries, priced differently —
	// proving history outranks the catalog in the batch path too. (The
	// catalog-passthrough leg lives in the single-suggestion and handler
	// tests; deepseek's catalog row shrank to one model, so a three-way
	// batch of distinct names no longer exists on this host.)
	historyName := catalogSeededModel(t)
	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: historyName}, now)
	if err != nil {
		t.Fatalf("seed CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: prov.ID, InputPrice: ptrFloat(42), OutputPrice: ptrFloat(43),
	}, now); err != nil {
		t.Fatalf("seed CreateModelCandidate failed: %v", err)
	}

	got, err := svc.SuggestCandidatePrices(prov.ID, []string{historyName, "no-such-model"})
	if err != nil {
		t.Fatalf("SuggestCandidatePrices failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected one entry per requested name, got %+v", got)
	}
	if got[historyName].Source != "history" || got[historyName].InputPrice != 42 {
		t.Fatalf("expected the provider's own price to outrank the catalog, got %+v", got[historyName])
	}
	if got["no-such-model"].Source != "" {
		t.Fatalf("a miss must come back with an empty source, got %+v", got["no-such-model"])
	}
}

func TestSuggestCandidatePricesProviderNotFound(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.SuggestCandidatePrices(9999, []string{"whatever"})
	if !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

// Two concurrent imports inserting overlapping new names in opposite orders
// can deadlock on PostgreSQL; the aborted transaction rolled back cleanly, so
// the import must re-run rather than surface a 500.
func TestImportProviderModelsRetriesAfterDeadlockAbort(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")

	fired := false
	if err := db.Callback().Create().Before("gorm:create").Register("deadlock_once", func(tx *gorm.DB) {
		if !fired {
			fired = true
			_ = tx.AddError(errors.New("ERROR: deadlock detected (SQLSTATE 40P01)"))
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "deadlocked-model", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
	}, now)
	if err != nil {
		t.Fatalf("expected the import to retry past a one-off deadlock abort, got %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Status != modeladmin.ImportStatusCreated {
		t.Fatalf("expected the retried batch to succeed, got %+v", result)
	}
}

// Two stored prices for the same provider+upstream name must resolve to the
// most recently repriced one — the same recency rule the single-name look-up
// applies — regardless of how the batch path fetches history.
func TestSuggestCandidatePricesReturnsLatestOfSeveralHistories(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	p := seedEnabledProvider(t, db, "prov-a")
	earlier := time.Now().UTC().Add(-time.Hour)
	later := time.Now().UTC()

	for i, seed := range []struct {
		model string
		price float64
		at    time.Time
	}{
		{"model-old", 10, earlier},
		{"model-new", 20, later},
	} {
		view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: seed.model}, seed.at)
		if err != nil {
			t.Fatalf("seed CreateModel %d failed: %v", i, err)
		}
		if _, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
			ProviderID: p.ID, ProviderModelName: "shared-upstream-name", InputPrice: ptrFloat(seed.price), OutputPrice: ptrFloat(seed.price),
		}, seed.at); err != nil {
			t.Fatalf("seed CreateModelCandidate %d failed: %v", i, err)
		}
	}

	got, err := svc.SuggestCandidatePrices(p.ID, []string{"shared-upstream-name"})
	if err != nil {
		t.Fatalf("SuggestCandidatePrices failed: %v", err)
	}
	if got["shared-upstream-name"].Source != "history" || got["shared-upstream-name"].InputPrice != 20 {
		t.Fatalf("expected the most recently repriced history (20), got %+v", got["shared-upstream-name"])
	}
}

// Slashes are separators inside a namespaced id, not free characters: real
// upstream ids never begin or end with one, and the discovery route's
// slash-trimming would make such names unretrievable or collide with the
// trimmed form of another model.
func TestModelNameRejectsBoundaryAndDoubleSlashes(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	for _, bad := range []string{"/foo", "foo/", "foo//bar", "/"} {
		if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: bad}, now); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}

	p := seedEnabledProvider(t, db, "prov-a")
	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "foo/", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if result.Items[0].Status != modeladmin.ImportStatusSkipped || result.Items[0].Reason != modeladmin.BatchSkipReasonInvalid {
		t.Fatalf("expected a boundary-slash name to skip as invalid, got %+v", result.Items[0])
	}
}

// A maximum-sized import must survive SQLite's bind-variable limit: one giant
// multi-row INSERT for 2,000 candidates exceeds it, so inserts have to run in
// bounded chunks (still inside one transaction).
func TestImportProviderModelsHandlesMaxSizedBatch(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")

	items := make([]modeladmin.ImportModelItem, 2000)
	for i := range items {
		items[i] = modeladmin.ImportModelItem{ProviderModelName: fmt.Sprintf("vendor/model-%04d", i), InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)}
	}
	result, err := svc.ImportProviderModels(p.ID, items, now)
	if err != nil {
		t.Fatalf("expected a 2000-item import to succeed, got %v", err)
	}
	if result.Created != 2000 {
		t.Fatalf("expected 2000 created, got %+v", result.Created)
	}
	var count int64
	if err := db.Model(&model.ModelCandidate{}).Where("provider_id = ?", p.ID).Count(&count).Error; err != nil {
		t.Fatalf("count candidates failed: %v", err)
	}
	if count != 2000 {
		t.Fatalf("expected 2000 stored candidates, got %d", count)
	}
}

// ---- capability declarations ----

func ptrBool(v bool) *bool { return &v }

// seedModelRow inserts a model row straight into the store, stamped at the
// given time so a test can prove "nothing was written" by the stamp
// surviving an import untouched. mutate shapes the declarations under test.
func seedModelRow(t *testing.T, db *gorm.DB, name string, mutate func(*model.Model), at time.Time) *model.Model {
	t.Helper()
	m := &model.Model{
		Name:             name,
		ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode:   model.ModelSchedulingModeFailover,
		OutputModalities: model.DefaultOutputModalitiesJSON,
		CreatedAt:        at, UpdatedAt: at,
	}
	if mutate != nil {
		mutate(m)
	}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model %s: %v", name, err)
	}
	return m
}

// seedCandidateRow inserts a candidate row directly, untested like every row
// before its first probe, with the management status the cross-entity rules
// read.
func seedCandidateRow(t *testing.T, db *gorm.DB, modelID, providerID uint, name string, maxOutput int, enabled bool) uint {
	t.Helper()
	now := time.Now().UTC()
	status := model.ModelCandidateStatusDisabled
	if enabled {
		status = model.ModelCandidateStatusEnabled
	}
	c := &model.ModelCandidate{
		ModelID: modelID, ProviderID: providerID, ProviderModelName: name,
		MaxOutput: maxOutput, ManagementStatus: status,
		VerificationStatus: model.ModelVerificationStatusUntested,
		CreatedAt:          now, UpdatedAt: now, PriceUpdatedAt: now,
	}
	if err := db.Create(c).Error; err != nil {
		t.Fatalf("seed candidate %s: %v", name, err)
	}
	return c.ID
}

// assertCapabilityRow compares one stored model row's capability columns
// against the expected shape, nil-pointer-aware on every tri-state.
func assertCapabilityRow(t *testing.T, got, want model.Model) {
	t.Helper()
	if !reflect.DeepEqual(got.ContextLength, want.ContextLength) {
		t.Errorf("context_length = %v, want %v", got.ContextLength, want.ContextLength)
	}
	//nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
	if !reflect.DeepEqual(got.SupportsImageInput, want.SupportsImageInput) { //nolint:staticcheck // SA1019: mirror assertion
		t.Errorf("supports_image_input = %v, want %v", got.SupportsImageInput, want.SupportsImageInput) //nolint:staticcheck // SA1019: mirror assertion
	}
	if got.InputModalities != want.InputModalities {
		t.Errorf("input_modalities = %q, want %q", got.InputModalities, want.InputModalities)
	}
	if !reflect.DeepEqual(got.SupportsTools, want.SupportsTools) {
		t.Errorf("supports_tools = %v, want %v", got.SupportsTools, want.SupportsTools)
	}
	if !reflect.DeepEqual(got.SupportsReasoning, want.SupportsReasoning) {
		t.Errorf("supports_reasoning = %v, want %v", got.SupportsReasoning, want.SupportsReasoning)
	}
	if !reflect.DeepEqual(got.Temperature, want.Temperature) {
		t.Errorf("temperature = %v, want %v", got.Temperature, want.Temperature)
	}
}

// The capability declarations a row carries must land on the model row it
// creates — the five slots verbatim, the input ids folded onto the
// vocabulary, and the deprecated image-input mirror derived from the input
// array in the same insert. Absent fields leave a created model undeclared,
// and an omitted token price still stores the zero the columns carry.
func TestImportProviderModelsStoresCapabilityDeclarations(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-cap-store")

	tests := []struct {
		name           string
		item           modeladmin.ImportModelItem
		want           model.Model
		zeroTokenSlots bool // the row submits no token prices or max_output
	}{
		{
			name: "full declaration lands with its input mirror",
			item: modeladmin.ImportModelItem{
				ProviderModelName: "upstream/cap-full", ModelName: ptrString("cap-full"),
				ContextLength: ptrInt(128000), SupportsTools: ptrString("yes"),
				SupportsReasoning: ptrString("no"), Temperature: ptrFloat(0.705),
				InputModalities: []string{"text", "image"},
				InputPrice:      ptrFloat(2), OutputPrice: ptrFloat(8), MaxOutput: 4096,
			},
			want: model.Model{
				ContextLength: ptrInt(128000), SupportsTools: ptrBool(true),
				SupportsReasoning: ptrBool(false), Temperature: ptrFloat(0.705),
				InputModalities: `["text","image"]`, SupportsImageInput: ptrBool(true), //nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
			},
		},
		{
			name: "text-only input list mirrors false",
			item: modeladmin.ImportModelItem{
				ProviderModelName: "upstream/cap-text", ModelName: ptrString("cap-text"),
				InputModalities: []string{"text"},
			},
			want:           model.Model{InputModalities: `["text"]`, SupportsImageInput: ptrBool(false)}, //nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
			zeroTokenSlots: true,
		},
		{
			name:           "absent declarations leave the model undeclared",
			item:           modeladmin.ImportModelItem{ProviderModelName: "cap-bare"},
			want:           model.Model{},
			zeroTokenSlots: true,
		},
		{
			name: "input ids fold onto the vocabulary",
			item: modeladmin.ImportModelItem{
				ProviderModelName: "upstream/cap-folded", ModelName: ptrString("cap-folded"),
				InputModalities: []string{"TEXT", "Image"},
			},
			want:           model.Model{InputModalities: `["text","image"]`, SupportsImageInput: ptrBool(true)}, //nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
			zeroTokenSlots: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{tc.item}, now)
			if err != nil {
				t.Fatalf("ImportProviderModels failed: %v", err)
			}
			if len(result.Items) != 1 || result.Items[0].Status != modeladmin.ImportStatusCreated {
				t.Fatalf("expected the row created, got %+v", result.Items)
			}
			var m model.Model
			if err := db.First(&m, result.Items[0].ModelID).Error; err != nil {
				t.Fatalf("load model: %v", err)
			}
			assertCapabilityRow(t, m, tc.want)

			var c model.ModelCandidate
			if err := db.First(&c, result.Items[0].CandidateID).Error; err != nil {
				t.Fatalf("load candidate: %v", err)
			}
			if c.ProviderModelName != tc.item.ProviderModelName {
				t.Errorf("candidate upstream name = %q, want the submitted %q", c.ProviderModelName, tc.item.ProviderModelName)
			}
			if tc.zeroTokenSlots && (c.InputPrice != 0 || c.OutputPrice != 0) {
				t.Errorf("unsubmitted token prices must store as zero, got %v/%v", c.InputPrice, c.OutputPrice)
			}
		})
	}

	// The fully-declared row's submitted prices survive verbatim.
	var full model.ModelCandidate
	if err := db.Where("provider_model_name = ?", "upstream/cap-full").First(&full).Error; err != nil {
		t.Fatalf("load full candidate: %v", err)
	}
	if full.InputPrice != 2 || full.OutputPrice != 8 || full.MaxOutput != 4096 {
		t.Fatalf("submitted prices/max_output not stored verbatim: %+v", full)
	}
}

// Every value domain the import validates is row-level: an out-of-range
// price, max_output, temperature, context, tri-state spelling or target name
// skips just its own row and the rest of the batch proceeds — the whole-batch
// rejection stays reserved for transport-shaped breakage at the binding.
func TestImportProviderModelsSkipsInvalidCapabilityValues(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-cap-invalid")

	overCap := strings.Repeat("x", 101)
	tests := []struct {
		name string
		item modeladmin.ImportModelItem
	}{
		{"negative input price", modeladmin.ImportModelItem{ProviderModelName: "bad-price", InputPrice: ptrFloat(-0.5)}},
		{"negative output price", modeladmin.ImportModelItem{ProviderModelName: "bad-output", OutputPrice: ptrFloat(-1)}},
		{"negative cache write price", modeladmin.ImportModelItem{ProviderModelName: "bad-cachew", CacheWritePrice: ptrFloat(-1)}},
		{"negative cache read price", modeladmin.ImportModelItem{ProviderModelName: "bad-cacher", CacheReadPrice: ptrFloat(-1)}},
		{"negative audio unit price", modeladmin.ImportModelItem{ProviderModelName: "bad-audiop", AudioUnitPrice: ptrFloat(-1)}},
		{"negative max output", modeladmin.ImportModelItem{ProviderModelName: "bad-maxout", MaxOutput: -1}},
		{"temperature above the range", modeladmin.ImportModelItem{ProviderModelName: "bad-temp-hi", Temperature: ptrFloat(3)}},
		{"temperature below the range", modeladmin.ImportModelItem{ProviderModelName: "bad-temp-lo", Temperature: ptrFloat(-0.1)}},
		{"zero context", modeladmin.ImportModelItem{ProviderModelName: "bad-ctx-zero", ContextLength: ptrInt(0)}},
		{"negative context", modeladmin.ImportModelItem{ProviderModelName: "bad-ctx-neg", ContextLength: ptrInt(-5)}},
		{"unknown tri-state spelling", modeladmin.ImportModelItem{ProviderModelName: "bad-tri-maybe", SupportsTools: ptrString("maybe")}},
		{"unknown clears nothing on import", modeladmin.ImportModelItem{ProviderModelName: "bad-tri-unknown", SupportsReasoning: ptrString("unknown")}},
		{"target name over the length cap", modeladmin.ImportModelItem{ProviderModelName: "upstream/long", ModelName: ptrString(overCap)}},
		{"target name with invalid characters", modeladmin.ImportModelItem{ProviderModelName: "upstream/chars", ModelName: ptrString("bad name!")}},
		{"unknown input modality", modeladmin.ImportModelItem{ProviderModelName: "bad-inputmod", InputModalities: []string{"warp"}}},
		{"duplicate input modality", modeladmin.ImportModelItem{ProviderModelName: "bad-inputmod-dup", InputModalities: []string{"text", "text"}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			healthy := modeladmin.ImportModelItem{ProviderModelName: tc.item.ProviderModelName + "-healthy"}
			result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{tc.item, healthy}, now)
			if err != nil {
				t.Fatalf("ImportProviderModels failed: %v", err)
			}
			if result.Items[0].Status != modeladmin.ImportStatusSkipped || result.Items[0].Reason != modeladmin.BatchSkipReasonInvalid {
				t.Fatalf("expected the bad row skipped as invalid, got %+v", result.Items[0])
			}
			if result.Items[1].Status != modeladmin.ImportStatusCreated {
				t.Fatalf("expected the healthy row created, got %+v", result.Items[1])
			}
		})
	}

	var count int64
	if err := db.Model(&model.ModelCandidate{}).Where("provider_id = ?", p.ID).Count(&count).Error; err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if count != int64(len(tests)) {
		t.Fatalf("expected exactly one stored candidate per healthy row (%d), got %d", len(tests), count)
	}
}

// The existing-model capability adjudication, row-atomic: an absent field
// touches nothing, a declaration fills an undeclared slot (also when the
// row's mapping is already stored), an agreeing declaration is the same
// no-op an absent field is, and any contradicting declaration refuses the
// whole row with zero writes — the fill it carried for other slots included,
// and no requeue id either.
func TestImportProviderModelsCapabilityAdjudication(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	p := seedEnabledProvider(t, db, "prov-cap-adjudication")
	seedTime := time.Now().UTC().Add(-2 * time.Hour)
	importTime := time.Now().UTC()

	single := func(item modeladmin.ImportModelItem) modeladmin.ImportProviderModelsResult {
		t.Helper()
		result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{item}, importTime)
		if err != nil {
			t.Fatalf("ImportProviderModels failed: %v", err)
		}
		if len(result.Items) != 1 {
			t.Fatalf("expected one item result, got %+v", result.Items)
		}
		return *result
	}
	loadModel := func(name string) model.Model {
		t.Helper()
		var m model.Model
		if err := db.Where("name = ?", name).First(&m).Error; err != nil {
			t.Fatalf("load model %s: %v", name, err)
		}
		return m
	}

	t.Run("absent declaration leaves the stored model untouched", func(t *testing.T) {
		seedModelRow(t, db, "adj-absent", nil, seedTime)
		result := single(modeladmin.ImportModelItem{ProviderModelName: "adj-absent"})
		if result.Items[0].Status != modeladmin.ImportStatusAppended {
			t.Fatalf("expected appended, got %+v", result.Items[0])
		}
		m := loadModel("adj-absent")
		assertCapabilityRow(t, m, model.Model{})
		if !m.UpdatedAt.Equal(seedTime) {
			t.Fatalf("a row without declarations must not write the model row, updated_at moved to %v", m.UpdatedAt)
		}
	})

	t.Run("a declaration fills an undeclared slot", func(t *testing.T) {
		seedModelRow(t, db, "adj-fill", nil, seedTime)
		result := single(modeladmin.ImportModelItem{
			ProviderModelName: "adj-fill",
			ContextLength:     ptrInt(8192), SupportsTools: ptrString("yes"),
		})
		if result.Items[0].Status != modeladmin.ImportStatusAppended {
			t.Fatalf("expected appended, got %+v", result.Items[0])
		}
		m := loadModel("adj-fill")
		assertCapabilityRow(t, m, model.Model{ContextLength: ptrInt(8192), SupportsTools: ptrBool(true)})
		if !m.UpdatedAt.Equal(importTime) {
			t.Fatalf("a fill must write the model row, updated_at stayed %v", m.UpdatedAt)
		}
	})

	t.Run("an agreeing declaration writes nothing extra", func(t *testing.T) {
		seedModelRow(t, db, "adj-agree", func(m *model.Model) {
			m.ContextLength = ptrInt(8192)
			m.SupportsTools = ptrBool(true)
		}, seedTime)
		result := single(modeladmin.ImportModelItem{
			ProviderModelName: "adj-agree",
			ContextLength:     ptrInt(8192), SupportsTools: ptrString("yes"),
		})
		if result.Items[0].Status != modeladmin.ImportStatusAppended {
			t.Fatalf("expected appended, got %+v", result.Items[0])
		}
		m := loadModel("adj-agree")
		assertCapabilityRow(t, m, model.Model{ContextLength: ptrInt(8192), SupportsTools: ptrBool(true)})
		if !m.UpdatedAt.Equal(seedTime) {
			t.Fatalf("an agreeing declaration must write nothing, updated_at moved to %v", m.UpdatedAt)
		}
	})

	t.Run("a contradicting declaration skips the row with zero writes", func(t *testing.T) {
		seedModelRow(t, db, "adj-conflict", func(m *model.Model) {
			m.ContextLength = ptrInt(8192)
		}, seedTime)
		result := single(modeladmin.ImportModelItem{
			ProviderModelName: "adj-conflict", ContextLength: ptrInt(4096),
		})
		item := result.Items[0]
		if item.Status != modeladmin.ImportStatusSkipped || item.Reason != modeladmin.BatchSkipReasonCapabilityMismatch {
			t.Fatalf("expected a capability_mismatch skip, got %+v", item)
		}
		if item.ModelID == 0 {
			t.Fatalf("the refusal must name the model it contradicts, got %+v", item)
		}
		m := loadModel("adj-conflict")
		assertCapabilityRow(t, m, model.Model{ContextLength: ptrInt(8192)})
		if !m.UpdatedAt.Equal(seedTime) {
			t.Fatalf("a refused row must write nothing, updated_at moved to %v", m.UpdatedAt)
		}
		var count int64
		if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", m.ID).Count(&count).Error; err != nil {
			t.Fatalf("count candidates: %v", err)
		}
		if count != 0 {
			t.Fatalf("a refused row must store no mapping, found %d", count)
		}
	})

	t.Run("one conflict zeroes the whole row's fill", func(t *testing.T) {
		seedModelRow(t, db, "adj-mixed", func(m *model.Model) {
			m.Temperature = ptrFloat(0.7)
		}, seedTime)
		result := single(modeladmin.ImportModelItem{
			ProviderModelName: "adj-mixed",
			Temperature:       ptrFloat(1.5), SupportsTools: ptrString("yes"),
		})
		if result.Items[0].Status != modeladmin.ImportStatusSkipped || result.Items[0].Reason != modeladmin.BatchSkipReasonCapabilityMismatch {
			t.Fatalf("expected a capability_mismatch skip, got %+v", result.Items[0])
		}
		m := loadModel("adj-mixed")
		// The tools slot would have been a clean fill, but the row never
		// lands partially: the conflict refuses it whole.
		assertCapabilityRow(t, m, model.Model{Temperature: ptrFloat(0.7)})
	})

	t.Run("an exists skip still fills the model row", func(t *testing.T) {
		m := seedModelRow(t, db, "adj-exists", nil, seedTime)
		candID := seedCandidateRow(t, db, m.ID, p.ID, "adj-exists", 0, false)
		if err := db.Model(&model.ModelCandidate{}).Where("id = ?", candID).
			Update("verification_status", model.ModelVerificationStatusPassed).Error; err != nil {
			t.Fatalf("settle seeded candidate: %v", err)
		}
		result := single(modeladmin.ImportModelItem{
			ProviderModelName: "adj-exists", InputModalities: []string{"text"},
		})
		if result.Items[0].Status != modeladmin.ImportStatusSkipped || result.Items[0].Reason != modeladmin.BatchSkipReasonExists {
			t.Fatalf("expected an exists skip, got %+v", result.Items[0])
		}
		if result.Items[0].CandidateID != 0 {
			t.Fatalf("a settled mapping must not be requeued, got %+v", result.Items[0])
		}
		stored := loadModel("adj-exists")
		assertCapabilityRow(t, stored, model.Model{InputModalities: `["text"]`, SupportsImageInput: ptrBool(false)}) //nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
	})

	t.Run("an exists fill keeps the untested requeue", func(t *testing.T) {
		m := seedModelRow(t, db, "adj-requeue", nil, seedTime)
		candID := seedCandidateRow(t, db, m.ID, p.ID, "adj-requeue", 0, false)
		result := single(modeladmin.ImportModelItem{
			ProviderModelName: "adj-requeue", SupportsReasoning: ptrString("no"),
		})
		if result.Items[0].Status != modeladmin.ImportStatusSkipped || result.Items[0].Reason != modeladmin.BatchSkipReasonExists {
			t.Fatalf("expected an exists skip, got %+v", result.Items[0])
		}
		if result.Items[0].CandidateID != candID {
			t.Fatalf("the untested mapping must still be offered for requeue, got %+v", result.Items[0])
		}
		stored := loadModel("adj-requeue")
		assertCapabilityRow(t, stored, model.Model{SupportsReasoning: ptrBool(false)})
	})

	t.Run("a created model carries its declared slots and nothing else", func(t *testing.T) {
		result := single(modeladmin.ImportModelItem{
			ProviderModelName: "adj-created",
			SupportsReasoning: ptrString("yes"), Temperature: ptrFloat(1.5),
		})
		if result.Items[0].Status != modeladmin.ImportStatusCreated {
			t.Fatalf("expected created, got %+v", result.Items[0])
		}
		stored := loadModel("adj-created")
		assertCapabilityRow(t, stored, model.Model{SupportsReasoning: ptrBool(true), Temperature: ptrFloat(1.5)})
	})
}

// The exists-branch fill is pinned to the columns the import actually read
// and adjudicated — it must never write name or management_status. A model
// PATCH landing in the window between the batch's model lookup and its fill
// write has to survive; riding the old UpdateModel path instead, whose name/
// status write is unconditional, would revert both from the stale copies the
// lookup carried, so this test goes red the moment the fill regresses to a
// blind whole-row write.
func TestImportProviderModelsExistsFillKeepsConcurrentModelRename(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	seedTime := time.Now().UTC().Add(-2 * time.Hour)
	importTime := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-fill-rename")

	m := seedModelRow(t, db, "fill-rename", nil, seedTime)
	// An already-stored mapping makes the row an exists skip — the branch
	// that still fills the model's undeclared slots.
	seedCandidateRow(t, db, m.ID, p.ID, "fill-rename", 0, false)

	// The concurrent PATCH, injected right before the fill UPDATE executes:
	// it renames the row and flips its status — exactly the two columns the
	// fill must never touch. The write goes through the callback's own tx (a
	// fresh pool connection would deadlock behind the import's transaction),
	// and only the models-table statement triggers it — the residue clear on
	// model_candidates in the same batch must not.
	renamed := false
	if err := db.Callback().Update().Before("gorm:update").Register("rename_model_mid_fill", func(tx *gorm.DB) {
		if renamed {
			return
		}
		if _, ok := tx.Statement.Model.(*model.Model); !ok {
			return
		}
		renamed = true
		if err := tx.Session(&gorm.Session{NewDB: true}).Exec(
			"UPDATE models SET name = ?, management_status = ? WHERE id = ?",
			"fill-rename-patched", model.ModelStatusDisabled, m.ID,
		).Error; err != nil {
			_ = tx.AddError(err)
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "fill-rename", ContextLength: ptrInt(8192)},
	}, importTime)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if result.Items[0].Status != modeladmin.ImportStatusSkipped || result.Items[0].Reason != modeladmin.BatchSkipReasonExists {
		t.Fatalf("expected an exists skip, got %+v", result.Items[0])
	}

	var stored model.Model
	if err := db.First(&stored, m.ID).Error; err != nil {
		t.Fatalf("reload model: %v", err)
	}
	if stored.Name != "fill-rename-patched" {
		t.Fatalf("the fill must not write the name: the concurrent rename was reverted to %q", stored.Name)
	}
	if stored.ManagementStatus != model.ModelStatusDisabled {
		t.Fatalf("the fill must not write management_status: the concurrent flip was reverted to %d", stored.ManagementStatus)
	}
	// The declared slot itself must still have been filled past the competitor.
	assertCapabilityRow(t, stored, model.Model{ContextLength: ptrInt(8192)})
}

// Rows that name the same target model share the model the first row found
// or created: later rows skip as existing instead of colliding on
// UNIQUE(model_id, provider_id), while one upstream name pointed at two
// different targets stays two independent mappings. Result names stay the
// submitted upstream names either way.
func TestImportProviderModelsDedupsByTargetModelName(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-dedup")

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "up-a", ModelName: ptrString("shared-target")},
		{ProviderModelName: "up-b", ModelName: ptrString("shared-target")},
		{ProviderModelName: "dup-up", ModelName: ptrString("target-one")},
		{ProviderModelName: "dup-up", ModelName: ptrString("target-two")},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	wantNames := []string{"up-a", "up-b", "dup-up", "dup-up"}
	wantStatuses := []string{modeladmin.ImportStatusCreated, modeladmin.ImportStatusSkipped, modeladmin.ImportStatusCreated, modeladmin.ImportStatusCreated}
	wantReasons := []string{"", modeladmin.BatchSkipReasonExists, "", ""}
	for i := range result.Items {
		if result.Items[i].Name != wantNames[i] {
			t.Fatalf("item %d name = %q, want %q", i, result.Items[i].Name, wantNames[i])
		}
		if result.Items[i].Status != wantStatuses[i] {
			t.Fatalf("item %d status = %q, want %q", i, result.Items[i].Status, wantStatuses[i])
		}
		if result.Items[i].Reason != wantReasons[i] {
			t.Fatalf("item %d reason = %q, want %q", i, result.Items[i].Reason, wantReasons[i])
		}
	}

	var shared model.Model
	if err := db.Where("name = ?", "shared-target").First(&shared).Error; err != nil {
		t.Fatalf("load shared model: %v", err)
	}
	if result.Items[1].ModelID != shared.ID {
		t.Fatalf("the duplicate row must report the shared model id, got %+v", result.Items[1])
	}
	var sharedCandidates int64
	if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", shared.ID).Count(&sharedCandidates).Error; err != nil {
		t.Fatalf("count shared candidates: %v", err)
	}
	if sharedCandidates != 1 {
		t.Fatalf("expected exactly one mapping on the shared model, got %d", sharedCandidates)
	}
	var firstCandidate model.ModelCandidate
	if err := db.Where("model_id = ?", shared.ID).First(&firstCandidate).Error; err != nil {
		t.Fatalf("load shared candidate: %v", err)
	}
	if firstCandidate.ProviderModelName != "up-a" {
		t.Fatalf("the first row owns the mapping, got upstream %q", firstCandidate.ProviderModelName)
	}
	for name, want := range map[string]string{"target-one": "dup-up", "target-two": "dup-up"} {
		var m model.Model
		if err := db.Where("name = ?", name).First(&m).Error; err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		var c model.ModelCandidate
		if err := db.Where("model_id = ?", m.ID).First(&c).Error; err != nil {
			t.Fatalf("load %s candidate: %v", name, err)
		}
		if c.ProviderModelName != want {
			t.Fatalf("%s must carry its own upstream name %q, got %q", name, want, c.ProviderModelName)
		}
	}
}

// The as-if-enabled context rule, the same criteria the candidate save paths
// enforce: a row that submits a non-zero max_output or declares a context is
// held to max_output within the effective window — against the model's
// enabled sibling candidates and against its own submission — with an equal
// value passing and an undeclared context pinning nothing.
func TestImportProviderModelsContextConsistency(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	p := seedEnabledProvider(t, db, "prov-ctx-a")
	pB := seedEnabledProvider(t, db, "prov-ctx-b")
	seedTime := time.Now().UTC().Add(-2 * time.Hour)
	importTime := time.Now().UTC()

	t.Run("an enabled sibling over the declared context refuses the row", func(t *testing.T) {
		m := seedModelRow(t, db, "ctx-sibling", nil, seedTime)
		seedCandidateRow(t, db, m.ID, pB.ID, "ctx-sibling-live", 8000, true)
		result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
			{ProviderModelName: "ctx-sibling-row", ModelName: ptrString("ctx-sibling"), ContextLength: ptrInt(4096)},
		}, importTime)
		if err != nil {
			t.Fatalf("ImportProviderModels failed: %v", err)
		}
		item := result.Items[0]
		if item.Status != modeladmin.ImportStatusSkipped || item.Reason != modeladmin.BatchSkipReasonCapabilityMismatch {
			t.Fatalf("expected a capability_mismatch skip, got %+v", item)
		}
		// Zero writes: the context the row declared is not filled either.
		var stored model.Model
		if err := db.Where("id = ?", m.ID).First(&stored).Error; err != nil {
			t.Fatalf("reload model: %v", err)
		}
		assertCapabilityRow(t, stored, model.Model{})
		if !stored.UpdatedAt.Equal(seedTime) {
			t.Fatalf("a refused row must write nothing, updated_at moved to %v", stored.UpdatedAt)
		}
		var count int64
		if err := db.Model(&model.ModelCandidate{}).Where("model_id = ? AND provider_id = ?", m.ID, p.ID).Count(&count).Error; err != nil {
			t.Fatalf("count candidates: %v", err)
		}
		if count != 0 {
			t.Fatalf("a refused row must store no mapping, found %d", count)
		}
	})

	t.Run("a fresh row over its own declared context is refused before anything is stored", func(t *testing.T) {
		result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
			{ProviderModelName: "ctx-selfnew", ContextLength: ptrInt(100), MaxOutput: 200},
		}, importTime)
		if err != nil {
			t.Fatalf("ImportProviderModels failed: %v", err)
		}
		item := result.Items[0]
		if item.Status != modeladmin.ImportStatusSkipped || item.Reason != modeladmin.BatchSkipReasonCapabilityMismatch {
			t.Fatalf("expected a capability_mismatch skip, got %+v", item)
		}
		var count int64
		if err := db.Model(&model.Model{}).Where("name = ?", "ctx-selfnew").Count(&count).Error; err != nil {
			t.Fatalf("count models: %v", err)
		}
		if count != 0 {
			t.Fatalf("a refused fresh row must not leave a model behind, found %d", count)
		}
		if err := db.Model(&model.ModelCandidate{}).Where("provider_id = ?", p.ID).Count(&count).Error; err != nil {
			t.Fatalf("count candidates: %v", err)
		}
		if count != 0 {
			t.Fatalf("a refused fresh row must store no mapping, found %d", count)
		}
	})

	t.Run("equal values pass on the sibling and the row itself", func(t *testing.T) {
		m := seedModelRow(t, db, "ctx-equal", func(m *model.Model) {
			m.ContextLength = ptrInt(8192)
		}, seedTime)
		seedCandidateRow(t, db, m.ID, pB.ID, "ctx-equal-live", 8192, true)
		result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
			{ProviderModelName: "ctx-equal-row", ModelName: ptrString("ctx-equal"), ContextLength: ptrInt(8192), MaxOutput: 8192},
		}, importTime)
		if err != nil {
			t.Fatalf("ImportProviderModels failed: %v", err)
		}
		if result.Items[0].Status != modeladmin.ImportStatusAppended {
			t.Fatalf("expected appended, got %+v", result.Items[0])
		}
		var c model.ModelCandidate
		if err := db.Where("model_id = ? AND provider_id = ?", m.ID, p.ID).First(&c).Error; err != nil {
			t.Fatalf("load appended candidate: %v", err)
		}
		if c.MaxOutput != 8192 {
			t.Fatalf("expected the submitted max_output stored, got %d", c.MaxOutput)
		}
	})

	t.Run("a max_output over the stored context refuses the row", func(t *testing.T) {
		m := seedModelRow(t, db, "ctx-strict", func(m *model.Model) {
			m.ContextLength = ptrInt(100)
		}, seedTime)
		result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
			{ProviderModelName: "ctx-strict-row", ModelName: ptrString("ctx-strict"), MaxOutput: 200},
		}, importTime)
		if err != nil {
			t.Fatalf("ImportProviderModels failed: %v", err)
		}
		item := result.Items[0]
		if item.Status != modeladmin.ImportStatusSkipped || item.Reason != modeladmin.BatchSkipReasonCapabilityMismatch {
			t.Fatalf("expected a capability_mismatch skip, got %+v", item)
		}
		var count int64
		if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", m.ID).Count(&count).Error; err != nil {
			t.Fatalf("count candidates: %v", err)
		}
		if count != 0 {
			t.Fatalf("a refused row must store no mapping, found %d", count)
		}
		var stored model.Model
		if err := db.Where("id = ?", m.ID).First(&stored).Error; err != nil {
			t.Fatalf("reload model: %v", err)
		}
		if !stored.UpdatedAt.Equal(seedTime) {
			t.Fatalf("a refused row must write nothing, updated_at moved to %v", stored.UpdatedAt)
		}
	})

	t.Run("a max_output equal to the stored context appends", func(t *testing.T) {
		m := seedModelRow(t, db, "ctx-fit", func(m *model.Model) {
			m.ContextLength = ptrInt(100)
		}, seedTime)
		result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
			{ProviderModelName: "ctx-fit-row", ModelName: ptrString("ctx-fit"), MaxOutput: 100},
		}, importTime)
		if err != nil {
			t.Fatalf("ImportProviderModels failed: %v", err)
		}
		if result.Items[0].Status != modeladmin.ImportStatusAppended {
			t.Fatalf("expected appended, got %+v", result.Items[0])
		}
		var c model.ModelCandidate
		if err := db.Where("model_id = ? AND provider_id = ?", m.ID, p.ID).First(&c).Error; err != nil {
			t.Fatalf("load appended candidate: %v", err)
		}
	})

	t.Run("rows that submit neither max_output nor a context skip the rule", func(t *testing.T) {
		m := seedModelRow(t, db, "ctx-pinned", func(m *model.Model) {
			m.ContextLength = ptrInt(1)
		}, seedTime)
		seedCandidateRow(t, db, m.ID, pB.ID, "ctx-pinned-live", 99999, true)
		result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
			{ProviderModelName: "ctx-pinned-row", ModelName: ptrString("ctx-pinned")},
		}, importTime)
		if err != nil {
			t.Fatalf("ImportProviderModels failed: %v", err)
		}
		if result.Items[0].Status != modeladmin.ImportStatusAppended {
			t.Fatalf("expected appended, got %+v", result.Items[0])
		}
	})
}

// Mixed output modalities keep the existing derivation untouched: a model
// that also serves text keeps token settlement (its audio price slot stays
// inert), while an image+audio declaration lands in the audio character
// price slot, because one mapping bills in one mode and audio is checked
// last.
func TestImportProviderModelsMixedAudioModalityBilling(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-audio-mixed")

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "speech-chat", OutputModalities: []string{model.OutputModalityText, model.OutputModalityAudio}, AudioUnitPrice: ptrFloat(9)},
		{ProviderModelName: "image-speech", OutputModalities: []string{model.OutputModalityImage, model.OutputModalityAudio}, AudioUnitPrice: ptrFloat(3.5)},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("expected two item results, got %+v", result.Items)
	}
	for _, item := range result.Items {
		if item.Status != modeladmin.ImportStatusCreated {
			t.Fatalf("expected every row created, got %+v", item)
		}
	}
	var textRow model.ModelCandidate
	if err := db.Where("provider_model_name = ?", "speech-chat").First(&textRow).Error; err != nil {
		t.Fatalf("load text+audio candidate: %v", err)
	}
	if textRow.BillingMode != model.BillingModeToken {
		t.Fatalf("a model that also serves text must keep token billing, got %q", textRow.BillingMode)
	}
	if textRow.AudioUnitPrice != nil {
		t.Fatalf("the audio price slot must stay inert under token billing, got %v", *textRow.AudioUnitPrice)
	}
	var audioRow model.ModelCandidate
	if err := db.Where("provider_model_name = ?", "image-speech").First(&audioRow).Error; err != nil {
		t.Fatalf("load image+audio candidate: %v", err)
	}
	if audioRow.BillingMode != model.BillingModeAudio {
		t.Fatalf("an image+audio declaration must land audio billing, got %q", audioRow.BillingMode)
	}
	if audioRow.AudioUnitPrice == nil || *audioRow.AudioUnitPrice != 3.5 {
		t.Fatalf("the character price slot must carry the row's price, got %+v", audioRow.AudioUnitPrice)
	}
}
