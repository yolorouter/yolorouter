package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func newTestModelService(t *testing.T) (*ModelService, *gorm.DB, *fakeProviderClient) {
	t.Helper()
	providerService, db, client := newTestProviderService(t)
	_ = providerService
	return NewModelService(db, testMasterKey(), client), db, client
}

func TestCreateModelSucceeds(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()

	view, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if view.Name != "smart" {
		t.Fatalf("expected name 'smart', got %q", view.Name)
	}
	if view.RunningStatus != ModelRunningStatusNotConfigured {
		t.Fatalf("expected not_configured running status for a model with no candidates, got %q", view.RunningStatus)
	}
}

func TestCreateModelRejectsDuplicateName(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	if _, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now); err != nil {
		t.Fatalf("first CreateModel failed: %v", err)
	}
	_, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if !errors.Is(err, errcode.ErrModelNameTaken) {
		t.Fatalf("expected ErrModelNameTaken, got %v", err)
	}
}

func TestCreateModelRejectsInvalidCharacters(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.CreateModel(CreateModelInput{Name: "smart model!"}, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected an error for a model name containing spaces/punctuation")
	}
}

func TestGetModelDetailReturnsNotFoundForUnknownID(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.GetModelDetail(999999)
	if !errors.Is(err, errcode.ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
}

func seedEnabledProviderForModelTest(t *testing.T, providerService *ProviderService, name string) *ProviderView {
	t.Helper()
	provider, err := providerService.CreateProvider(context.Background(), CreateProviderInput{
		Name: name, BaseURL: "https://" + name + ".example.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	return provider
}

func TestModelRunningStatusTransitionsFromNotConfiguredToPending(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if detail.RunningStatus != ModelRunningStatusPending {
		t.Fatalf("expected pending_test running status, got %q", detail.RunningStatus)
	}
}

func TestCreateModelCandidateEnablesWhenServerReverifyPasses(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.result = TestResult{Outcome: TestSuccess, DurationMs: 5}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the candidate to be enabled after a passing server-side retest, got status %d", candidate.ManagementStatus)
	}
	if candidate.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification_status=passed, got %d", candidate.VerificationStatus)
	}
}

func TestCreateModelCandidateDefaultsProviderModelNameToModelNameWhenBlank(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.ProviderModelName != "smart" {
		t.Fatalf("expected blank provider_model_name to default to the model's own name %q, got %q", "smart", candidate.ProviderModelName)
	}
}

func TestUpdateModelCandidateDefaultsProviderModelNameToModelNameWhenBlank(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	created, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	updated, err := svc.UpdateModelCandidate(created.ID, UpdateCandidateInput{
		ProviderModelName: "", InputPrice: 3, OutputPrice: 4,
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if updated.ProviderModelName != "smart" {
		t.Fatalf("expected blank provider_model_name to default to the model's own name %q, got %q", "smart", updated.ProviderModelName)
	}
}

func TestTestCandidateMappingPreviewDefaultsProviderModelNameToModelNameWhenBlank(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.result = TestResult{Outcome: TestSuccess, DurationMs: 3}
	if _, err := svc.TestCandidateMappingPreview(context.Background(), modelView.ID, provider.ID, "", "basic"); err != nil {
		t.Fatalf("TestCandidateMappingPreview failed: %v", err)
	}
	if client.lastModel != "smart" {
		t.Fatalf("expected blank provider_model_name to resolve to the model's own name %q upstream, got %q", "smart", client.lastModel)
	}
}

func TestCreateModelCandidateFallsBackToDisabledWhenServerReverifyFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.result = TestResult{Outcome: TestAuthFailed}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the candidate to fall back to disabled when the server-side retest fails, got status %d", candidate.ManagementStatus)
	}
	if candidate.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected verification_status=failed, got %d", candidate.VerificationStatus)
	}
}

