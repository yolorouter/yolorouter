package modeladmin_test

// Billing-declaration tests at the service boundary: what create accepts and
// stores, what update replaces or leaves alone, and what the write path
// refuses — image mode without a table that prices.

import (
	"errors"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func createModelWithCandidate(t *testing.T, svc *modeladmin.ModelService, providerID uint, name string) (modelID, candidateID uint) {
	t.Helper()
	now := time.Now().UTC()
	created, err := svc.CreateModel(modeladmin.CreateModelInput{Name: name}, now)
	if err != nil {
		t.Fatalf("create model: %v", err)
	}
	view, err := svc.CreateModelCandidate(t.Context(), created.ID, modeladmin.CreateCandidateInput{
		ProviderID: providerID, ProviderModelName: name + "-real",
	}, now)
	if err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	return created.ID, view.ID
}

func TestCandidateBillingDeclaration(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	providerA := seedEnabledProviderForModelTest(t, providerService, "billing-a")
	providerB := seedEnabledProviderForModelTest(t, providerService, "billing-b")

	t.Run("image mode with a table is stored and served", func(t *testing.T) {
		modelID, _ := createModelWithCandidate(t, svc, providerA.ID, "priced-one")
		view, err := svc.CreateModelCandidate(t.Context(), modelID, modeladmin.CreateCandidateInput{
			ProviderID: providerB.ID, ProviderModelName: "priced-one-real-2",
			BillingMode: model.BillingModeImage,
			ImagePricingTiers: &model.ImagePricingTiers{
				Tiers: []model.ImagePricingTier{{Quality: "high", Price: 0.19}},
			},
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if view.BillingMode != model.BillingModeImage {
			t.Fatalf("view billing mode = %q, want image", view.BillingMode)
		}
		if view.ImagePricingTiers == nil || len(view.ImagePricingTiers.Tiers) != 1 {
			t.Fatalf("view tiers = %+v, want the stored table", view.ImagePricingTiers)
		}
		var stored model.ModelCandidate
		if err := db.Where("id = ?", view.ID).First(&stored).Error; err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got, _, ok := model.ParseImagePricingTiers(stored.ImagePricingTiers).ResolvePrice("high", ""); !ok || got != 0.19 {
			t.Fatalf("stored table does not resolve high: %v,%v", got, ok)
		}
	})

	t.Run("image mode without a table is refused", func(t *testing.T) {
		modelID, _ := createModelWithCandidate(t, svc, providerA.ID, "priced-two")
		_, err := svc.CreateModelCandidate(t.Context(), modelID, modeladmin.CreateCandidateInput{
			ProviderID: providerB.ID, BillingMode: model.BillingModeImage,
		}, time.Now().UTC())
		if !errors.Is(err, errcode.ErrModelBillingInvalid) {
			t.Fatalf("error = %v, want the billing sentinel", err)
		}
	})

	t.Run("unknown mode is refused", func(t *testing.T) {
		modelID, _ := createModelWithCandidate(t, svc, providerA.ID, "priced-three")
		_, err := svc.CreateModelCandidate(t.Context(), modelID, modeladmin.CreateCandidateInput{
			ProviderID: providerB.ID, BillingMode: "per_pixel",
		}, time.Now().UTC())
		if !errors.Is(err, errcode.ErrModelBillingInvalid) {
			t.Fatalf("error = %v, want the billing sentinel", err)
		}
	})

	t.Run("update switches mode and keeps or replaces the table", func(t *testing.T) {
		_, candidateID := createModelWithCandidate(t, svc, providerA.ID, "priced-four")

		// Switch to image mode carrying the table in the same edit.
		mode := model.BillingModeImage
		result, err := svc.UpdateModelCandidate(t.Context(), candidateID, modeladmin.UpdateCandidateInput{
			ProviderModelName: "priced-four-real",
			BillingMode:       &mode,
			ImagePricingTiers: &model.ImagePricingTiers{DefaultPrice: ptrFloat(0.02)},
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("update to image: %v", err)
		}
		if result.Candidate.BillingMode != model.BillingModeImage || result.Candidate.ImagePricingTiers == nil {
			t.Fatalf("update result = %+v, want image mode with a table", result.Candidate)
		}

		// A later edit that submits neither half keeps the declaration.
		result, err = svc.UpdateModelCandidate(t.Context(), candidateID, modeladmin.UpdateCandidateInput{
			ProviderModelName: "priced-four-real",
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("plain update: %v", err)
		}
		if result.Candidate.BillingMode != model.BillingModeImage || result.Candidate.ImagePricingTiers == nil {
			t.Fatalf("plain update dropped the billing declaration: %+v", result.Candidate)
		}

		// Switching back to token clears the requirement, and the stored
		// table is rewritten only because the declaration as a whole was
		// submitted — token mode has no table.
		token := model.BillingModeToken
		result, err = svc.UpdateModelCandidate(t.Context(), candidateID, modeladmin.UpdateCandidateInput{
			ProviderModelName: "priced-four-real",
			BillingMode:       &token,
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("update to token: %v", err)
		}
		if result.Candidate.BillingMode != model.BillingModeToken {
			t.Fatalf("billing mode = %q, want token", result.Candidate.BillingMode)
		}
	})
}

func ptrFloat(v float64) *float64 { return &v }

func TestCandidateVideoBillingDeclaration(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	providerA := seedEnabledProviderForModelTest(t, providerService, "video-billing-a")
	createModelOnly := func(t *testing.T, name string) uint {
		t.Helper()
		created, err := svc.CreateModel(modeladmin.CreateModelInput{Name: name}, time.Now().UTC())
		if err != nil {
			t.Fatalf("create model: %v", err)
		}
		return created.ID
	}

	t.Run("video mode without a table is refused", func(t *testing.T) {
		modelID := createModelOnly(t, "video-priced-one")
		_, err := svc.CreateModelCandidate(t.Context(), modelID, modeladmin.CreateCandidateInput{
			ProviderID: providerA.ID, ProviderModelName: "video-priced-one-real",
			BillingMode: model.BillingModeVideo,
		}, time.Now().UTC())
		if !errors.Is(err, errcode.ErrModelBillingInvalid) {
			t.Fatalf("video mode without a table must be refused with the billing error, got %v", err)
		}
	})

	t.Run("video mode with tiers is stored, served, and editable", func(t *testing.T) {
		modelID := createModelOnly(t, "video-priced-two")
		view, err := svc.CreateModelCandidate(t.Context(), modelID, modeladmin.CreateCandidateInput{
			ProviderID: providerA.ID, ProviderModelName: "video-priced-two-real",
			BillingMode: model.BillingModeVideo,
			VideoPricingTiers: &model.VideoPricingTiers{
				Tiers: []model.VideoPricingTier{
					{Resolution: "", PurchasePrice: 0.4, SellPrice: 0.5},
					{Resolution: "1080P", PurchasePrice: 0.8, SellPrice: 1.0},
				},
			},
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if view.BillingMode != model.BillingModeVideo || view.VideoPricingTiers == nil || len(view.VideoPricingTiers.Tiers) != 2 {
			t.Fatalf("view must carry the video declaration, got %+v", view)
		}

		video := model.BillingModeVideo
		result, err := svc.UpdateModelCandidate(t.Context(), view.ID, modeladmin.UpdateCandidateInput{
			ProviderModelName: "video-priced-two-real",
			BillingMode:       &video,
			VideoPricingTiers: &model.VideoPricingTiers{
				Tiers: []model.VideoPricingTier{{Resolution: "720P", PurchasePrice: 0.3, SellPrice: 0.4}},
			},
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if result.Candidate.VideoPricingTiers == nil || len(result.Candidate.VideoPricingTiers.Tiers) != 1 ||
			result.Candidate.VideoPricingTiers.Tiers[0].Resolution != "720P" {
			t.Fatalf("update must replace the table, got %+v", result.Candidate.VideoPricingTiers)
		}

		// An edit that submits neither half keeps the declaration.
		result, err = svc.UpdateModelCandidate(t.Context(), view.ID, modeladmin.UpdateCandidateInput{
			ProviderModelName: "video-priced-two-real",
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("plain update: %v", err)
		}
		if result.Candidate.BillingMode != model.BillingModeVideo || result.Candidate.VideoPricingTiers == nil {
			t.Fatalf("plain update dropped the video declaration: %+v", result.Candidate)
		}

		// Switching to token clears the table — token mode has none.
		token := model.BillingModeToken
		result, err = svc.UpdateModelCandidate(t.Context(), view.ID, modeladmin.UpdateCandidateInput{
			ProviderModelName: "video-priced-two-real",
			BillingMode:       &token,
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("update to token: %v", err)
		}
		if result.Candidate.BillingMode != model.BillingModeToken || result.Candidate.VideoPricingTiers != nil {
			t.Fatalf("token mode must leave no video table, got %+v", result.Candidate)
		}
	})
}
