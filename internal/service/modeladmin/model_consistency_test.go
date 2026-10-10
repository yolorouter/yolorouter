package modeladmin_test

// Save-time consistency validation tests: the three hard-coded rules — image
// pricing tiers, context-length ceiling, temperature range — each asserted on
// BOTH save paths. The model save path
// (UpdateModel PATCH) re-reads the candidate set; the candidate save paths
// (test-and-create, plain create, candidate PATCH) re-read the model and the
// siblings. Every arm is assertable on its own: the rejection sentinel, the
// no-persistence of the rejected save, and the exempt configurations that
// must keep passing.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/service/providerclient/providerclienttest"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// consistencyFixture builds a service on a fresh SQLite db with three enabled
// providers (a model takes at most one candidate per provider, so a
// multi-candidate model needs several; the third also hosts directly-seeded
// rows whose provider must satisfy the foreign key).
func consistencyFixture(t *testing.T, slug string) (*modeladmin.ModelService, *gorm.DB, *providerclienttest.Fake, *provider.ProviderView, *provider.ProviderView, *provider.ProviderView) {
	t.Helper()
	providerService, db, client := newTestProviderService(t)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	return svc, db, client,
		seedEnabledProviderForModelTest(t, providerService, slug+"-prov-a"),
		seedEnabledProviderForModelTest(t, providerService, slug+"-prov-b"),
		seedEnabledProviderForModelTest(t, providerService, slug+"-prov-c")
}

// consistencyTiers is a minimal per-image table that prices.
func consistencyTiers(price float64) *model.ImagePricingTiers {
	return &model.ImagePricingTiers{Mode: "per_image", DefaultPrice: &price}
}

// consistencySeedEnabled adds an enabled candidate through the test-and-create
// path (the fake client's probes always pass) and returns the stored row id.
func consistencySeedEnabled(t *testing.T, svc *modeladmin.ModelService, modelID, providerID uint, upstream string, in modeladmin.CreateCandidateInput) uint {
	t.Helper()
	in.ProviderID = providerID
	in.ProviderModelName = upstream
	in.ManagementStatus = model.ModelCandidateStatusEnabled
	res, err := svc.TestAndCreateCandidate(t.Context(), modelID, in, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed enabled candidate: %v", err)
	}
	if !res.Created || res.Candidate == nil {
		t.Fatalf("seed enabled candidate: not created, report %+v", res.Report)
	}
	if res.Candidate.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("seed enabled candidate: status = %d, want enabled", res.Candidate.ManagementStatus)
	}
	return res.Candidate.ID
}

// consistencyCreateModel creates a model, failing on anything but success.
func consistencyCreateModel(t *testing.T, svc *modeladmin.ModelService, in modeladmin.CreateModelInput) uint {
	t.Helper()
	view, err := svc.CreateModel(in, time.Now().UTC())
	if err != nil {
		t.Fatalf("create model %q: %v", in.Name, err)
	}
	return view.ID
}

// consistencyStoredModel reads the stored row for a no-persistence assertion.
func consistencyStoredModel(t *testing.T, db *gorm.DB, id uint) model.Model {
	t.Helper()
	var stored model.Model
	if err := db.Where("id = ?", id).First(&stored).Error; err != nil {
		t.Fatalf("read stored model: %v", err)
	}
	return stored
}

// consistencyStoredCandidate reads the stored row for a no-persistence
// assertion.
func consistencyStoredCandidate(t *testing.T, db *gorm.DB, id uint) model.ModelCandidate {
	t.Helper()
	var stored model.ModelCandidate
	if err := db.Where("id = ?", id).First(&stored).Error; err != nil {
		t.Fatalf("read stored candidate: %v", err)
	}
	return stored
}

