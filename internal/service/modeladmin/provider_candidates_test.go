package modeladmin_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func TestListProviderCandidatesReturnsMappingsWithModelNamesAndProbeState(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	other := seedEnabledProviderForModelTest(t, providerService, "prov-b")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	// Two mappings for prov-a in a known state each — one probed and failed
	// (reason persisted), one untouched — plus one for another provider that
	// must not leak into prov-a's listing.
	failedID := seedUntestedCandidate(t, svc, db, prov.ID, "vendor/failing-model")
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound, Detail: "upstream returned 404"}
	if err := svc.ProbeQueuedCandidate(t.Context(), failedID, time.Now().UTC()); err != nil {
		t.Fatalf("seed probe failed: %v", err)
	}
	untestedID := seedUntestedCandidate(t, svc, db, prov.ID, "vendor/untested-model")
	seedUntestedCandidate(t, svc, db, other.ID, "vendor/other-provider-model")

	list, err := svc.ListProviderCandidates(prov.ID)
	if err != nil {
		t.Fatalf("ListProviderCandidates failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected exactly prov-a's two mappings, got %+v", list)
	}
	byID := map[uint]modeladmin.ProviderCandidateView{}
	for _, v := range list {
		byID[v.CandidateID] = v
	}

	failed := byID[failedID]
	if failed.ModelName != "vendor/failing-model" || failed.ProviderModelName != "vendor/failing-model" {
		t.Fatalf("expected the mapping to carry its model name, got %+v", failed)
	}
	if failed.VerificationStatus != model.ModelVerificationStatusFailed || failed.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the probed-and-failed state, got %+v", failed)
	}
	if failed.LastTestError == nil || *failed.LastTestError != "upstream returned 404" {
		t.Fatalf("expected the persisted failure reason, got %+v", failed.LastTestError)
	}
	if failed.ModelID == 0 {
		t.Fatalf("expected the model id for the retest route, got %+v", failed)
	}

	untested := byID[untestedID]
	if untested.VerificationStatus != model.ModelVerificationStatusUntested || untested.LastTestError != nil {
		t.Fatalf("expected a pristine untested mapping, got %+v", untested)
	}
	if untested.InputPrice != 1 || untested.OutputPrice != 2 {
		t.Fatalf("expected prices carried through, got %+v", untested)
	}
}

func TestListProviderCandidatesEmptyProviderReturnsEmptyList(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	list, err := svc.ListProviderCandidates(prov.ID)
	if err != nil {
		t.Fatalf("ListProviderCandidates failed: %v", err)
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("expected an empty (non-nil) list, got %#v", list)
	}
}

func TestListProviderCandidatesProviderNotFound(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.ListProviderCandidates(9999)
	if !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

// The list view carries the billing declaration so a price column can price
// an image-billed mapping off its tier table instead of the (inert) per-M
// token slots.
func TestListProviderCandidatesCarriesBillingDeclaration(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "billing-view-provider")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	view, err := svc.CreateModel(modeladmin.CreateModelInput{
		Name: "billing-view-model", OutputModalities: []string{model.OutputModalityImage},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("create image model: %v", err)
	}
	tiers := &model.ImagePricingTiers{Mode: "per_image", DefaultPrice: ptrFloat(0.25)}
	if _, err := svc.CreateModelCandidate(t.Context(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: prov.ID, ProviderModelName: "billing-view-model",
		InputPrice: 0.3, OutputPrice: 1.2, BillingMode: model.BillingModeImage, ImagePricingTiers: tiers,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("create candidate: %v", err)
	}

	list, err := svc.ListProviderCandidatesWithQueueStates(prov.ID, nil)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one row, got %+v", list)
	}
	row := list[0]
	if row.BillingMode != model.BillingModeImage {
		t.Fatalf("billing mode not carried, got %q", row.BillingMode)
	}
	if row.ImagePricingTiers == nil || row.ImagePricingTiers.DefaultPrice == nil || *row.ImagePricingTiers.DefaultPrice != 0.25 {
		t.Fatalf("tier table not carried, got %+v", row.ImagePricingTiers)
	}
}