func TestCreateModelCandidateSavesDisabledWithoutServerReverifyWhenNotRequestingEnable(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	callsBefore := client.calls

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the candidate to stay disabled, got status %d", candidate.ManagementStatus)
	}
	if client.calls != callsBefore {
		t.Fatalf("expected 'save as disabled' to trigger no server-side test, calls went from %d to %d", callsBefore, client.calls)
	}
}

func TestCreateModelCandidateRejectsDuplicateProviderOnSameModel(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("first CreateModelCandidate failed: %v", err)
	}
	_, err = svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
	}, now)
	if !errors.Is(err, errcode.ErrModelCandidateProviderTaken) {
		t.Fatalf("expected ErrModelCandidateProviderTaken, got %v", err)
	}
}

func TestSetCandidateStatusRejectsEnablingUnverifiedCandidate(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	if err := svc.SetCandidateStatus(candidate.ID, true, now); !errors.Is(err, errcode.ErrModelCandidateNotVerified) {
		t.Fatalf("expected ErrModelCandidateNotVerified, got %v", err)
	}
}

func TestSetCandidateStatusEnablesAfterPassingTest(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	client.result = TestResult{Outcome: TestSuccess}
	if _, err := svc.TestModelCandidate(context.Background(), candidate.ID, "basic", now); err != nil {
		t.Fatalf("TestModelCandidate(basic) failed: %v", err)
	}
	if err := svc.SetCandidateStatus(candidate.ID, true, now); err != nil {
		t.Fatalf("SetCandidateStatus(true) failed: %v", err)
	}
}

func TestSetCandidateStatusDisableDoesNotRequireVerification(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if err := svc.SetCandidateStatus(candidate.ID, false, now); err != nil {
		t.Fatalf("SetCandidateStatus(false) failed: %v", err)
	}
}

func TestSetCandidateStatusReturnsNotFoundForUnknownCandidate(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	if err := svc.SetCandidateStatus(999999, true, time.Now().UTC()); !errors.Is(err, errcode.ErrModelCandidateNotFound) {
		t.Fatalf("expected ErrModelCandidateNotFound, got %v", err)
	}
}

func TestTestModelCandidateBasicRecordsVerificationStatus(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected the freshly-created candidate to still be untested, got %d", candidate.VerificationStatus)
	}

	client.result = TestResult{Outcome: TestSuccess, DurationMs: 8}
	updated, err := svc.TestModelCandidate(context.Background(), candidate.ID, "basic", now)
	if err != nil {
		t.Fatalf("TestModelCandidate(basic) failed: %v", err)
	}
	if updated.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification_status=passed after a passing basic test, got %d", updated.VerificationStatus)
	}
}

func TestTestModelCandidateStreamingDoesNotAffectVerificationStatus(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	client.result = TestResult{Outcome: TestSuccess}
	updated, err := svc.TestModelCandidate(context.Background(), candidate.ID, "streaming", now)
	if err != nil {
		t.Fatalf("TestModelCandidate(streaming) failed: %v", err)
	}
	if !updated.SupportsStreaming {
		t.Fatalf("expected supports_streaming=true after a passing streaming test")
	}
	if updated.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected verification_status to remain untested — streaming test must not touch it, got %d", updated.VerificationStatus)
	}
}

func TestTestModelCandidateFunctionCallingDoesNotAffectVerificationStatus(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	client.result = TestResult{Outcome: TestSuccess}
	updated, err := svc.TestModelCandidate(context.Background(), candidate.ID, "function_calling", now)
	if err != nil {
		t.Fatalf("TestModelCandidate(function_calling) failed: %v", err)
	}
	if !updated.SupportsFunctionCalling {
		t.Fatalf("expected supports_function_calling=true after a passing function-calling test")
	}
	if updated.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected verification_status to remain untested, got %d", updated.VerificationStatus)
	}
}

func TestTestModelCandidateReturnsNotFoundForUnknownCandidate(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.TestModelCandidate(context.Background(), 999999, "basic", time.Now().UTC())
	if !errors.Is(err, errcode.ErrModelCandidateNotFound) {
		t.Fatalf("expected ErrModelCandidateNotFound, got %v", err)
	}
}

