package modeladmin_test

// The bulk import declares modality per row: an image row creates an image
// model and a per-image billed mapping, anything that still serves text keeps
// token settlement, and a bad declaration costs only its own row.

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

func importRow(t *testing.T, svc *modeladmin.ModelService, providerID uint, item modeladmin.ImportModelItem) modeladmin.ImportItemResult {
	t.Helper()
	result, err := svc.ImportProviderModels(providerID, []modeladmin.ImportModelItem{item}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected one item result, got %+v", result.Items)
	}
	return result.Items[0]
}

// The full scenario a bulk import of image models must survive end to end:
// declare, probe, and land enabled — every stage switching on the declaration.
func TestImportedImageRowProbesImageAndAutoEnables(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "image-import-provider")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	result, err := svc.ImportProviderModels(prov.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "wan2.7-image", OutputModalities: []string{model.OutputModalityImage}, InputPrice: 0.3, OutputPrice: 1.2},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	item := result.Items[0]
	if item.Status != modeladmin.ImportStatusCreated {
		t.Fatalf("expected the image row created, got %+v", item)
	}

	var m model.Model
	if err := db.First(&m, item.ModelID).Error; err != nil {
		t.Fatalf("imported model not stored: %v", err)
	}
	if m.OutputModalities != `["image"]` {
		t.Fatalf("expected the model declared image-only, got %q", m.OutputModalities)
	}

	var c model.ModelCandidate
	if err := db.First(&c, item.CandidateID).Error; err != nil {
		t.Fatalf("imported candidate not stored: %v", err)
	}
	if c.BillingMode != model.BillingModeImage {
		t.Fatalf("an image-exclusive mapping must bill per image, got %q", c.BillingMode)
	}
	if c.ImagePricingTiers != "" {
		t.Fatalf("the import carries no tier table, expected empty tiers, got %q", c.ImagePricingTiers)
	}
	// Token prices are stored as submitted even under per-image billing:
	// they are inert there, and keeping them lossless preserves the batch's
	// audit trail for an admin who flips the mode back.
	if c.InputPrice != 0.3 || c.OutputPrice != 1.2 {
		t.Fatalf("submitted prices not stored verbatim: %+v", c)
	}

	basicBefore := client.CallCountFor("basic")
	imageBefore := client.CallCountFor("image")
	if err := svc.ProbeQueuedCandidate(t.Context(), item.CandidateID, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}

	if got := client.CallCountFor("image") - imageBefore; got != 1 {
		t.Errorf("image probes = %d, want exactly 1", got)
	}
	if got := client.CallCountFor("basic") - basicBefore; got != 0 {
		t.Errorf("chat probes against the imported image model = %d, want 0", got)
	}
	reloaded := loadCandidate(t, db, item.CandidateID)
	if reloaded.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("a passing image probe must auto-enable the imported mapping, got %d", reloaded.ManagementStatus)
	}
	if reloaded.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification passed, got %d", reloaded.VerificationStatus)
	}
}

// A declaration that includes text keeps token settlement even when image is
// also declared: the mapping still carries chat traffic.
func TestImportProviderModelsMixedAndTextRowsStayTokenBilled(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "mixed-import-provider")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	result, err := svc.ImportProviderModels(prov.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "gemini-image-chat", OutputModalities: []string{model.OutputModalityText, model.OutputModalityImage}},
		{ProviderModelName: "plain-text-model"},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	for _, item := range result.Items {
		if item.Status != modeladmin.ImportStatusCreated {
			t.Fatalf("expected every row created, got %+v", item)
		}
		var c model.ModelCandidate
		if err := db.First(&c, item.CandidateID).Error; err != nil {
			t.Fatalf("candidate not stored: %v", err)
		}
		if c.BillingMode != model.BillingModeToken {
			t.Fatalf("a model that also serves text must keep token billing, got %q for %s", c.BillingMode, item.Name)
		}
	}

	var mixed model.Model
	if err := db.First(&mixed, result.Items[0].ModelID).Error; err != nil {
		t.Fatalf("mixed model not stored: %v", err)
	}
	if mixed.OutputModalities != `["text","image"]` {
		t.Fatalf("expected both modalities stored, got %q", mixed.OutputModalities)
	}
	var textOnly model.Model
	if err := db.First(&textOnly, result.Items[1].ModelID).Error; err != nil {
		t.Fatalf("text model not stored: %v", err)
	}
	if textOnly.OutputModalities != `["text"]` {
		t.Fatalf("an undeclared row imports as text-only, got %q", textOnly.OutputModalities)
	}
}

// A bad declaration skips its own row only; the rest of the batch proceeds.
func TestImportProviderModelsSkipsInvalidModalityRow(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "invalid-modality-provider")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	result, err := svc.ImportProviderModels(prov.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "broken-row", OutputModalities: []string{"video"}},
		{ProviderModelName: "healthy-row", OutputModalities: []string{model.OutputModalityImage}},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("expected two item results, got %+v", result.Items)
	}
	if result.Items[0].Status != modeladmin.ImportStatusSkipped || result.Items[0].Reason != modeladmin.BatchSkipReasonInvalid {
		t.Fatalf("expected the bad-modality row skipped as invalid, got %+v", result.Items[0])
	}
	if result.Items[1].Status != modeladmin.ImportStatusCreated {
		t.Fatalf("expected the healthy row created, got %+v", result.Items[1])
	}
	if result.Skipped != 1 || result.Created != 1 {
		t.Fatalf("tally mismatch: %+v", result)
	}
}