// TestConsistencyImageRuleOnModelSavePath is the image rule's model-path
// half: a declaration containing image output must meet the per-image billing
// expectation of every ENABLED candidate at the moment the model is saved.
func TestConsistencyImageRuleOnModelSavePath(t *testing.T) {
	svc, db, _, provA, _, _ := consistencyFixture(t, "imgsave")
	now := time.Now().UTC()

	t.Run("token-billed enabled candidate blocks an image declaration", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "imgsave-block"})
		consistencySeedEnabled(t, svc, id, provA.ID, "imgsave-block-real", modeladmin.CreateCandidateInput{})

		_, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "imgsave-block", OutputModalitiesSet: true,
			OutputModalities: []string{model.OutputModalityText, model.OutputModalityImage},
		}, now)
		if !errors.Is(err, errcode.ErrModelImageBillingMismatch) {
			t.Fatalf("error = %v, want the image-billing sentinel", err)
		}
		if got := consistencyStoredModel(t, db, id).OutputModalities; got != `["text"]` {
			t.Fatalf("a rejected declaration must not persist, got %q", got)
		}
	})

	t.Run("image-billed enabled candidate with a table passes", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "imgsave-pass"})
		consistencySeedEnabled(t, svc, id, provA.ID, "imgsave-pass-real", modeladmin.CreateCandidateInput{
			BillingMode: model.BillingModeImage, ImagePricingTiers: consistencyTiers(0.25),
		})
		view, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "imgsave-pass", OutputModalitiesSet: true,
			OutputModalities: []string{model.OutputModalityText, model.OutputModalityImage},
		}, now)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if len(view.OutputModalities) != 2 || view.OutputModalities[1] != model.OutputModalityImage {
			t.Fatalf("output modalities = %v, want the image declaration stored", view.OutputModalities)
		}
	})

	t.Run("image-billed enabled candidate without a stored table blocks", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "imgsave-notiers"})
		cid := consistencySeedEnabled(t, svc, id, provA.ID, "imgsave-notiers-real", modeladmin.CreateCandidateInput{
			BillingMode: model.BillingModeImage, ImagePricingTiers: consistencyTiers(0.25),
		})
		// Legacy shape no API path can produce anymore: image mode with no
		// stored table. The save re-reads the pair as every reader will, so
		// the image declaration must still be refused.
		if err := db.Model(&model.ModelCandidate{}).Where("id = ?", cid).Update("image_pricing_tiers", "").Error; err != nil {
			t.Fatalf("wipe tiers: %v", err)
		}

		_, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "imgsave-notiers", OutputModalitiesSet: true,
			OutputModalities: []string{model.OutputModalityImage},
		}, now)
		if !errors.Is(err, errcode.ErrModelImageBillingMismatch) {
			t.Fatalf("error = %v, want the image-billing sentinel for a table-less image candidate", err)
		}
	})

	t.Run("disabled token candidate is exempt", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "imgsave-disabled"})
		// The plain create path stores disabled rows — a work-in-progress
		// mapping must not block the model's declaration.
		if _, err := svc.CreateModelCandidate(t.Context(), id, modeladmin.CreateCandidateInput{
			ProviderID: provA.ID, ProviderModelName: "imgsave-disabled-real",
		}, now); err != nil {
			t.Fatalf("create disabled candidate: %v", err)
		}
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "imgsave-disabled", OutputModalitiesSet: true,
			OutputModalities: []string{model.OutputModalityImage},
		}, now); err != nil {
			t.Fatalf("a disabled candidate must be exempt, got %v", err)
		}
	})
}