func TestTestCandidateMappingPreviewDoesNotPersist(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.result = TestResult{Outcome: TestSuccess, DurationMs: 3}
	result, err := svc.TestCandidateMappingPreview(context.Background(), modelView.ID, provider.ID, "gpt-4o", "basic")
	if err != nil {
		t.Fatalf("TestCandidateMappingPreview failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess, got %v", result.Outcome)
	}

	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if len(detail.Candidates) != 0 {
		t.Fatalf("expected the preview test to persist nothing, but a candidate exists: %+v", detail.Candidates)
	}
}

func TestUpdateModelCandidate(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	updated, err := svc.UpdateModelCandidate(candidate.ID, UpdateCandidateInput{
		ProviderModelName: "gpt-4o-2024", InputPrice: 1.5, OutputPrice: 3, MaxOutput: 4096,
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if updated.ProviderModelName != "gpt-4o-2024" || updated.InputPrice != 1.5 {
		t.Fatalf("expected updated fields, got %+v", updated)
	}
}

func TestUpdateModelCandidateReturnsNotFoundForUnknownID(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.UpdateModelCandidate(999999, UpdateCandidateInput{ProviderModelName: "gpt-4o"}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrModelCandidateNotFound) {
		t.Fatalf("expected ErrModelCandidateNotFound, got %v", err)
	}
}

func TestReorderModelCandidateSwapsOrder(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	providerA := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	providerB := seedEnabledProviderForModelTest(t, providerService, "provider-b")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	first, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: providerA.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate(a) failed: %v", err)
	}
	second, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: providerB.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate(b) failed: %v", err)
	}

	if err := svc.ReorderModelCandidate(modelView.ID, second.ID, "up"); err != nil {
		t.Fatalf("ReorderModelCandidate failed: %v", err)
	}
	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if detail.Candidates[0].ID != second.ID {
		t.Fatalf("expected the second candidate to now sort first, got %+v", detail.Candidates)
	}
	_ = first
}

func TestReorderModelCandidateReturnsNotFoundForUnknownCandidate(t *testing.T) {
	svc, db, client := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if err := svc.ReorderModelCandidate(modelView.ID, 999999, "up"); !errors.Is(err, errcode.ErrModelCandidateNotFound) {
		t.Fatalf("expected ErrModelCandidateNotFound, got %v", err)
	}
	_ = db
	_ = client
}

func TestDeleteModelCandidateSucceeds(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	if err := svc.DeleteModelCandidate(candidate.ID); err != nil {
		t.Fatalf("DeleteModelCandidate failed: %v", err)
	}
	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if len(detail.Candidates) != 0 {
		t.Fatalf("expected no candidates after deletion, got %+v", detail.Candidates)
	}
}

func TestDeleteModelCandidateReturnsNotFoundForUnknownID(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	if err := svc.DeleteModelCandidate(999999); !errors.Is(err, errcode.ErrModelCandidateNotFound) {
		t.Fatalf("expected ErrModelCandidateNotFound, got %v", err)
	}
}

func TestUpdateModelNameStatusRenamesModel(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	updated, err := svc.UpdateModelNameStatus(modelView.ID, "smart-v2", now)
	if err != nil {
		t.Fatalf("UpdateModelNameStatus failed: %v", err)
	}
	if updated.Name != "smart-v2" {
		t.Fatalf("expected name 'smart-v2', got %q", updated.Name)
	}
}

func TestUpdateModelNameStatusRejectsDuplicateName(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	if _, err := svc.CreateModel(CreateModelInput{Name: "taken"}, now); err != nil {
		t.Fatalf("CreateModel(taken) failed: %v", err)
	}
	other, err := svc.CreateModel(CreateModelInput{Name: "other"}, now)
	if err != nil {
		t.Fatalf("CreateModel(other) failed: %v", err)
	}
	_, err = svc.UpdateModelNameStatus(other.ID, "taken", now)
	if !errors.Is(err, errcode.ErrModelNameTaken) {
		t.Fatalf("expected ErrModelNameTaken, got %v", err)
	}
}