// Appending a mapping to a model that already declares image output derives
// per-image billing from the stored declaration, not from the row's form —
// and an explicit declaration that AGREES with the stored one appends too.
func TestImportProviderModelsAppendsPerImageCandidateToExistingImageModel(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "append-image-provider")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	view, err := svc.CreateModel(modeladmin.CreateModelInput{
		Name: "existing-image-model", OutputModalities: []string{model.OutputModalityImage},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create image model: %v", err)
	}

	// The row re-imports the name without re-declaring the modality.
	item := importRow(t, svc, prov.ID, modeladmin.ImportModelItem{ProviderModelName: "existing-image-model"})
	if item.Status != modeladmin.ImportStatusAppended {
		t.Fatalf("expected the mapping appended, got %+v", item)
	}
	var c model.ModelCandidate
	if err := db.First(&c, item.CandidateID).Error; err != nil {
		t.Fatalf("appended candidate not stored: %v", err)
	}
	if c.ModelID != view.ID {
		t.Fatalf("appended candidate must link the existing model, got %+v", c)
	}
	if c.BillingMode != model.BillingModeImage {
		t.Fatalf("billing must follow the stored image declaration, got %q", c.BillingMode)
	}

	// Another provider appends while declaring exactly what the model already
	// says: agreement is redundant, not a conflict.
	prov2 := seedEnabledProviderForModelTest(t, providerService, "append-image-provider-2")
	item2 := importRow(t, svc, prov2.ID, modeladmin.ImportModelItem{
		ProviderModelName: "existing-image-model",
		OutputModalities:  []string{model.OutputModalityImage},
	})
	if item2.Status != modeladmin.ImportStatusAppended {
		t.Fatalf("an agreeing declaration must append, got %+v", item2)
	}
}

// A row that explicitly declares modalities contradicting the existing
// model's stored declaration is refused, naming the conflict: billing and
// probe selection both follow the model row, so appending anyway would
// silently bill and probe against a model the row just disavowed (the live
// failure mode that motivated this guard — re-importing over models a
// pre-modality import had declared as text).
func TestImportProviderModelsSkipsModalityMismatchOnAppend(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "mismatch-provider")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "wan2.7-image"}, time.Now().UTC()); err != nil {
		t.Fatalf("create text-declared model: %v", err)
	}

	item := importRow(t, svc, prov.ID, modeladmin.ImportModelItem{
		ProviderModelName: "wan2.7-image",
		OutputModalities:  []string{model.OutputModalityImage},
	})
	if item.Status != modeladmin.ImportStatusSkipped || item.Reason != modeladmin.BatchSkipReasonModalityMismatch {
		t.Fatalf("expected a modality-mismatch skip, got %+v", item)
	}
	if item.CandidateID != 0 {
		t.Fatalf("a refused row must not store a mapping, got candidate %d", item.CandidateID)
	}
	var count int64
	if err := db.Model(&model.ModelCandidate{}).Where("provider_id = ?", prov.ID).Count(&count).Error; err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if count != 0 {
		t.Fatalf("a refused row must store nothing, found %d candidates", count)
	}
	// The stored declaration is untouched — the model edit is the only lever.
	var m model.Model
	if err := db.Where("name = ?", "wan2.7-image").First(&m).Error; err != nil {
		t.Fatalf("load model: %v", err)
	}
	if m.OutputModalities != `["text"]` {
		t.Fatalf("the refused row must not rewrite the model declaration, got %q", m.OutputModalities)
	}
}

// The conflict guard also covers a row whose MAPPING already exists: the
// refusal outranks the exists skip (naming the real problem) and, unlike a
// requeue, hands back no candidate id — re-probing under a declaration the
// row just contradicted would only record another inconclusive verdict.
func TestImportProviderModelsMismatchOutranksExistsAndSkipsRequeue(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "mismatch-exists-provider")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	// First import lands the mapping under the text declaration.
	item := importRow(t, svc, prov.ID, modeladmin.ImportModelItem{ProviderModelName: "wan2.7-image"})
	if item.Status != modeladmin.ImportStatusCreated {
		t.Fatalf("expected the first row created, got %+v", item)
	}

	// Re-import with a contradicting declaration: the mapping exists AND the
	// declaration conflicts — the mismatch must win, with no requeue id.
	result, err := svc.ImportProviderModels(prov.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "wan2.7-image", OutputModalities: []string{model.OutputModalityImage}},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected one item result, got %+v", result.Items)
	}
	got := result.Items[0]
	if got.Status != modeladmin.ImportStatusSkipped || got.Reason != modeladmin.BatchSkipReasonModalityMismatch {
		t.Fatalf("expected a modality-mismatch skip outranking exists, got %+v", got)
	}
	if got.CandidateID != 0 {
		t.Fatalf("a refused row must not surface a requeue candidate id, got %d", got.CandidateID)
	}
}