// TestConsistencyImageRuleOnCandidateSavePath is the image rule's
// candidate-path half: a candidate save under an image-output model is judged
// by the same rule, with the model and the sibling candidates re-read at
// save time.
func TestConsistencyImageRuleOnCandidateSavePath(t *testing.T) {
	svc, db, client, provA, provB, _ := consistencyFixture(t, "imgcand")
	now := time.Now().UTC()
	imageModel := func(name string) uint {
		return consistencyCreateModel(t, svc, modeladmin.CreateModelInput{
			Name: name, OutputModalities: []string{model.OutputModalityImage},
		})
	}

	t.Run("test-and-create refuses an enabled token candidate before probing", func(t *testing.T) {
		id := imageModel("imgcand-refuse")
		probesBefore := client.CallCountFor("image")

		_, err := svc.TestAndCreateCandidate(t.Context(), id, modeladmin.CreateCandidateInput{
			ProviderID: provA.ID, ProviderModelName: "imgcand-refuse-real",
			ManagementStatus: model.ModelCandidateStatusEnabled,
		}, now)
		if !errors.Is(err, errcode.ErrModelImageBillingMismatch) {
			t.Fatalf("error = %v, want the image-billing sentinel", err)
		}
		if got := client.CallCountFor("image") - probesBefore; got != 0 {
			t.Fatalf("a refused save spent %d upstream probes; the rules must reject before probing", got)
		}
		var count int64
		if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", id).Count(&count).Error; err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatalf("a refused save stored %d candidates", count)
		}
	})

	t.Run("test-and-create accepts an enabled image-billed candidate", func(t *testing.T) {
		id := imageModel("imgcand-accept")
		cid := consistencySeedEnabled(t, svc, id, provA.ID, "imgcand-accept-real", modeladmin.CreateCandidateInput{
			BillingMode: model.BillingModeImage, ImagePricingTiers: consistencyTiers(0.19),
		})
		stored := consistencyStoredCandidate(t, db, cid)
		if stored.BillingMode != model.BillingModeImage || stored.ImagePricingTiers == "" {
			t.Fatalf("stored declaration = %q/%q, want image mode with a table", stored.BillingMode, stored.ImagePricingTiers)
		}
	})

	t.Run("switching an enabled candidate off image billing is refused", func(t *testing.T) {
		id := imageModel("imgcand-switch")
		cid := consistencySeedEnabled(t, svc, id, provA.ID, "imgcand-switch-real", modeladmin.CreateCandidateInput{
			BillingMode: model.BillingModeImage, ImagePricingTiers: consistencyTiers(0.19),
		})
		token := model.BillingModeToken
		_, err := svc.UpdateModelCandidate(t.Context(), cid, modeladmin.UpdateCandidateInput{BillingMode: &token}, now)
		if !errors.Is(err, errcode.ErrModelImageBillingMismatch) {
			t.Fatalf("error = %v, want the image-billing sentinel", err)
		}
		if stored := consistencyStoredCandidate(t, db, cid); stored.BillingMode != model.BillingModeImage {
			t.Fatalf("a rejected switch must not persist, got billing mode %q", stored.BillingMode)
		}
	})

	t.Run("enabling a token candidate through a save is refused", func(t *testing.T) {
		id := imageModel("imgcand-enable")
		view, err := svc.CreateModelCandidate(t.Context(), id, modeladmin.CreateCandidateInput{
			ProviderID: provA.ID, ProviderModelName: "imgcand-enable-real",
		}, now)
		if err != nil {
			t.Fatalf("create disabled candidate: %v", err)
		}
		enabled := model.ModelCandidateStatusEnabled
		_, err = svc.UpdateModelCandidate(t.Context(), view.ID, modeladmin.UpdateCandidateInput{ManagementStatus: &enabled}, now)
		if !errors.Is(err, errcode.ErrModelImageBillingMismatch) {
			t.Fatalf("error = %v, want the image-billing sentinel", err)
		}
		if stored := consistencyStoredCandidate(t, db, view.ID); stored.ManagementStatus != model.ModelCandidateStatusDisabled {
			t.Fatalf("a rejected enable must not persist, got status %d", stored.ManagementStatus)
		}
	})

	t.Run("a save re-asserts a legacy table-less row", func(t *testing.T) {
		id := imageModel("imgcand-legacy")
		cid := consistencySeedEnabled(t, svc, id, provA.ID, "imgcand-legacy-real", modeladmin.CreateCandidateInput{
			BillingMode: model.BillingModeImage, ImagePricingTiers: consistencyTiers(0.19),
		})
		if err := db.Model(&model.ModelCandidate{}).Where("id = ?", cid).Update("image_pricing_tiers", "").Error; err != nil {
			t.Fatalf("wipe tiers: %v", err)
		}
		price := 9.9
		_, err := svc.UpdateModelCandidate(t.Context(), cid, modeladmin.UpdateCandidateInput{InputPrice: &price}, now)
		if !errors.Is(err, errcode.ErrModelImageBillingMismatch) {
			t.Fatalf("error = %v, want the save to re-assert the effective set", err)
		}
	})

	t.Run("a second enabled candidate joins the same rule", func(t *testing.T) {
		id := imageModel("imgcand-second")
		consistencySeedEnabled(t, svc, id, provA.ID, "imgcand-second-a", modeladmin.CreateCandidateInput{
			BillingMode: model.BillingModeImage, ImagePricingTiers: consistencyTiers(0.19),
		})
		// The second enabled candidate refuses to land token-billed while the
		// model declares image output.
		_, err := svc.TestAndCreateCandidate(t.Context(), id, modeladmin.CreateCandidateInput{
			ProviderID: provB.ID, ProviderModelName: "imgcand-second-b",
			ManagementStatus: model.ModelCandidateStatusEnabled,
		}, now)
		if !errors.Is(err, errcode.ErrModelImageBillingMismatch) {
			t.Fatalf("error = %v, want the image-billing sentinel for the second candidate", err)
		}
	})
}