func TestUpdateModelNameStatusReturnsNotFoundForUnknownID(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.UpdateModelNameStatus(999999, "whatever", time.Now().UTC())
	if !errors.Is(err, errcode.ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
}

func TestSetModelStatusTogglesStatus(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if err := svc.SetModelStatus(modelView.ID, false, now); err != nil {
		t.Fatalf("SetModelStatus(false) failed: %v", err)
	}
	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if detail.ManagementStatus != model.ModelStatusDisabled {
		t.Fatalf("expected management_status=disabled, got %d", detail.ManagementStatus)
	}
}

func TestSetModelStatusReturnsNotFoundForUnknownID(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	if err := svc.SetModelStatus(999999, true, time.Now().UTC()); !errors.Is(err, errcode.ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
}

func TestComputeModelRunningStatusAvailableWhenFirstCandidateRoutable(t *testing.T) {
	status := computeModelRunningStatus([]CandidateView{
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: true},
		{VerificationStatus: model.ModelVerificationStatusUntested, Routable: false},
	})
	if status != ModelRunningStatusAvailable {
		t.Fatalf("expected available, got %q", status)
	}
}

func TestComputeModelRunningStatusDegradedWhenOnlyLaterCandidateRoutable(t *testing.T) {
	status := computeModelRunningStatus([]CandidateView{
		{VerificationStatus: model.ModelVerificationStatusFailed, Routable: false},
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: true},
	})
	if status != ModelRunningStatusDegraded {
		t.Fatalf("expected degraded, got %q", status)
	}
}

func TestComputeModelRunningStatusUnavailableWhenNoCandidateRoutable(t *testing.T) {
	status := computeModelRunningStatus([]CandidateView{
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: false},
		{VerificationStatus: model.ModelVerificationStatusFailed, Routable: false},
	})
	if status != ModelRunningStatusUnavailable {
		t.Fatalf("expected unavailable, got %q", status)
	}
}

func TestIsCandidateRoutableRejectsDisabledCandidate(t *testing.T) {
	c := model.ModelCandidate{ManagementStatus: model.ModelCandidateStatusDisabled, ProviderModelName: "gpt-4o", VerificationStatus: model.ModelVerificationStatusPassed}
	if isCandidateRoutable(c, true, true) {
		t.Fatalf("expected false for a disabled candidate")
	}
}

func TestIsCandidateRoutableRejectsDisabledProvider(t *testing.T) {
	c := model.ModelCandidate{ManagementStatus: model.ModelCandidateStatusEnabled, ProviderModelName: "gpt-4o", VerificationStatus: model.ModelVerificationStatusPassed}
	if isCandidateRoutable(c, false, true) {
		t.Fatalf("expected false when the provider is disabled")
	}
}

func TestIsCandidateRoutableRejectsNoAvailableKey(t *testing.T) {
	c := model.ModelCandidate{ManagementStatus: model.ModelCandidateStatusEnabled, ProviderModelName: "gpt-4o", VerificationStatus: model.ModelVerificationStatusPassed}
	if isCandidateRoutable(c, true, false) {
		t.Fatalf("expected false when the provider has no available key")
	}
}

func TestIsCandidateRoutableRejectsEmptyProviderModelName(t *testing.T) {
	c := model.ModelCandidate{ManagementStatus: model.ModelCandidateStatusEnabled, ProviderModelName: "", VerificationStatus: model.ModelVerificationStatusPassed}
	if isCandidateRoutable(c, true, true) {
		t.Fatalf("expected false for an empty provider_model_name")
	}
}

func TestIsCandidateRoutableRejectsUnverifiedCandidate(t *testing.T) {
	c := model.ModelCandidate{ManagementStatus: model.ModelCandidateStatusEnabled, ProviderModelName: "gpt-4o", VerificationStatus: model.ModelVerificationStatusUntested}
	if isCandidateRoutable(c, true, true) {
		t.Fatalf("expected false for an unverified candidate")
	}
}

