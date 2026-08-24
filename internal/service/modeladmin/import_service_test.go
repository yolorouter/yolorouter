package modeladmin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/pricecatalog"
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
		{ProviderModelName: "deepseek-ai/DeepSeek-V4", InputPrice: 2, OutputPrice: 8, CacheWritePrice: &cacheWrite, MaxOutput: 8192},
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
		ProviderID: pA.ID, InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("seed CreateModelCandidate failed: %v", err)
	}

	result, err := svc.ImportProviderModels(pB.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "glm-5", InputPrice: 3, OutputPrice: 12},
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
		ProviderID: p.ID, InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("seed CreateModelCandidate failed: %v", err)
	}

	// The 151-char name pins the public-model length limit as a PER-ITEM skip:
	// bulk import publishes the upstream id as the model name (capped at 100),
	// and an over-long id must not fail the whole batch.
	overlong := strings.Repeat("x", 151)
	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "already-mapped", InputPrice: 1, OutputPrice: 2},
		{ProviderModelName: "bad name!", InputPrice: 1, OutputPrice: 2},
		{ProviderModelName: "fresh-one", InputPrice: 1, OutputPrice: 2},
		{ProviderModelName: "fresh-one", InputPrice: 9, OutputPrice: 9},
		{ProviderModelName: overlong, InputPrice: 1, OutputPrice: 2},
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
		ProviderID: p.ID, InputPrice: 1, OutputPrice: 2,
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
		ProviderID: p.ID, InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("seed settled candidate failed: %v", err)
	}
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", settled.ID).
		Update("verification_status", model.ModelVerificationStatusPassed).Error; err != nil {
		t.Fatalf("seed settled verdict failed: %v", err)
	}

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "unfinished-model", InputPrice: 1, OutputPrice: 2},
		{ProviderModelName: "settled-model", InputPrice: 1, OutputPrice: 2},
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
		{ProviderModelName: "raced-model", InputPrice: 1, OutputPrice: 2},
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
		{ProviderModelName: "some-model", InputPrice: 1, OutputPrice: 2},
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
		{ProviderModelName: "alpha", InputPrice: 1, OutputPrice: 2},
		{ProviderModelName: "boom", InputPrice: 1, OutputPrice: 2},
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

func TestSuggestCandidatePricesBatchReturnsHistorySeedAndEmpty(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedProviderWithBaseURL(t, providerService, "deepseek", catalogSeededHost)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	now := time.Now().UTC()
	seedName := catalogSeededModel(t)

	// History for a name the catalog ALSO carries, priced differently — proving
	// history outranks the catalog in the batch path too.
	const historyName = "deepseek-v4-pro"
	if _, ok := pricecatalog.Lookup(catalogSeededHost, historyName); !ok {
		t.Fatalf("the seed catalog no longer carries %s; pick another name", historyName)
	}
	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: historyName}, now)
	if err != nil {
		t.Fatalf("seed CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: prov.ID, InputPrice: 42, OutputPrice: 43,
	}, now); err != nil {
		t.Fatalf("seed CreateModelCandidate failed: %v", err)
	}

	got, err := svc.SuggestCandidatePrices(prov.ID, []string{historyName, seedName, "no-such-model"})
	if err != nil {
		t.Fatalf("SuggestCandidatePrices failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected one entry per requested name, got %+v", got)
	}
	if got[historyName].Source != "history" || got[historyName].InputPrice != 42 {
		t.Fatalf("expected the provider's own price to outrank the catalog, got %+v", got[historyName])
	}
	if got[seedName].Source != "seed" {
		t.Fatalf("expected a catalog hit for %s, got %+v", seedName, got[seedName])
	}
	want, _ := pricecatalog.Lookup(catalogSeededHost, seedName)
	if got[seedName].InputPrice != want.Input || got[seedName].OutputPrice != want.Output {
		t.Fatalf("catalog figures must pass through, want %v/%v got %+v", want.Input, want.Output, got[seedName])
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
		{ProviderModelName: "deadlocked-model", InputPrice: 1, OutputPrice: 2},
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
			ProviderID: p.ID, ProviderModelName: "shared-upstream-name", InputPrice: seed.price, OutputPrice: seed.price,
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
		{ProviderModelName: "foo/", InputPrice: 1, OutputPrice: 2},
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
		items[i] = modeladmin.ImportModelItem{ProviderModelName: fmt.Sprintf("vendor/model-%04d", i), InputPrice: 1, OutputPrice: 2}
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