// TestConsistencyContextRuleOnModelSavePath is the context rule's model-path
// half: any enabled candidate's max_output over the declared context_length
// refuses the save; equal passes; an undeclared context pins nothing.
func TestConsistencyContextRuleOnModelSavePath(t *testing.T) {
	svc, db, _, provA, provB, provC := consistencyFixture(t, "ctxsave")
	now := time.Now().UTC()

	t.Run("single candidate over the ceiling is refused, equal passes", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "ctxsave-single"})
		consistencySeedEnabled(t, svc, id, provA.ID, "ctxsave-single-real", modeladmin.CreateCandidateInput{MaxOutput: 8192})

		over := 4096
		_, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxsave-single", ContextLengthSet: true, ContextLength: &over,
		}, now)
		if !errors.Is(err, errcode.ErrModelMaxOutputExceedsContext) {
			t.Fatalf("error = %v, want the max-output sentinel", err)
		}
		if consistencyStoredModel(t, db, id).ContextLength != nil {
			t.Fatal("a rejected context declaration must not persist")
		}

		equal := 8192
		view, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxsave-single", ContextLengthSet: true, ContextLength: &equal,
		}, now)
		if err != nil {
			t.Fatalf("a candidate filling the whole window must pass, got %v", err)
		}
		if view.ContextLength == nil || *view.ContextLength != 8192 {
			t.Fatalf("context_length = %v, want 8192", view.ContextLength)
		}
	})

	t.Run("clearing the declaration removes the pin", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "ctxsave-clear"})
		consistencySeedEnabled(t, svc, id, provA.ID, "ctxsave-clear-real", modeladmin.CreateCandidateInput{MaxOutput: 4096})
		equal := 4096
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxsave-clear", ContextLengthSet: true, ContextLength: &equal,
		}, now); err != nil {
			t.Fatalf("declare: %v", err)
		}
		// nil with the set-flag is the explicit clear back to undeclared —
		// an undeclared context must stop constraining saves.
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxsave-clear", ContextLengthSet: true,
		}, now); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if consistencyStoredModel(t, db, id).ContextLength != nil {
			t.Fatal("the clear must land as NULL")
		}
	})

	t.Run("multi-candidate: the one over refuses, the disabled one is exempt", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "ctxsave-multi"})
		consistencySeedEnabled(t, svc, id, provA.ID, "ctxsave-multi-a", modeladmin.CreateCandidateInput{MaxOutput: 4096})
		consistencySeedEnabled(t, svc, id, provB.ID, "ctxsave-multi-b", modeladmin.CreateCandidateInput{MaxOutput: 8192})
		// A disabled mapping may carry any max_output: it routes nothing. Both
		// providers are taken by the enabled pair, so the disabled row is
		// stored directly.
		disabled := &model.ModelCandidate{
			ModelID: id, ProviderID: provC.ID, ProviderModelName: "ctxsave-multi-c",
			MaxOutput: 999999, ManagementStatus: model.ModelCandidateStatusDisabled,
			VerificationStatus: model.ModelVerificationStatusUntested,
			CreatedAt:          now, UpdatedAt: now, PriceUpdatedAt: now,
		}
		if err := db.Create(disabled).Error; err != nil {
			t.Fatalf("seed disabled candidate: %v", err)
		}

		within := 8192
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxsave-multi", ContextLengthSet: true, ContextLength: &within,
		}, now); err != nil {
			t.Fatalf("equal-to-the-largest must pass: %v", err)
		}
		justUnder := 8191
		_, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxsave-multi", ContextLengthSet: true, ContextLength: &justUnder,
		}, now)
		if !errors.Is(err, errcode.ErrModelMaxOutputExceedsContext) {
			t.Fatalf("error = %v, want the max-output sentinel for the 8192 candidate", err)
		}
	})

	t.Run("an undeclared context never constrains an unrelated save", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "ctxsave-nil"})
		consistencySeedEnabled(t, svc, id, provA.ID, "ctxsave-nil-real", modeladmin.CreateCandidateInput{MaxOutput: 1000000})
		temp := 0.5
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxsave-nil", TemperatureSet: true, Temperature: &temp,
		}, now); err != nil {
			t.Fatalf("a model that declares no context must not constrain saves: %v", err)
		}
	})
}