func TestIsCandidateRoutableAcceptsFullyQualifiedCandidate(t *testing.T) {
	c := model.ModelCandidate{ManagementStatus: model.ModelCandidateStatusEnabled, ProviderModelName: "gpt-4o", VerificationStatus: model.ModelVerificationStatusPassed}
	if !isCandidateRoutable(c, true, true) {
		t.Fatalf("expected true when every condition is satisfied")
	}
}

func TestProviderHasAvailableKeyReturnsFalseWhenNoKeyQualifies(t *testing.T) {
	keys := []model.ProviderKey{
		{ManagementStatus: model.ProviderKeyStatusDisabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: 1},
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusFailed, AuthorizedDestinationVersion: 1},
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: 2},
	}
	if providerHasAvailableKey(keys, 1) {
		t.Fatalf("expected false when no key satisfies all three conditions")
	}
}

func TestModelRunningStatusAvailableEndToEnd(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if detail.RunningStatus != ModelRunningStatusAvailable {
		t.Fatalf("expected available, got %q", detail.RunningStatus)
	}
}

func TestGetModelDetailErrorsWhenDBUnavailable(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	dropTable(t, db, "models")
	if _, err := svc.GetModelDetail(modelView.ID); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestCreateModelErrorsWhenNameLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	dropTable(t, db, "models")
	if _, err := svc.CreateModel(CreateModelInput{Name: "smart"}, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestCreateModelCandidateErrorsWhenModelLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	dropTable(t, db, "models")
	_, err := svc.CreateModelCandidate(context.Background(), 1, CreateCandidateInput{ProviderID: 1, ProviderModelName: "gpt-4o"}, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestTestCandidateMappingPreviewReturnsNotFoundForUnknownProvider(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	_, err = svc.TestCandidateMappingPreview(context.Background(), modelView.ID, 999999, "gpt-4o", "basic")
	if !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestTestCandidateMappingPreviewErrorsWhenNoTestableKey(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	// A provider created without requesting enabled leaves its only key
	// disabled/untested — no key qualifies as "available" for a test.
	provider, err := providerService.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-a", BaseURL: "https://a.example.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	_, err = svc.TestCandidateMappingPreview(context.Background(), modelView.ID, provider.ID, "gpt-4o", "basic")
	if !errors.Is(err, errcode.ErrProviderNoTestableModel) {
		t.Fatalf("expected ErrProviderNoTestableModel, got %v", err)
	}
}

func TestCreateModelCandidateStaysDisabledWhenNoTestableKeyForServerReverify(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	// Same "no available key" setup as above — requesting Enabled should
	// silently fail the best-effort server-side retest and leave the
	// candidate Disabled/Untested rather than erroring out the whole
	// CreateModelCandidate call (the candidate row itself still saves).
	provider, err := providerService.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-a", BaseURL: "https://a.example.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the candidate to stay disabled when no key is testable, got status %d", candidate.ManagementStatus)
	}
}

func TestTestModelCandidateErrorsForUnknownTestType(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if _, err := svc.TestModelCandidate(context.Background(), candidate.ID, "bogus", now); err == nil {
		t.Fatalf("expected an error for an unrecognized test_type")
	}
}

func TestUpdateModelCandidateErrorsWhenUpdateFailsForNonUniqueReason(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	blockTableWrites(t, db, "model_candidates", "UPDATE")
	if _, err := svc.UpdateModelCandidate(candidate.ID, UpdateCandidateInput{ProviderModelName: "gpt-4o-2"}, now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestDeleteModelCandidateErrorsWhenDeleteFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	blockTableWrites(t, db, "model_candidates", "DELETE")
	if err := svc.DeleteModelCandidate(candidate.ID); err == nil {
		t.Fatalf("expected an error when the DELETE statement fails")
	}
}

func TestUpdateModelNameStatusRejectsInvalidCharacters(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.UpdateModelNameStatus(modelView.ID, "bad name!", now); err == nil {
		t.Fatalf("expected an error for an invalid model name")
	}
}

func TestUpdateModelNameStatusErrorsWhenUpdateFailsForNonUniqueReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	blockTableWrites(t, db, "models", "UPDATE")
	if _, err := svc.UpdateModelNameStatus(modelView.ID, "smart-v2", now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestSetModelStatusErrorsWhenUpdateFails(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	blockTableWrites(t, db, "models", "UPDATE")
	if err := svc.SetModelStatus(modelView.ID, true, now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestToModelViewErrorsWhenProviderKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	dropTable(t, db, "provider_keys")

	if _, err := svc.GetModelDetail(modelView.ID); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestListModelsErrorsWhenModelsTableMissing(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	dropTable(t, db, "models")
	if _, err := svc.ListModels(); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestListModelsErrorsWhenModelCandidatesTableMissing(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	if _, err := svc.CreateModel(CreateModelInput{Name: "smart"}, time.Now().UTC()); err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	dropTable(t, db, "model_candidates")
	if _, err := svc.ListModels(); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestListModelsErrorsWhenProviderKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	dropTable(t, db, "provider_keys")

	if _, err := svc.ListModels(); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestGetModelDetailErrorsWhenModelCandidatesTableMissing(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	dropTable(t, db, "model_candidates")
	if _, err := svc.GetModelDetail(modelView.ID); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestCreateModelErrorsWhenInsertFailsForNonUniqueReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	blockTableWrites(t, db, "models", "INSERT")
	if _, err := svc.CreateModel(CreateModelInput{Name: "smart"}, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the INSERT statement fails")
	}
}

func TestTestCandidateMappingPreviewErrorsWhenKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	dropTable(t, db, "provider_keys")

	if _, err := svc.TestCandidateMappingPreview(context.Background(), modelView.ID, provider.ID, "gpt-4o", "basic"); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestCreateModelCandidateErrorsWhenSortOrderLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	dropTable(t, db, "model_candidates")

	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o",
	}, now); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestCreateModelCandidateErrorsWhenInsertFailsForNonUniqueReason(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	blockTableWrites(t, db, "model_candidates", "INSERT")

	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o",
	}, now); err == nil {
		t.Fatalf("expected an error when the INSERT statement fails")
	}
}

func TestToCandidateViewErrorsWhenProviderKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	dropTable(t, db, "provider_keys")

	if _, err := svc.UpdateModelCandidate(candidate.ID, UpdateCandidateInput{ProviderModelName: "gpt-4o-2"}, now); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestUpdateModelCandidateErrorsWhenReloadFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	dropTable(t, db, "model_candidates")

	if _, err := svc.UpdateModelCandidate(candidate.ID, UpdateCandidateInput{}, now); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestSetCandidateStatusErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	dropTable(t, db, "model_candidates")
	if err := svc.SetCandidateStatus(1, true, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestTestModelCandidateErrorsWhenProviderLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	dropTable(t, db, "provider_keys")
	dropTable(t, db, "providers")

	if _, err := svc.TestModelCandidate(context.Background(), candidate.ID, "basic", now); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestTestModelCandidateErrorsWhenCommitFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess}
	blockTableWrites(t, db, "model_candidates", "UPDATE")

	if _, err := svc.TestModelCandidate(context.Background(), candidate.ID, "basic", now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestTestModelCandidateStreamingErrorsWhenCommitFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess}
	blockTableWrites(t, db, "model_candidates", "UPDATE")

	if _, err := svc.TestModelCandidate(context.Background(), candidate.ID, "streaming", now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestReorderModelCandidateReturnsRawErrorForOtherFailures(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	dropTable(t, db, "model_candidates")
	if err := svc.ReorderModelCandidate(1, 1, "up"); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestDeleteModelCandidateErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	dropTable(t, db, "model_candidates")
	if err := svc.DeleteModelCandidate(1); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestUpdateModelNameStatusErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	dropTable(t, db, "models")
	if _, err := svc.UpdateModelNameStatus(1, "smart", time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestSetModelStatusErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	dropTable(t, db, "models")
	if err := svc.SetModelStatus(1, true, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestTestCandidateMappingPreviewErrorsWhenProviderLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	dropTable(t, db, "providers")
	if _, err := svc.TestCandidateMappingPreview(context.Background(), modelView.ID, 1, "gpt-4o", "basic"); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestCreateModelCandidateErrorsWhenModelLookupFailsForOtherReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	if _, err := svc.CreateModel(CreateModelInput{Name: "smart"}, time.Now().UTC()); err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	dropTable(t, db, "models")
	if _, err := svc.CreateModelCandidate(context.Background(), 1, CreateCandidateInput{ProviderID: 1, ProviderModelName: "gpt-4o"}, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestCreateModelCandidateSilentlySkipsReverifyWhenClientErrors(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	client.err = errors.New("client refused the call")

	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the candidate to stay disabled when the client call errors, got status %d", candidate.ManagementStatus)
	}
}

func TestCreateModelCandidateSilentlySkipsReverifyWhenCommitFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess}
	blockTableWrites(t, db, "model_candidates", "UPDATE")

	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err != nil {
		t.Fatalf("expected CreateModelCandidate itself to still succeed (the candidate row insert isn't blocked), got %v", err)
	}
}

func TestToCandidateViewErrorsWhenProviderLookupFailsForNonNotFoundReason(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	dropTable(t, db, "provider_keys")
	dropTable(t, db, "providers")

	if _, err := svc.UpdateModelCandidate(candidate.ID, UpdateCandidateInput{ProviderModelName: "gpt-4o-2"}, now); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestSetCandidateStatusErrorsWhenCASWriteFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	client.result = TestResult{Outcome: TestSuccess}
	if _, err := svc.TestModelCandidate(context.Background(), candidate.ID, "basic", now); err != nil {
		t.Fatalf("TestModelCandidate failed: %v", err)
	}
	blockTableWrites(t, db, "model_candidates", "UPDATE")

	if err := svc.SetCandidateStatus(candidate.ID, true, now); err == nil {
		t.Fatalf("expected an error when the CAS UPDATE statement fails")
	}
}

func TestTestModelCandidateErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	dropTable(t, db, "model_candidates")
	if _, err := svc.TestModelCandidate(context.Background(), 1, "basic", time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestTestModelCandidateErrorsWhenProviderKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	dropTable(t, db, "provider_keys")

	if _, err := svc.TestModelCandidate(context.Background(), candidate.ID, "basic", now); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestTestModelCandidateErrorsWhenNoTestableKey(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	// A provider created without requesting enabled leaves its only key
	// disabled/untested — no key qualifies as available for a test.
	provider, err := providerService.CreateProvider(context.Background(), CreateProviderInput{
		Name: "provider-a", BaseURL: "https://a.example.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	svc := NewModelService(db, testMasterKey(), client)
	modelView, err := svc.CreateModel(CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	if _, err := svc.TestModelCandidate(context.Background(), candidate.ID, "basic", now); !errors.Is(err, errcode.ErrProviderNoTestableModel) {
		t.Fatalf("expected ErrProviderNoTestableModel, got %v", err)
	}
}

func TestListModelsAvoidsNPlusOneCandidateQueries(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := NewModelService(db, testMasterKey(), client)
	for _, name := range []string{"smart", "fast"} {
		modelView, err := svc.CreateModel(CreateModelInput{Name: name}, now)
		if err != nil {
			t.Fatalf("CreateModel(%s) failed: %v", name, err)
		}
		if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, CreateCandidateInput{
			ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		}, now); err != nil {
			t.Fatalf("CreateModelCandidate(%s) failed: %v", name, err)
		}
	}

	views, err := svc.ListModels()
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("expected 2 models, got %d", len(views))
	}
	for _, v := range views {
		if len(v.Candidates) != 1 {
			t.Fatalf("expected each model to have exactly 1 candidate, got %+v", v)
		}
	}
}