// TestConsistencyContextRuleOnCandidateSavePath is the context rule's
// candidate-path half: the candidate save is judged against the model's
// stored declaration, and re-asserts the whole enabled set — a sibling the
// save does not touch can refuse it just the same.
func TestConsistencyContextRuleOnCandidateSavePath(t *testing.T) {
	svc, db, _, provA, provB, _ := consistencyFixture(t, "ctxcand")
	now := time.Now().UTC()

	t.Run("raising max_output over the model's ceiling is refused, equal passes", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "ctxcand-raise"})
		ctxLen := 8192
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxcand-raise", ContextLengthSet: true, ContextLength: &ctxLen,
		}, now); err != nil {
			t.Fatalf("declare context: %v", err)
		}
		cid := consistencySeedEnabled(t, svc, id, provA.ID, "ctxcand-raise-real", modeladmin.CreateCandidateInput{MaxOutput: 4096})

		over := 9999
		_, err := svc.UpdateModelCandidate(t.Context(), cid, modeladmin.UpdateCandidateInput{MaxOutput: &over}, now)
		if !errors.Is(err, errcode.ErrModelMaxOutputExceedsContext) {
			t.Fatalf("error = %v, want the max-output sentinel", err)
		}
		if got := consistencyStoredCandidate(t, db, cid).MaxOutput; got != 4096 {
			t.Fatalf("a rejected max_output must not persist, got %d", got)
		}

		equal := 8192
		if _, err := svc.UpdateModelCandidate(t.Context(), cid, modeladmin.UpdateCandidateInput{MaxOutput: &equal}, now); err != nil {
			t.Fatalf("a candidate filling the whole window must pass: %v", err)
		}
	})

	t.Run("create with requested enable over the ceiling is refused", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "ctxcand-create"})
		ctxLen := 4096
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxcand-create", ContextLengthSet: true, ContextLength: &ctxLen,
		}, now); err != nil {
			t.Fatalf("declare context: %v", err)
		}
		_, err := svc.TestAndCreateCandidate(t.Context(), id, modeladmin.CreateCandidateInput{
			ProviderID: provA.ID, ProviderModelName: "ctxcand-create-real",
			ManagementStatus: model.ModelCandidateStatusEnabled, MaxOutput: 8192,
		}, now)
		if !errors.Is(err, errcode.ErrModelMaxOutputExceedsContext) {
			t.Fatalf("error = %v, want the max-output sentinel", err)
		}
		var count int64
		if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", id).Count(&count).Error; err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatalf("a refused create stored %d candidates", count)
		}
	})

	t.Run("disabled create over the ceiling is exempt", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "ctxcand-disabled"})
		ctxLen := 4096
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxcand-disabled", ContextLengthSet: true, ContextLength: &ctxLen,
		}, now); err != nil {
			t.Fatalf("declare context: %v", err)
		}
		if _, err := svc.CreateModelCandidate(t.Context(), id, modeladmin.CreateCandidateInput{
			ProviderID: provA.ID, ProviderModelName: "ctxcand-disabled-real", MaxOutput: 999999,
		}, now); err != nil {
			t.Fatalf("a disabled mapping may carry any max_output: %v", err)
		}
	})

	t.Run("an undeclared model ceiling pins nothing", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "ctxcand-nil"})
		consistencySeedEnabled(t, svc, id, provA.ID, "ctxcand-nil-real", modeladmin.CreateCandidateInput{MaxOutput: 999999})
	})

	t.Run("a save re-asserts the whole enabled set", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "ctxcand-sibling"})
		ctxLen := 8192
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "ctxcand-sibling", ContextLengthSet: true, ContextLength: &ctxLen,
		}, now); err != nil {
			t.Fatalf("declare context: %v", err)
		}
		cid := consistencySeedEnabled(t, svc, id, provA.ID, "ctxcand-sibling-a", modeladmin.CreateCandidateInput{MaxOutput: 4096})
		// A sibling the save never touches, already over the ceiling — the
		// shape only a legacy row can hold. Any candidate save on the model
		// must refuse until the pair is consistent again.
		overSibling := &model.ModelCandidate{
			ModelID: id, ProviderID: provB.ID, ProviderModelName: "ctxcand-sibling-b",
			MaxOutput: 999999, ManagementStatus: model.ModelCandidateStatusEnabled,
			VerificationStatus: model.ModelVerificationStatusPassed,
			CreatedAt:          now, UpdatedAt: now, PriceUpdatedAt: now,
		}
		if err := db.Create(overSibling).Error; err != nil {
			t.Fatalf("seed over-limit sibling: %v", err)
		}

		price := 1.5
		_, err := svc.UpdateModelCandidate(t.Context(), cid, modeladmin.UpdateCandidateInput{InputPrice: &price}, now)
		if !errors.Is(err, errcode.ErrModelMaxOutputExceedsContext) {
			t.Fatalf("error = %v, want the save to re-assert the whole enabled set", err)
		}
	})
}

// TestConsistencyTemperatureBounds is the temperature rule: the declared
// temperature is bounded by the closed interval [0, 2] — endpoints pass, a
// hair outside either edge refuses, and nil (absent or explicitly cleared)
// passes.
func TestConsistencyTemperatureBounds(t *testing.T) {
	svc, db, _, _, _, _ := consistencyFixture(t, "temp")
	now := time.Now().UTC()

	declared := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		value   *float64
		wantErr bool
	}{
		{"zero endpoint passes", declared(0), false},
		{"two endpoint passes", declared(2), false},
		{"below zero refuses", declared(-0.01), true},
		{"above two refuses", declared(2.01), true},
		{"explicit clear passes", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slug := strings.ReplaceAll(tc.name, " ", "-")
			id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "temp-" + slug})
			view, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
				Name: "temp-" + slug, TemperatureSet: true, Temperature: tc.value,
			}, now)
			if tc.wantErr {
				if !errors.Is(err, errcode.ErrModelTemperatureInvalid) {
					t.Fatalf("error = %v, want the temperature sentinel", err)
				}
				if consistencyStoredModel(t, db, id).Temperature != nil {
					t.Fatal("a rejected temperature must not persist")
				}
				return
			}
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if tc.value == nil {
				if view.Temperature != nil {
					t.Fatalf("temperature = %v, want nil", *view.Temperature)
				}
				return
			}
			if view.Temperature == nil || *view.Temperature != *tc.value {
				t.Fatalf("temperature = %v, want %v", view.Temperature, *tc.value)
			}
		})
	}

	t.Run("nil absent passes and a rejected save persists nothing else either", func(t *testing.T) {
		id := consistencyCreateModel(t, svc, modeladmin.CreateModelInput{Name: "temp-absent"})
		// A PATCH that never mentions temperature is legal on a model whose
		// candidate would not constrain anything.
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{Name: "temp-absent"}, now); err != nil {
			t.Fatalf("absent temperature must pass: %v", err)
		}
		// The same PATCH carrying an out-of-range value alongside a rename
		// must refuse as a whole — the rename cannot land while the pair is
		// self-contradictory.
		bad := 2.01
		if _, err := svc.UpdateModel(id, modeladmin.UpdateModelInput{
			Name: "temp-absent-renamed", TemperatureSet: true, Temperature: &bad,
		}, now); !errors.Is(err, errcode.ErrModelTemperatureInvalid) {
			t.Fatalf("error = %v, want the temperature sentinel", err)
		}
		if consistencyStoredModel(t, db, id).Name != "temp-absent" {
			t.Fatal("a rejected PATCH must not persist its other fields")
		}
	})
}
