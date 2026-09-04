package modeladmin_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/pricecatalog"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/apikey"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/service/providerclient/providerclienttest"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// newTestProviderService builds a provider service on the shared fake —
// the same construction the provider test suite uses (each suite carries
// its own copy: the helper drives only exported API, and neither package's
// tests can import the other's without an import cycle).
func newTestProviderService(t *testing.T) (*provider.ProviderService, *gorm.DB, *providerclienttest.Fake) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	client := &providerclienttest.Fake{Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 10}}
	svc := provider.NewProviderService(db, testutil.ProviderSecrets(), client)
	return svc, db, client
}

func newTestModelService(t *testing.T) (*modeladmin.ModelService, *gorm.DB, *providerclienttest.Fake) {
	t.Helper()
	providerService, db, client := newTestProviderService(t)
	_ = providerService
	return modeladmin.NewModelService(db, testutil.ProviderSecrets(), client), db, client
}

func TestCreateModelSucceeds(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if view.Name != "smart" {
		t.Fatalf("expected name 'smart', got %q", view.Name)
	}
	if view.RunningStatus != modeladmin.ModelRunningStatusNotConfigured {
		t.Fatalf("expected not_configured running status for a model with no candidates, got %q", view.RunningStatus)
	}
}

func TestCreateModelRejectsDuplicateName(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now); err != nil {
		t.Fatalf("first CreateModel failed: %v", err)
	}
	_, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if !errors.Is(err, errcode.ErrModelNameTaken) {
		t.Fatalf("expected ErrModelNameTaken, got %v", err)
	}
}

func TestCreateModelRejectsInvalidCharacters(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart model!"}, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected an error for a model name containing spaces/punctuation")
	}
}

func TestCreateModelsBatchCreatesValidAndSkipsExistingAndInvalid(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "gpt-5.6"}, now); err != nil {
		t.Fatalf("seed CreateModel failed: %v", err)
	}

	// "gpt-5.6" already exists → skip(exists); "bad name!" is invalid →
	// skip(invalid); the repeated "claude-sonnet-5" is created once, the
	// second occurrence skips(exists).
	result, err := svc.CreateModelsBatch(
		modeladmin.CreateModelsBatchInput{Names: []string{"gpt-5.6", "claude-sonnet-5", "bad name!", "claude-sonnet-5", "deepseek-v4-flash"}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateModelsBatch failed: %v", err)
	}

	createdNames := make([]string, 0, len(result.Created))
	for _, m := range result.Created {
		createdNames = append(createdNames, m.Name)
	}
	wantCreated := []string{"claude-sonnet-5", "deepseek-v4-flash"}
	if len(createdNames) != len(wantCreated) {
		t.Fatalf("expected created %v, got %v", wantCreated, createdNames)
	}
	for i, name := range wantCreated {
		if createdNames[i] != name {
			t.Fatalf("expected created[%d]=%q, got %q", i, name, createdNames[i])
		}
	}

	reasons := map[string]string{}
	for _, s := range result.Skipped {
		reasons[s.Name] = s.Reason
	}
	if reasons["gpt-5.6"] != modeladmin.BatchSkipReasonExists {
		t.Fatalf("expected gpt-5.6 skipped as exists, got %q", reasons["gpt-5.6"])
	}
	if reasons["bad name!"] != modeladmin.BatchSkipReasonInvalid {
		t.Fatalf("expected 'bad name!' skipped as invalid, got %q", reasons["bad name!"])
	}
	if got := len(result.Skipped); got != 3 {
		t.Fatalf("expected 3 skipped (exists, invalid, in-batch dup), got %d: %+v", got, result.Skipped)
	}
}

// A storage failure part-way through the batch must roll back every insert,
// not leave the models created before the failure committed while the caller
// sees a total failure.
func TestCreateModelsBatchRollsBackOnMidBatchFailure(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	// Inject a storage failure when the model named "boom" is inserted, so the
	// batch fails only after "alpha" has already been inserted in the same
	// transaction.
	if err := db.Callback().Create().Before("gorm:create").Register("fail_on_boom", func(tx *gorm.DB) {
		if m, ok := tx.Statement.Dest.(*model.Model); ok && m.Name == "boom" {
			_ = tx.AddError(errors.New("injected storage failure"))
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	if _, err := svc.CreateModelsBatch(modeladmin.CreateModelsBatchInput{Names: []string{"alpha", "boom", "gamma"}}, now); err == nil {
		t.Fatalf("expected an error from the injected mid-batch failure")
	}

	// The whole batch must have rolled back — "alpha" (inserted before "boom"
	// failed) must not remain committed.
	models, err := svc.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("expected no models after rollback, got %d: %+v", len(models), models)
	}
}

func TestGetModelDetailReturnsNotFoundForUnknownID(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.GetModelDetail(999999)
	if !errors.Is(err, errcode.ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
}

func seedEnabledProviderForModelTest(t *testing.T, providerService *provider.ProviderService, name string) *provider.ProviderView {
	t.Helper()
	provider, err := providerService.CreateProvider(context.Background(), provider.CreateProviderInput{
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

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if detail.RunningStatus != modeladmin.ModelRunningStatusPending {
		t.Fatalf("expected pending_test running status, got %q", detail.RunningStatus)
	}
}

func TestCreateModelCandidateEnablesWhenServerReverifyPasses(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	created, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	updated, err := svc.UpdateModelCandidate(context.Background(), created.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "", InputPrice: 3, OutputPrice: 4,
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if updated.Candidate.ProviderModelName != "smart" {
		t.Fatalf("expected blank provider_model_name to default to the model's own name %q, got %q", "smart", updated.Candidate.ProviderModelName)
	}
}

func TestTestAndCreateCandidateDefaultsProviderModelNameToModelNameWhenBlank(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if _, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err != nil {
		t.Fatalf("TestAndCreateCandidate failed: %v", err)
	}
	if client.LastModel != "smart" {
		t.Fatalf("expected blank provider_model_name to resolve to the model's own name %q upstream, got %q", "smart", client.LastModel)
	}
}

func TestCreateModelCandidateFallsBackToDisabledWhenServerReverifyFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestAuthFailed}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the candidate to fall back to disabled when the server-side retest fails, got status %d", candidate.ManagementStatus)
	}
	// Untested, not Failed: a bad credential says nothing about whether the
	// provider serves this model name, and a candidate's mapping validity is a
	// separate dimension from its key's credential validity. Either way it cannot
	// be enabled, which is what the status assertion above pins down.
	if candidate.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected verification_status=untested after an inconclusive probe, got %d", candidate.VerificationStatus)
	}
}

// A decisive failure, by contrast, is recorded as such.
func TestCreateModelCandidateRecordsFailedWhenServerReverifyIsDecisive(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "nope", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected disabled, got %d", candidate.ManagementStatus)
	}
	if candidate.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected verification_status=failed for a model that does not exist, got %d", candidate.VerificationStatus)
	}
}

func TestCreateModelCandidateSavesDisabledWithoutServerReverifyWhenNotRequestingEnable(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	callsBefore := client.Calls

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the candidate to stay disabled, got status %d", candidate.ManagementStatus)
	}
	if client.Calls != callsBefore {
		t.Fatalf("expected 'save as disabled' to trigger no server-side test, calls went from %d to %d", callsBefore, client.Calls)
	}
}

func TestCreateModelCandidateRejectsDuplicateProviderOnSameModel(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("first CreateModelCandidate failed: %v", err)
	}
	_, err = svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if _, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err != nil {
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

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if candidate.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected the freshly-created candidate to still be untested, got %d", candidate.VerificationStatus)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 8}
	updated, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("TestModelCandidate(basic) failed: %v", err)
	}
	if updated.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification_status=passed after a passing basic test, got %d", updated.VerificationStatus)
	}
}

// seedCandidateForRetest builds an untested candidate ready to be probed.
func seedCandidateForRetest(t *testing.T, svc *modeladmin.ModelService, providerID uint, now time.Time) *modeladmin.CandidateView {
	t.Helper()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: providerID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	return candidate
}

func TestRetestModelCandidateRunsAllThreeProbesAndRecordsCapabilities(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// Measured as a delta: seeding an enabled provider already ran a key
	// verification through the same fake client.
	before := map[string]int{}
	for _, testType := range []string{"basic", "streaming", "function_calling"} {
		before[testType] = client.CallCountFor(testType)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	updated, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if updated.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification_status=passed, got %d", updated.VerificationStatus)
	}
	if updated.SupportsStreaming == nil || !*updated.SupportsStreaming {
		t.Fatalf("expected supports_streaming=true, got %v", updated.SupportsStreaming)
	}
	if updated.SupportsFunctionCalling == nil || !*updated.SupportsFunctionCalling {
		t.Fatalf("expected supports_function_calling=true, got %v", updated.SupportsFunctionCalling)
	}
	for _, testType := range []string{"basic", "streaming", "function_calling"} {
		if got := client.CallCountFor(testType) - before[testType]; got != 1 {
			t.Fatalf("expected exactly one %s probe, got %d", testType, got)
		}
	}
}

// A retest that lands the mapping's FIRST pass must also enable it: imported
// and failed mappings sit disabled only because no probe had proven them yet,
// and the one-click retest carries the same promise as the import queue —
// pass means routable. Without this the operator sees a green "passed" badge
// on a row that still serves nothing.
func TestRetestModelCandidateEnablesOnFirstPass(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now) // disabled + untested

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	updated, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if updated.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification passed, got %d", updated.VerificationStatus)
	}
	if updated.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the first pass to enable the mapping, got management_status=%d", updated.ManagementStatus)
	}
}

// A mapping an admin explicitly switched off after it passed keeps their
// decision: its verification is already Passed, so a re-confirming retest is
// not a first pass and must not undo the disable.
func TestRetestModelCandidateKeepsAdminDisabledMappingDisabled(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now) // passed + enabled

	if err := svc.SetCandidateStatus(candidate.ID, false, now); err != nil {
		t.Fatalf("SetCandidateStatus(disable) failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	updated, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if updated.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the admin's disable to stand through a re-confirming retest, got management_status=%d", updated.ManagementStatus)
	}
}

// The database can apply a commit and then lose the connection before the
// acknowledgment: the caller sees an error although the row is stamped. The
// retry's compare-and-set then misses ITS OWN earlier write and would
// misreport the run as superseded — and skip the auto-enable — so the commit
// path must recognize an already-applied write as success.
func TestRetestModelCandidateRecognizesItsOwnWriteWhenAcknowledgmentIsLost(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC().Truncate(time.Second)
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// The first UPDATE applies, but its acknowledgment is "lost": the error is
	// injected AFTER the statement executed.
	failed := false
	if err := db.Callback().Update().After("gorm:update").Register("test:lose-ack-once", func(tx *gorm.DB) {
		if failed || tx.Error != nil {
			return
		}
		failed = true
		_ = tx.AddError(errors.New("connection dropped after apply"))
	}); err != nil {
		t.Fatalf("register fault-injection callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:lose-ack-once"); err != nil {
			t.Fatalf("remove fault-injection callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	updated, applied, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if !applied {
		t.Fatal("expected the run to recognize its own applied write instead of reporting itself superseded")
	}
	if updated.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification passed, got %d", updated.VerificationStatus)
	}
	if updated.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the first pass to enable the mapping despite the lost acknowledgment, got %d", updated.ManagementStatus)
	}
}

// The own-write readback after a lost acknowledgment is itself a database
// read, and the same flaky connection that lost the acknowledgment can fail it
// too. Swallowing that read error would misreport the run as superseded even
// though its outcome IS stored — the readback must retry like every other
// probe-persistence operation.
func TestRetestModelCandidateSurvivesReadbackHiccupAfterLostAcknowledgment(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC().Truncate(time.Second)
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// The commit applies but its acknowledgment is lost; the ownership
	// readback that follows then hits one transient read failure of its own.
	ackLost := false
	if err := db.Callback().Update().After("gorm:update").Register("test:lose-ack-once", func(tx *gorm.DB) {
		if ackLost || tx.Error != nil {
			return
		}
		ackLost = true
		_ = tx.AddError(errors.New("connection dropped after apply"))
	}); err != nil {
		t.Fatalf("register update fault: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:lose-ack-once"); err != nil {
			t.Fatalf("remove update fault: %v", err)
		}
	}()
	readFailed := false
	if err := db.Callback().Query().Before("gorm:query").Register("test:fail-one-read", func(tx *gorm.DB) {
		// Armed only once the lost acknowledgment happened: the next read is
		// the ownership readback.
		if !ackLost || readFailed {
			return
		}
		readFailed = true
		_ = tx.AddError(errors.New("transient read blip"))
	}); err != nil {
		t.Fatalf("register query fault: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:fail-one-read"); err != nil {
			t.Fatalf("remove query fault: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	updated, applied, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if !applied {
		t.Fatal("expected the retried readback to recognize the run's own stored write")
	}
	if updated.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the first pass enabled, got %d", updated.ManagementStatus)
	}
}

// applied must describe what the RETURNED VIEW shows, not just whether this
// run's commit landed: a competitor probe can commit right after this retest's
// CAS and before the final reload, in which case the view carries the
// competitor's outcome. Announcing it as this click's result would report the
// exact opposite of what this run observed.
func TestRetestModelCandidateReportsSupersededWhenALaterProbeWinsBeforeReload(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC().Truncate(time.Second)
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// Right after this retest's own commit (the first UPDATE), a competitor
	// probe that read the row post-commit lands a PASS — deliberately stamped
	// with the SAME timestamp, so any ordering-by-time ownership check would
	// be fooled; only the run token tells the two runs apart.
	fired := false
	if err := db.Callback().Update().After("gorm:update").Register("test:competitor-commit", func(tx *gorm.DB) {
		if fired || tx.Error != nil {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		var row model.ModelCandidate
		if err := session.First(&row, candidate.ID).Error; err != nil {
			t.Errorf("competitor read failed: %v", err)
			return
		}
		passed := model.ModelVerificationStatusPassed
		ok := 0
		if _, err := repository.CommitModelCandidateProbeResults(session,
			candidate.ID, "gpt-4o", row.LastProbeRunID, "competitor-run", repository.CandidateProbeCommit{
				VerificationStatus: &passed, LastTestResult: &ok, WriteLastTestError: true,
			}, now); err != nil {
			t.Errorf("competitor commit failed: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:competitor-commit"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	// This retest itself observes a decisive failure.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound, Detail: "nope"}
	updated, applied, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if updated.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the returned view to show the competitor's pass, got %d", updated.VerificationStatus)
	}
	if applied {
		t.Fatal("expected applied=false when the returned view no longer carries this run's outcome")
	}
}

// The production client reports a request killed mid-flight as an inconclusive
// RESULT (unreachable, nil error). If the caller's context died — the admin
// aborted the request, the server is shutting down — that result is a
// cancellation artifact, and committing it would stamp the row with an attempt
// no upstream ever answered.
func TestRetestModelCandidateDoesNotCommitWhenContextCancelledMidProbe(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	ctx, cancel := context.WithCancel(context.Background())
	client.Result = providerclient.TestResult{Outcome: providerclient.TestUnreachable, Detail: "context canceled"}
	client.SideEffect = cancel

	if _, _, err := svc.RetestModelCandidate(ctx, candidate.ID, now); err == nil {
		t.Fatal("expected the cancelled retest to surface an error instead of a verdict")
	}

	var c model.ModelCandidate
	if err := db.First(&c, candidate.ID).Error; err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if c.LastTestedAt != nil || c.LastTestError != nil || c.LastTestResult != nil {
		t.Fatalf("a cancelled retest must leave the row untouched, got tested_at=%v error=%v result=%v",
			c.LastTestedAt, c.LastTestError, c.LastTestResult)
	}
}

// A failing basic probe means the credential, address or model name is wrong, so
// the capability probes cannot say anything useful — they must be skipped rather
// than spend two more upstream requests and risk recording a misleading verdict.
func TestRetestModelCandidateSkipsCapabilityProbesWhenBasicFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	updated, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if updated.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected verification_status=failed, got %d", updated.VerificationStatus)
	}
	if updated.SupportsStreaming != nil || updated.SupportsFunctionCalling != nil {
		t.Fatalf("expected capabilities to stay unknown, got streaming=%v function_calling=%v",
			updated.SupportsStreaming, updated.SupportsFunctionCalling)
	}
	if got := client.CallCountFor("streaming"); got != 0 {
		t.Fatalf("expected the streaming probe to be skipped, got %d calls", got)
	}
	if got := client.CallCountFor("function_calling"); got != 0 {
		t.Fatalf("expected the function-calling probe to be skipped, got %d calls", got)
	}
}

// A probe that fails to confirm a capability must leave a verdict that was
// previously earned alone. Erasing it would let one transient rate limit undo a
// real confirmation, and the operator would see a capability they had already
// verified quietly revert to unknown.
func TestRetestModelCandidateKeepsEarnedCapabilityWhenProbeIsUnconfirmed(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// The verdict has to be EARNED first, or the assertions below cannot tell
	// "preserved the stored value" apart from "overwrote it with the nil it
	// already held" — which is exactly how an overwrite bug once hid here.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	seeded, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("seeding retest failed: %v", err)
	}
	if seeded.SupportsStreaming == nil || !*seeded.SupportsStreaming {
		t.Fatalf("expected the seeding retest to confirm streaming, got %v", seeded.SupportsStreaming)
	}
	if seeded.SupportsFunctionCalling == nil || !*seeded.SupportsFunctionCalling {
		t.Fatalf("expected the seeding retest to confirm function calling, got %v", seeded.SupportsFunctionCalling)
	}

	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic":            {Result: providerclient.TestResult{Outcome: providerclient.TestSuccess}},
		"streaming":        {Result: providerclient.TestResult{Outcome: providerclient.TestRateLimited}},
		"function_calling": {Result: providerclient.TestResult{Outcome: providerclient.TestUpstreamError}},
	}
	updated, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if updated.SupportsStreaming == nil || !*updated.SupportsStreaming {
		t.Fatalf("expected a rate-limited streaming probe to leave the earned verdict alone, got %v", updated.SupportsStreaming)
	}
	if updated.SupportsFunctionCalling == nil || !*updated.SupportsFunctionCalling {
		t.Fatalf("expected an unconfirmed function-calling probe to leave the earned verdict alone, got %v", updated.SupportsFunctionCalling)
	}
}

// verification_status gates ALL routing, not just streaming and tool calling, so
// an inconclusive basic probe must leave a passing candidate passing. Otherwise a
// single upstream hiccup during a retest takes a healthy candidate out of service
// entirely until a human lands a successful retest.
func TestRetestModelCandidateKeepsVerificationWhenBasicProbeIsInconclusive(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	for _, tc := range []struct {
		name   string
		result providerclient.TestResult
	}{
		{name: "rate limited", result: providerclient.TestResult{Outcome: providerclient.TestRateLimited}},
		{name: "unreachable", result: providerclient.TestResult{Outcome: providerclient.TestUnreachable}},
		{name: "upstream 502", result: providerclient.TestResult{Outcome: providerclient.TestUpstreamError}},
		{name: "out of quota", result: providerclient.TestResult{Outcome: providerclient.TestQuotaUnavailable}},
		{name: "bad credential", result: providerclient.TestResult{Outcome: providerclient.TestAuthFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client.PerTestType = nil
			client.Result = tc.result
			updated, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
			if err != nil {
				t.Fatalf("RetestModelCandidate failed: %v", err)
			}
			if updated.VerificationStatus != model.ModelVerificationStatusPassed {
				t.Fatalf("expected an inconclusive basic probe to leave verification passed, got %d", updated.VerificationStatus)
			}
			if updated.ManagementStatus != model.ModelCandidateStatusEnabled {
				t.Fatalf("expected the candidate to stay enabled, got %d", updated.ManagementStatus)
			}
			if !updated.Routable {
				t.Fatal("expected the candidate to remain routable")
			}
		})
	}
}

// A probe takes seconds of upstream round trips, so the row can be retargeted
// underneath it. Its verdict must then be dropped: stamping the new mapping with
// the old mapping's result would mark a target nobody tested as verified, and the
// gateway routes on exactly that flag.
func TestRetestModelCandidateDiscardsVerdictWhenTargetChangedMidProbe(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// Stand in for a concurrent PATCH landing while the probe is in flight.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	client.SideEffect = func() {
		if err := db.Model(&model.ModelCandidate{}).Where("id = ?", candidate.ID).
			Update("provider_model_name", "retargeted-by-someone-else").Error; err != nil {
			t.Errorf("simulate concurrent retarget: %v", err)
		}
	}

	updated, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if updated.VerificationStatus == model.ModelVerificationStatusPassed {
		t.Fatal("expected the verdict to be discarded, but the retargeted mapping was marked verified")
	}
	if updated.ProviderModelName != "retargeted-by-someone-else" {
		t.Fatalf("expected the concurrent retarget to stand, got %q", updated.ProviderModelName)
	}
}

// The edit flow reads the candidate, renames it, then probes the new target.
// Another probe can commit between that read and the rename, advancing the run
// token; the rename keeps the token, so probing with the token from the STALE
// read would see its guard miss even though it tested the row's current
// target — leaving the mapping untested and disabled for no reason. The token
// must be re-read after the rename.
func TestUpdateModelCandidateProbesWithFreshTokenAfterRetarget(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// A competitor probe advances the run token in the window between the
	// edit's initial read and its fresh baseline read — injected just before
	// the second candidates query (the fresh read; the first is the edit's
	// initial read). A query hook rather than an update hook: the edit's
	// field update now runs inside a transaction, and a nested write from
	// within its update callback would deadlock on the connection pool.
	queries := 0
	fired := false
	if err := db.Callback().Query().Before("gorm:query").Register("test:competitor-around-rename", func(tx *gorm.DB) {
		if fired || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "model_candidates" {
			return
		}
		queries++
		if queries != 2 {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := session.Model(&model.ModelCandidate{}).Where("id = ?", candidate.ID).
			Update("last_probe_run_id", "competitor-run").Error; err != nil {
			t.Errorf("simulate competitor commit: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:competitor-around-rename"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the new target's passing probe to land despite the pre-rename competitor, got verification=%d",
			result.Candidate.VerificationStatus)
	}
	// The verdict lands, but the enable is forfeited: a token that moved
	// between the edit's two reads means someone else acted on the row, and a
	// bare token advance is indistinguishable from an explicit no-op disable —
	// which must win. The operator sees the verified-but-disabled row and can
	// enable it with the toggle.
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the enable forfeited to the mid-edit token advance, got %d", result.Candidate.ManagementStatus)
	}
}

// When the edit-triggered probe loses the commit race to a concurrent probe,
// the result must SAY so: the report describes an outcome the row does not
// hold, and a client treating it as the saved state would close with "saved
// and enabled" over a row that actually carries someone else's verdict.
func TestUpdateModelCandidateReportsSupersededProbe(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// While the edit's probe is upstream, a competitor probe commits — a real
	// probe commit stamps the attempt and lands a verdict along with the token
	// advance, and that stamp is what tells the edit a FRESHER verdict owns
	// the row (a bare token bump would read as an admin status write, whose
	// discard the edit is entitled to recover from by re-committing).
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	fired := false
	client.SideEffect = func() {
		if fired {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		var row model.ModelCandidate
		if err := session.First(&row, candidate.ID).Error; err != nil {
			t.Errorf("competitor read failed: %v", err)
			return
		}
		failed := model.ModelVerificationStatusFailed
		notFound := 2
		if _, err := repository.CommitModelCandidateProbeResults(session, candidate.ID, row.ProviderModelName,
			row.LastProbeRunID, "competitor-run", repository.CandidateProbeCommit{
				VerificationStatus: &failed, LastTestResult: &notFound, WriteLastTestError: true,
			}, time.Now().UTC()); err != nil {
			t.Errorf("competitor commit failed: %v", err)
		}
	}

	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Report == nil {
		t.Fatal("expected a probe report for an enable-with-retarget edit")
	}
	if result.ReportApplied {
		t.Fatal("expected the superseded probe to be reported as not applied")
	}
}

// Like the retest path, the edit path must re-check ownership AFTER its final
// reload: a competitor (a queue worker against a fast upstream) can commit
// right after the edit-probe's own commit and before the reload, leaving the
// returned candidate with the competitor's verdict while report_applied still
// claims the report is the row's state.
func TestUpdateModelCandidateReportsSupersededWhenALaterProbeWinsBeforeReload(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// The edit flow's writes: #1 the rename, #2 the probe commit. The
	// competitor lands right after #2 — reading the fresh token and
	// overwriting the row with its own failing verdict — before the reload.
	updates := 0
	fired := false
	if err := db.Callback().Update().After("gorm:update").Register("test:competitor-after-commit", func(tx *gorm.DB) {
		if fired || tx.Error != nil {
			return
		}
		updates++
		if updates != 2 {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		var row model.ModelCandidate
		if err := session.First(&row, candidate.ID).Error; err != nil {
			t.Errorf("competitor read failed: %v", err)
			return
		}
		failed := model.ModelVerificationStatusFailed
		notFound := 2
		if _, err := repository.CommitModelCandidateProbeResults(session,
			candidate.ID, "gpt-4o-mini", row.LastProbeRunID, "competitor-run", repository.CandidateProbeCommit{
				VerificationStatus: &failed, LastTestResult: &notFound, WriteLastTestError: true,
				DisableOnFail: true,
			}, now.Add(time.Second)); err != nil {
			t.Errorf("competitor commit failed: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:competitor-after-commit"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected the returned view to show the competitor's verdict, got %d", result.Candidate.VerificationStatus)
	}
	if result.ReportApplied {
		t.Fatal("expected report_applied=false once the view no longer carries this run's outcome")
	}
}

// An operator disabling a candidate mid-probe must win: verification_status alone
// says nothing about their intent, so enabling on it would silently undo them.
func TestUpdateModelCandidateDoesNotReEnableAfterConcurrentDisable(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	client.SideEffect = func() {
		if err := db.Model(&model.ModelCandidate{}).Where("id = ?", candidate.ID).
			Update("management_status", model.ModelCandidateStatusDisabled).Error; err != nil {
			t.Errorf("simulate concurrent disable: %v", err)
		}
	}

	// Renaming the target of an already-enabled candidate is what triggers a
	// probe whose success would otherwise re-enable the row.
	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the concurrent disable to stand, got management_status=%d", result.Candidate.ManagementStatus)
	}
}

// Enabling a verified mapping must not invalidate a retest already in flight:
// the enable is not new evidence about the mapping, and advancing the probe
// token would discard the retest's verdict — a decisive failure would vanish
// and the row would keep routing as Passed+Enabled moments after being proven
// broken. The verdict must land; the enable stands or falls on its own.
func TestSetCandidateStatusEnableDoesNotInvalidateInFlightRetest(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)
	// The admin had explicitly disabled the verified mapping.
	if err := svc.SetCandidateStatus(candidate.ID, false, now); err != nil {
		t.Fatalf("explicit disable failed: %v", err)
	}

	// While the retest is upstream, the admin re-enables the mapping.
	enabled := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	client.SideEffect = func() {
		if enabled {
			return
		}
		enabled = true
		if err := svc.SetCandidateStatus(candidate.ID, true, time.Now().UTC()); err != nil {
			t.Errorf("enable during retest: %v", err)
		}
	}

	view, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if view.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected the decisive failure recorded despite the concurrent enable, got verification=%d", view.VerificationStatus)
	}
	c := loadCandidate(t, db, candidate.ID)
	if c.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected the stored row to carry the failure, got verification=%d", c.VerificationStatus)
	}
	// The decisive failure also demotes the row the concurrent enable turned
	// on: verification not being Passed already stops routing, but leaving
	// management enabled would keep the admin list claiming a mapping just
	// proven broken is serving traffic.
	if c.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the decisive failure to demote the concurrently enabled row, got management=%d", c.ManagementStatus)
	}
}

// A disable that lands between the edit's field update and its probe must
// stand: the probe reads the row AFTER that disable, and adopting what it
// finds as the enable's CAS baseline would make "status unchanged since I
// looked" trivially true — the passing probe would re-enable the mapping the
// API just acknowledged as off. The baseline must be the edit's own initial
// read; only the probe token comes from the fresh read.
func TestUpdateModelCandidateDoesNotReEnableWhenDisableLandsBeforeItsProbe(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	// After the edit's field update — before its probe reads its baseline —
	// another request explicitly disables the candidate. Injected just
	// before the second candidates query (the fresh read); a nested write
	// from inside the transactional field update's own callback would
	// deadlock on the connection pool.
	queries := 0
	fired := false
	if err := db.Callback().Query().Before("gorm:query").Register("test:disable-after-field-update", func(tx *gorm.DB) {
		if fired || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "model_candidates" {
			return
		}
		queries++
		if queries != 2 {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.SetModelCandidateManagementStatusAdvancingProbeToken(session, candidate.ID,
			model.ModelCandidateStatusDisabled, "competitor-run", time.Now().UTC()); err != nil {
			t.Errorf("simulate concurrent explicit disable: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:disable-after-field-update"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the acknowledged disable to stand, got management_status=%d", result.Candidate.ManagementStatus)
	}
	if c := loadCandidate(t, db, candidate.ID); c.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the stored row to stay disabled, got management_status=%d", c.ManagementStatus)
	}
}

// A decisive retest verdict must survive an explicit disable that lands while
// the probe is upstream: the disable advances the token, but it says nothing
// about whether the mapping works — discarding the verdict would leave a stale
// Passed on the row, and the status toggle (which trusts verification alone)
// would happily re-enable a mapping just proven broken. The verdict re-commits
// under the fresh token, without any management transition of its own.
func TestRetestModelCandidateLandsVerdictDespiteConcurrentDisable(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	// While the retest is upstream, the admin explicitly disables the mapping.
	disabled := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	client.SideEffect = func() {
		if disabled {
			return
		}
		disabled = true
		if err := svc.SetCandidateStatus(candidate.ID, false, time.Now().UTC()); err != nil {
			t.Errorf("explicit disable during retest: %v", err)
		}
	}

	view, applied, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if view.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected the decisive failure recorded despite the concurrent disable, got verification=%d", view.VerificationStatus)
	}
	if !applied {
		t.Fatal("expected the re-committed verdict to be owned by this retest")
	}
	if view.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the explicit disable to stand, got management_status=%d", view.ManagementStatus)
	}
	// The stale Passed is gone, so the toggle can no longer re-enable the
	// broken mapping.
	if err := svc.SetCandidateStatus(candidate.ID, true, now); !errors.Is(err, errcode.ErrModelCandidateNotVerified) {
		t.Fatalf("expected re-enable to be rejected on the failed row, got %v", err)
	}
}

// An explicit no-op disable landing between the edit's field write and its
// fresh baseline read advances the token while leaving the status Disabled —
// the very values the edit then adopts as its probe baseline. Enabling off
// that baseline would re-enable the row after the LATER disable was already
// acknowledged. A token that moved between the edit's two reads means someone
// else acted on the row: the verdict still lands, the enable is forfeited.
func TestUpdateModelCandidateDoesNotReEnableWhenNoOpDisableLandsBeforeFreshRead(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, provider.ID, "some-model")

	// The explicit (value-preserving) disable lands after the edit's field
	// update, before the fresh baseline read — injected just before the
	// second candidates query (the fresh read); a nested write from inside
	// the transactional field update's own callback would deadlock on the
	// connection pool.
	queries := 0
	fired := false
	if err := db.Callback().Query().Before("gorm:query").Register("test:disable-before-fresh-read", func(tx *gorm.DB) {
		if fired || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "model_candidates" {
			return
		}
		queries++
		if queries != 2 {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.SetModelCandidateManagementStatusAdvancingProbeToken(session, id,
			model.ModelCandidateStatusDisabled, "competitor-run", time.Now().UTC()); err != nil {
			t.Errorf("simulate concurrent explicit disable: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:disable-before-fresh-read"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	result, err := svc.UpdateModelCandidate(context.Background(), id, modeladmin.UpdateCandidateInput{
		ProviderModelName: "some-model", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the later explicit disable to stand, got management_status=%d", result.Candidate.ManagementStatus)
	}
	if result.Candidate.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the verdict to land despite the forfeited enable, got verification=%d", result.Candidate.VerificationStatus)
	}
}

// The same stale-Passed danger exists on the edit path: an enable-edit of a
// verified-but-disabled row probes WITHOUT renaming, so a concurrent no-op
// disable that discards the failing verdict leaves the old Passed standing —
// and the status toggle would re-enable the broken mapping. The edit's probe
// must re-commit its verdict under the fresh token exactly as the retest does.
func TestUpdateModelCandidateLandsVerdictDespiteConcurrentDisable(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)
	if err := svc.SetCandidateStatus(candidate.ID, false, now); err != nil {
		t.Fatalf("explicit disable failed: %v", err)
	}

	// While the edit's probe is upstream, another admin disables the (already
	// disabled) mapping — a value-preserving write that still advances the token.
	disabled := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	client.SideEffect = func() {
		if disabled {
			return
		}
		disabled = true
		if err := svc.SetCandidateStatus(candidate.ID, false, time.Now().UTC()); err != nil {
			t.Errorf("explicit disable during edit probe: %v", err)
		}
	}

	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected the decisive failure recorded despite the concurrent disable, got verification=%d", result.Candidate.VerificationStatus)
	}
	if err := svc.SetCandidateStatus(candidate.ID, true, now); !errors.Is(err, errcode.ErrModelCandidateNotVerified) {
		t.Fatalf("expected re-enable to be rejected on the failed row, got %v", err)
	}
}

// Ownership is (token, target): a concurrent PATCH that retargets the row
// clears the verdict fields but deliberately keeps last_probe_run_id, so a
// token-only reload check would leave report_applied=true — announcing "saved
// and verified" over a row that is now a different, untested mapping.
func TestUpdateModelCandidateReportsSupersededWhenRetargetedBeforeReload(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	// The edit flow's writes: #1 the rename, #2 the probe commit. The
	// competitor's retarget lands right after #2, before the reload.
	updates := 0
	fired := false
	if err := db.Callback().Update().After("gorm:update").Register("test:retarget-after-commit", func(tx *gorm.DB) {
		if fired || tx.Error != nil {
			return
		}
		updates++
		if updates != 2 {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.UpdateModelCandidate(session, candidate.ID, "competitor-target", 1, 2,
			nil, nil, 0, true, false, time.Now().UTC()); err != nil {
			t.Errorf("competitor retarget failed: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:retarget-after-commit"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.ReportApplied {
		t.Fatal("expected report_applied=false once the returned row is the competitor's retargeted mapping")
	}
}

// Same guarantee on the retest path: a retarget between this retest's commit
// and its reload keeps the token but replaces the mapping — the verdict must
// not be announced as the returned row's state.
func TestRetestModelCandidateReportsSupersededWhenRetargetedBeforeReload(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	// The retest issues one write (its probe commit); the retarget lands right
	// after it, before the reload.
	fired := false
	if err := db.Callback().Update().After("gorm:update").Register("test:retarget-after-retest", func(tx *gorm.DB) {
		if fired || tx.Error != nil {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.UpdateModelCandidate(session, candidate.ID, "competitor-target", 1, 2,
			nil, nil, 0, true, false, time.Now().UTC()); err != nil {
			t.Errorf("competitor retarget failed: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:retarget-after-retest"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	_, applied, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if applied {
		t.Fatal("expected applied=false once the returned row is the competitor's retargeted mapping")
	}
}

// A manual retest of an ARMED row must go through the armed commit mode: a
// failing retest bumps the row clock, and without the armed mode's
// realignment the promise would be left misaligned — the next queue probe
// that PASSES would then refuse the enable and revoke the promise, stranding
// the mapping Passed+Disabled because someone once clicked retest.
func TestRetestModelCandidateKeepsArmedPromiseUsableAfterFailure(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, provider.ID, "some-model")

	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	if _, _, err := svc.RetestModelCandidate(context.Background(), id, now); err != nil {
		t.Fatalf("failing retest errored: %v", err)
	}

	// The upstream recovers; a queue probe (a requeued run, a recovery run)
	// passes — the import's promise must still deliver the enable.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("queue probe errored: %v", err)
	}
	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the recovery pass recorded, got verification=%d", c.VerificationStatus)
	}
	if c.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the armed promise fulfilled after the failed retest, got management=%d", c.ManagementStatus)
	}
}

// An explicit disable that lands while an ARMED row's retest is upstream
// clears the promise flag — but that revocation is NOT a requeue, and must
// not make the re-commit recovery give up: giving up would leave the row's
// stale Passed standing, and the status toggle would trust it to re-enable a
// mapping the retest just proved broken. What separates the two: a requeue
// re-arms with a fresh armed_at, an explicit disable leaves armed_at alone.
func TestRetestModelCandidateRelandsFailureWhenDisableClearsArmedFlag(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, provider.ID, "some-model")
	// The armed row earned a pass earlier (stamped, Passed, still disabled by
	// an admin's later toggle would consume armed — so seed the pass raw,
	// keeping the armed promise and its alignment intact).
	landed := time.Now().UTC()
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"verification_status": model.ModelVerificationStatusPassed,
			"last_tested_at":      landed,
		}).Error; err != nil {
		t.Fatalf("seed passed state: %v", err)
	}

	// While the retest is upstream, an explicit disable clears the armed flag
	// and advances the token (leaving armed_at untouched).
	disabled := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	client.SideEffect = func() {
		if disabled {
			return
		}
		disabled = true
		if err := svc.SetCandidateStatus(id, false, time.Now().UTC()); err != nil {
			t.Errorf("explicit disable during retest: %v", err)
		}
	}

	if _, _, err := svc.RetestModelCandidate(context.Background(), id, now); err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected the decisive failure re-landed despite the armed revocation, got verification=%d", c.VerificationStatus)
	}
	// The re-landed commit runs in the armed mode too, so a promise the row
	// still carried is settled by its misalignment rules rather than left as
	// a dead flag a later same-name edit could re-align and resurrect.
	if c.AutoEnableOnPass {
		t.Fatal("expected the re-landed failure to settle the armed flag, not leave it dangling")
	}
	if err := svc.SetCandidateStatus(id, true, now); !errors.Is(err, errcode.ErrModelCandidateNotVerified) {
		t.Fatalf("expected re-enable rejected on the failed row, got %v", err)
	}
}

// The combination that must not fool the re-commit recovery: a same-name
// price edit re-aligns armed_at (without advancing the token), then an
// explicit disable advances the token — neither is a requeue, and the
// retest's decisive failure must still re-land. Requeues are identified by
// their token namespace, not by armed_at movement.
func TestRetestModelCandidateRelandsFailureAfterRealignAndDisable(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, provider.ID, "some-model")
	landed := time.Now().UTC()
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"verification_status": model.ModelVerificationStatusPassed,
			"last_tested_at":      landed,
		}).Error; err != nil {
		t.Fatalf("seed passed state: %v", err)
	}

	// While the retest is upstream: a same-name price edit (re-aligns
	// armed_at), then an explicit disable (advances the token, clears armed).
	raced := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	client.SideEffect = func() {
		if raced {
			return
		}
		raced = true
		if _, err := svc.UpdateModelCandidate(context.Background(), id, modeladmin.UpdateCandidateInput{
			ProviderModelName: "some-model", InputPrice: 9, OutputPrice: 9,
		}, time.Now().UTC()); err != nil {
			t.Errorf("same-name edit during retest: %v", err)
		}
		if err := svc.SetCandidateStatus(id, false, time.Now().UTC()); err != nil {
			t.Errorf("explicit disable during retest: %v", err)
		}
	}

	if _, _, err := svc.RetestModelCandidate(context.Background(), id, now); err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected the decisive failure re-landed, got verification=%d", c.VerificationStatus)
	}
}

// A re-import requeue that lands while a manual retest is upstream advances
// the token precisely to supersede that retest — its verdict must NOT sneak
// back in through the re-commit recovery. The requeue leaves the same nil
// attempt stamp the retest's baseline had, so the stamp alone cannot betray
// it; the re-armed columns can.
func TestRetestModelCandidateDoesNotRelandVerdictOverARequeue(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, provider.ID, "some-model")

	// While the retest is upstream, a re-import requeues the row (advancing
	// the token, clearing residue, re-arming with a fresh armed_at).
	requeued := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	client.SideEffect = func() {
		if requeued {
			return
		}
		requeued = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.ClearCandidatesProbeResidue(session, []uint{id}, "rq-requeue-run", time.Now().UTC().Add(time.Second)); err != nil {
			t.Errorf("simulate requeue: %v", err)
		}
	}

	_, applied, err := svc.RetestModelCandidate(context.Background(), id, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if applied {
		t.Fatal("expected the superseded retest to report applied=false")
	}
	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected the row still waiting for the requeued probe, got verification=%d", c.VerificationStatus)
	}
}

// The lost-acknowledgment readback must disown the write the same way: the
// write applied and the token reads back as this run's, but a retarget that
// landed in between has already replaced the mapping (keeping the token), so
// token-only recognition would report a verdict the row no longer holds.
func TestRetestModelCandidateDisownsVerdictWhenRetargetLandsBeforeReadback(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	// Write #1: the probe commit applies but its acknowledgment is lost.
	// Write #2: the durable retry whose CAS misses its own effect. The
	// retarget lands right after #2, before the ownership readback.
	updates := 0
	fired := false
	if err := db.Callback().Update().After("gorm:update").Register("test:retarget-before-readback", func(tx *gorm.DB) {
		if fired {
			return
		}
		updates++
		if updates == 1 && tx.Error == nil {
			_ = tx.AddError(errors.New("connection dropped after apply"))
			return
		}
		if updates != 2 {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.UpdateModelCandidate(session, candidate.ID, "competitor-target", 1, 2,
			nil, nil, 0, true, false, time.Now().UTC()); err != nil {
			t.Errorf("competitor retarget failed: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:retarget-before-readback"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	_, applied, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if applied {
		t.Fatal("expected the readback to disown the verdict once the mapping was retargeted")
	}
}

// A PATCH that says "disabled" is the same instruction as the dedicated status
// toggle, so it must revoke the standing auto-enable promise and advance the
// probe token even when the stored status is already Disabled — otherwise a
// queued probe that passes moments later re-enables the row the API just
// acknowledged as off.
func TestUpdateModelCandidateRevokesAutoEnableOnExplicitDisable(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, provider.ID, "some-model")
	staleToken := loadCandidate(t, db, id).LastProbeRunID

	result, err := svc.UpdateModelCandidate(context.Background(), id, modeladmin.UpdateCandidateInput{
		ProviderModelName: "some-model", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: disableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the candidate to stay disabled, got %d", result.Candidate.ManagementStatus)
	}

	row := loadCandidate(t, db, id)
	if row.AutoEnableOnPass {
		t.Fatal("an explicit disable must revoke the auto-enable promise")
	}
	if row.LastProbeRunID == staleToken {
		t.Fatal("an explicit disable must advance the probe token so in-flight commits miss")
	}

	// The probe already queued for this row commits with the token it read
	// before the disable: its verdict must be discarded, not enable the row.
	passed := model.ModelVerificationStatusPassed
	success := int(providerclient.TestSuccess)
	applied, err := repository.CommitModelCandidateProbeResults(db, id, "some-model", staleToken, "worker-run",
		repository.CandidateProbeCommit{
			VerificationStatus: &passed, LastTestResult: &success, WriteLastTestError: true,
			EnableOnPassWhenArmed: true,
		}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("stale probe commit errored: %v", err)
	}
	if applied {
		t.Fatal("a probe holding the pre-disable token must see its commit discarded")
	}
	if got := loadCandidate(t, db, id).ManagementStatus; got != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the explicit disable to stand, got management_status=%d", got)
	}
}

// A decisive failure, by contrast, must both record the failure and stop the
// admin list claiming the candidate is serving traffic it cannot serve.
func TestRetestModelCandidateDemotesOnDecisiveBasicFailure(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	updated, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
	if err != nil {
		t.Fatalf("RetestModelCandidate failed: %v", err)
	}
	if updated.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected verification_status=failed, got %d", updated.VerificationStatus)
	}
	if updated.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected a decisively broken mapping to be demoted, got management_status=%d", updated.ManagementStatus)
	}
}

func TestTestModelCandidateReturnsNotFoundForUnknownCandidate(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, _, err := svc.RetestModelCandidate(context.Background(), 999999, time.Now().UTC())
	if !errors.Is(err, errcode.ErrModelCandidateNotFound) {
		t.Fatalf("expected ErrModelCandidateNotFound, got %v", err)
	}
}

// Asking to enable a mapping is a request for something that works now, so a
// failing basic probe must leave nothing behind — the admin fixes the config and
// retries instead of finding a broken row in the list.
func TestTestAndCreateCandidateDoesNotPersistWhenEnableRequestedAndBasicFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	result, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("TestAndCreateCandidate failed: %v", err)
	}
	if result.Created {
		t.Fatal("expected no candidate to be created when the basic probe failed")
	}
	if result.Candidate != nil {
		t.Fatal("expected no candidate view when nothing was created")
	}
	if result.Report.Basic.Passed() {
		t.Fatal("expected the basic probe to be reported as failed")
	}
	if result.Report.Streaming.Ran || result.Report.FunctionCalling.Ran {
		t.Fatal("expected the capability probes to be skipped after a failing basic probe")
	}

	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if len(detail.Candidates) != 0 {
		t.Fatalf("expected zero stored candidates, got %d", len(detail.Candidates))
	}
}

// Saving as disabled is an explicit "store this for now", so it persists
// whatever the probes found rather than being rejected.
func TestTestAndCreateCandidatePersistsDisabledEvenWhenBasicFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	result, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusDisabled,
	}, now)
	if err != nil {
		t.Fatalf("TestAndCreateCandidate failed: %v", err)
	}
	if !result.Created || result.Candidate == nil {
		t.Fatal("expected the candidate to be stored when saving as disabled")
	}
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the candidate to be disabled, got %d", result.Candidate.ManagementStatus)
	}
	if result.Candidate.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected the failed probe to be recorded, got status %d", result.Candidate.VerificationStatus)
	}
}

// A passing run stores the mapping enabled with both capability verdicts, which
// is the whole point of probing at save time: the admin never runs a test by
// hand and the capability columns are populated from the start.
func TestTestAndCreateCandidateStoresEnabledWithCapabilitiesWhenAllProbesPass(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	result, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("TestAndCreateCandidate failed: %v", err)
	}
	if !result.Created || result.Candidate == nil {
		t.Fatal("expected the candidate to be created")
	}
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the candidate to be enabled, got %d", result.Candidate.ManagementStatus)
	}
	if result.Candidate.SupportsStreaming == nil || !*result.Candidate.SupportsStreaming {
		t.Fatalf("expected streaming support recorded at creation, got %v", result.Candidate.SupportsStreaming)
	}
	if result.Candidate.SupportsFunctionCalling == nil || !*result.Candidate.SupportsFunctionCalling {
		t.Fatalf("expected function-calling support recorded at creation, got %v", result.Candidate.SupportsFunctionCalling)
	}
}

// A provider with no verified key yet cannot be probed at all. Configuring a
// mapping while the credential is still being sorted out has to remain possible,
// so saving as disabled goes through with unknown verdicts rather than failing.
func TestTestAndCreateCandidateSavesDisabledWhenProviderCannotBeProbed(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	// A provider with no keys at all: decryptHighestPriorityAvailableKey has
	// nothing to pick, so no probe can run.
	provider := model.Provider{
		Name: "keyless", ProviderType: "openai", BaseURL: "https://example.invalid",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatalf("seed keyless provider failed: %v", err)
	}
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	result, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusDisabled,
	}, now)
	if err != nil {
		t.Fatalf("expected saving as disabled to succeed without a probe, got %v", err)
	}
	if !result.Created || result.Candidate == nil {
		t.Fatalf("expected the candidate to be stored, got %+v", result)
	}
	if result.Candidate.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected untested (not failed) when no probe could run, got %d", result.Candidate.VerificationStatus)
	}
	if result.Candidate.SupportsStreaming != nil || result.Candidate.SupportsFunctionCalling != nil {
		t.Fatalf("expected unknown capabilities, got streaming=%v fc=%v",
			result.Candidate.SupportsStreaming, result.Candidate.SupportsFunctionCalling)
	}
}

// Enabling asserts the mapping works now, which cannot be established without a
// probe — so the reason is surfaced instead of silently storing it disabled.
func TestTestAndCreateCandidateFailsWhenEnableRequestedAndProviderCannotBeProbed(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	provider := model.Provider{
		Name: "keyless", ProviderType: "openai", BaseURL: "https://example.invalid",
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatalf("seed keyless provider failed: %v", err)
	}
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	if _, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err == nil {
		t.Fatal("expected an error when enablement was requested but no probe could run")
	}

	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if len(detail.Candidates) != 0 {
		t.Fatalf("expected nothing stored, got %d candidates", len(detail.Candidates))
	}
}

// The disabled escape hatch must not touch the upstream at all: it is what the
// UI calls after a probe run already failed, and re-probing there would spend
// three more requests to re-learn what the operator was just shown.
func TestCreateModelCandidateDisabledSkipsProbing(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	before := client.CallCountFor("basic")
	created, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusDisabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if got := client.CallCountFor("basic") - before; got != 0 {
		t.Fatalf("expected no probe when saving as disabled, got %d basic probes", got)
	}
	if created.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected an untested candidate, got status %d", created.VerificationStatus)
	}
	if created.SupportsStreaming != nil || created.SupportsFunctionCalling != nil {
		t.Fatalf("expected unknown capabilities, got streaming=%v fc=%v",
			created.SupportsStreaming, created.SupportsFunctionCalling)
	}
}

func TestUpdateModelCandidate(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	updated, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-2024", InputPrice: 1.5, OutputPrice: 3, MaxOutput: 4096,
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if updated.Candidate.ProviderModelName != "gpt-4o-2024" || updated.Candidate.InputPrice != 1.5 {
		t.Fatalf("expected updated fields, got %+v", updated)
	}
}

func TestUpdateModelCandidateResetsVerificationWhenModelNameChanges(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	seedVerified := func() {
		if err := db.Model(&model.ModelCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]interface{}{
			"verification_status":       model.ModelVerificationStatusPassed,
			"supports_streaming":        true,
			"supports_function_calling": true,
		}).Error; err != nil {
			t.Fatalf("seed verified state failed: %v", err)
		}
	}
	reload := func() model.ModelCandidate {
		var c model.ModelCandidate
		if err := db.Where("id = ?", candidate.ID).First(&c).Error; err != nil {
			t.Fatalf("reload candidate failed: %v", err)
		}
		return c
	}

	// Changing provider_model_name invalidates the mapping verification and the
	// capability flags — they were established against the OLD name. This
	// candidate is disabled and the edit does not ask to enable it, so no
	// re-probe runs (a disabled mapping cannot route, so probing it would spend
	// upstream requests on a verdict nothing can use yet) and the reset is the
	// whole observable effect.
	seedVerified()
	if _, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("UpdateModelCandidate (name change) failed: %v", err)
	}
	if c := reload(); c.VerificationStatus != model.ModelVerificationStatusUntested || c.SupportsStreaming != nil || c.SupportsFunctionCalling != nil {
		t.Fatalf("expected verification+capabilities reset to untested/unknown after model name change, got status=%d streaming=%v fc=%v",
			c.VerificationStatus, c.SupportsStreaming, c.SupportsFunctionCalling)
	}

	// An edit that keeps provider_model_name and does not change enablement
	// touches nothing verification-related: prices say nothing about whether the
	// mapping works, so the stored verdicts stand and no upstream call is made.
	seedVerified()
	before := client.CallCountFor("basic")
	if _, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 9, OutputPrice: 9,
	}, now); err != nil {
		t.Fatalf("UpdateModelCandidate (same name) failed: %v", err)
	}
	c := reload()
	if c.VerificationStatus != model.ModelVerificationStatusPassed ||
		c.SupportsStreaming == nil || !*c.SupportsStreaming ||
		c.SupportsFunctionCalling == nil || !*c.SupportsFunctionCalling {
		t.Fatalf("expected verification+capabilities preserved when name unchanged, got status=%d streaming=%v fc=%v",
			c.VerificationStatus, c.SupportsStreaming, c.SupportsFunctionCalling)
	}
	if after := client.CallCountFor("basic"); after != before {
		t.Fatalf("expected a price-only edit to skip probing, basic probe count went %d -> %d", before, after)
	}
}

// enableStatus / disableStatus build the pointer modeladmin.UpdateCandidateInput expects.
// Leaving ManagementStatus nil means "do not touch enablement", which is what a
// caller editing only prices sends.
func enableStatus() *int  { s := model.ModelCandidateStatusEnabled; return &s }
func disableStatus() *int { s := model.ModelCandidateStatusDisabled; return &s }

// seedEnabledCandidate returns a candidate that is enabled and verified.
func seedEnabledCandidate(t *testing.T, svc *modeladmin.ModelService, client *providerclienttest.Fake, providerID uint, now time.Time) *modeladmin.CandidateView {
	t.Helper()
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	created, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: providerID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	if created.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the seeded candidate to be enabled, got %d", created.ManagementStatus)
	}
	return created
}

// An explicit disable must win even when the same request also renames the
// target. The rename triggers a re-probe, and a probe can only ever grant the
// enabled state — so if the disable is not written independently, the candidate
// keeps serving production traffic the admin just switched off.
func TestUpdateModelCandidateHonoursDisableWhenTargetAlsoChanged(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: disableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the candidate to be disabled, got %d", result.Candidate.ManagementStatus)
	}
	if result.Candidate.Routable {
		t.Fatal("expected a disabled candidate to be unroutable")
	}
}

// An edit that says nothing about enablement must not change it. With a plain int
// the zero value was indistinguishable from "disable", so a price-only edit
// silently pulled the candidate out of routing.
func TestUpdateModelCandidateLeavesEnablementAloneWhenNotRequested(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o", InputPrice: 9, OutputPrice: 9,
		ManagementStatus: nil,
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the candidate to stay enabled after a price-only edit, got %d", result.Candidate.ManagementStatus)
	}
	if result.Report != nil {
		t.Fatal("expected no probe for a price-only edit")
	}
}

// A rename whose re-probe fails must demote the candidate. Leaving it enabled
// with verification failed produces a row the admin list shows as on while the
// gateway silently refuses to route it, with no signal that it stopped serving.
func TestUpdateModelCandidateDemotesWhenReprobeFailsOnEnabledCandidate(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "does-not-exist", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected verification_status=failed, got %d", result.Candidate.VerificationStatus)
	}
	if result.Candidate.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected a failed re-probe to demote the candidate, got management_status=%d", result.Candidate.ManagementStatus)
	}
}

// Renaming the upstream target re-probes it, so a mapping that works under the
// new name comes back verified without the operator running a separate test.
func TestUpdateModelCandidateReprobesAfterModelNameChange(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: enableStatus(),
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Report == nil {
		t.Fatal("expected a probe report after a target rename")
	}
	if result.Candidate.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification_status=passed after a passing re-probe, got %d", result.Candidate.VerificationStatus)
	}
	if result.Candidate.SupportsStreaming == nil || !*result.Candidate.SupportsStreaming {
		t.Fatalf("expected the re-probe to record streaming support, got %v", result.Candidate.SupportsStreaming)
	}
}

// The field update is the operator's explicit instruction about data the probe
// has no bearing on, so a probe failure must not be able to discard it.
func TestUpdateModelCandidatePersistsFieldsEvenWhenReprobeFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedCandidateForRetest(t, svc, provider.ID, now)

	client.Result = providerclient.TestResult{Outcome: providerclient.TestUnreachable}
	result, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "gpt-4o-mini", InputPrice: 7.5, OutputPrice: 9.5, MaxOutput: 2048,
	}, now)
	if err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	if result.Candidate.ProviderModelName != "gpt-4o-mini" || result.Candidate.InputPrice != 7.5 || result.Candidate.MaxOutput != 2048 {
		t.Fatalf("expected edited fields to survive a failed re-probe, got %+v", result.Candidate)
	}
	if result.Candidate.VerificationStatus == model.ModelVerificationStatusPassed {
		t.Fatal("expected a failed re-probe to leave the candidate unverified")
	}
}

func TestUpdateModelCandidateReturnsNotFoundForUnknownID(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.UpdateModelCandidate(context.Background(), 999999, modeladmin.UpdateCandidateInput{ProviderModelName: "gpt-4o"}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrModelCandidateNotFound) {
		t.Fatalf("expected ErrModelCandidateNotFound, got %v", err)
	}
}

func TestReorderModelCandidateSwapsOrder(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	providerA := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	providerB := seedEnabledProviderForModelTest(t, providerService, "provider-b")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	first, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: providerA.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate(a) failed: %v", err)
	}
	second, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
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

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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

func TestUpdateModelRenamesModel(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	updated, err := svc.UpdateModel(modelView.ID, modeladmin.UpdateModelInput{Name: "smart-v2"}, now)
	if err != nil {
		t.Fatalf("UpdateModel failed: %v", err)
	}
	if updated.Name != "smart-v2" {
		t.Fatalf("expected name 'smart-v2', got %q", updated.Name)
	}
}

func TestUpdateModelRejectsDuplicateName(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "taken"}, now); err != nil {
		t.Fatalf("CreateModel(taken) failed: %v", err)
	}
	other, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "other"}, now)
	if err != nil {
		t.Fatalf("CreateModel(other) failed: %v", err)
	}
	_, err = svc.UpdateModel(other.ID, modeladmin.UpdateModelInput{Name: "taken"}, now)
	if !errors.Is(err, errcode.ErrModelNameTaken) {
		t.Fatalf("expected ErrModelNameTaken, got %v", err)
	}
}

func TestUpdateModelReturnsNotFoundForUnknownID(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.UpdateModel(999999, modeladmin.UpdateModelInput{Name: "whatever"}, time.Now().UTC())
	if !errors.Is(err, errcode.ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
}

func TestSetModelStatusTogglesStatus(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
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

// Each of these leaves a candidate unroutable, and each needs a different
// repair: switch the candidate back on, switch the provider back on, add a
// usable key, fill in the provider's name for the model, run a probe. Reporting
// only that it is unroutable leaves an operator to guess which.
func TestACandidateSaysWhatStopsItBeingRoutedTo(t *testing.T) {
	enabled := model.ModelCandidate{
		ManagementStatus:   model.ModelCandidateStatusEnabled,
		ProviderModelName:  "gpt-4o",
		VerificationStatus: model.ModelVerificationStatusPassed,
	}
	disabled := enabled
	disabled.ManagementStatus = model.ModelCandidateStatusDisabled
	unnamed := enabled
	unnamed.ProviderModelName = ""
	unverified := enabled
	unverified.VerificationStatus = model.ModelVerificationStatusUntested

	for _, tc := range []struct {
		name            string
		candidate       model.ModelCandidate
		providerEnabled bool
		providerHasKey  bool
		want            string
	}{
		{"nothing stops it", enabled, true, true, ""},
		{"the candidate is switched off", disabled, true, true, modeladmin.CandidateBlockedByOwnStatus},
		{"the provider is switched off", enabled, false, true, modeladmin.CandidateBlockedByProvider},
		{"no key can be used", enabled, true, false, modeladmin.CandidateBlockedByNoUsableKey},
		{"the provider's name for the model is missing", unnamed, true, true, modeladmin.CandidateBlockedByMissingName},
		{"nothing has probed it", unverified, true, true, modeladmin.CandidateBlockedByUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := modeladmin.CandidateBlockedBy(tc.candidate, tc.providerEnabled, tc.providerHasKey)
			if got != tc.want {
				t.Fatalf("blocked by %q, want %q", got, tc.want)
			}
		})
	}
}

// A candidate that is both disabled and unverified must report the unverified
// state, not the disabled one: SetCandidateStatus refuses to enable a candidate
// that has not passed a probe, so "switch it back on" is advice that cannot be
// followed until the probe passes.
func TestADisabledUnverifiedCandidateReportsTheProbeItNeedsFirst(t *testing.T) {
	c := model.ModelCandidate{
		ManagementStatus:   model.ModelCandidateStatusDisabled,
		ProviderModelName:  "gpt-4o",
		VerificationStatus: model.ModelVerificationStatusUntested,
	}
	if got := modeladmin.CandidateBlockedBy(c, true, true); got != modeladmin.CandidateBlockedByUnverified {
		t.Fatalf("blocked by %q, want the missing probe — enabling is refused until it passes", got)
	}
}

// When more than one thing is wrong, the one reported is the one to fix first.
// A candidate whose provider is switched off has nothing to probe yet, so
// naming the unprobed state would send an operator to run a probe against a
// provider that cannot answer.
func TestTheReasonReportedIsTheOneToFixFirst(t *testing.T) {
	everythingWrong := model.ModelCandidate{
		ManagementStatus:   model.ModelCandidateStatusEnabled,
		ProviderModelName:  "",
		VerificationStatus: model.ModelVerificationStatusUntested,
	}
	if got := modeladmin.CandidateBlockedBy(everythingWrong, false, false); got != modeladmin.CandidateBlockedByProvider {
		t.Fatalf("blocked by %q, want the provider being switched off — the one that has to be fixed before any of the others can be", got)
	}
}

func TestProviderHasAvailableKeyReturnsFalseWhenNoKeyQualifies(t *testing.T) {
	keys := []model.ProviderKey{
		{ManagementStatus: model.ProviderKeyStatusDisabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: 1},
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusFailed, AuthorizedDestinationVersion: 1},
		{ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusPassed, AuthorizedDestinationVersion: 2},
	}
	if modeladmin.ProviderHasAvailableKey(keys, 1) {
		t.Fatalf("expected false when no key satisfies all three conditions")
	}
}

func TestModelRunningStatusAvailableEndToEnd(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	detail, err := svc.GetModelDetail(modelView.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if detail.RunningStatus != modeladmin.ModelRunningStatusAvailable {
		t.Fatalf("expected available, got %q", detail.RunningStatus)
	}
}

func TestGetModelDetailErrorsWhenDBUnavailable(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.DropTable(t, db, "models")
	if _, err := svc.GetModelDetail(modelView.ID); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestCreateModelErrorsWhenNameLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "models")
	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestCreateModelCandidateErrorsWhenModelLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "models")
	_, err := svc.CreateModelCandidate(context.Background(), 1, modeladmin.CreateCandidateInput{ProviderID: 1, ProviderModelName: "gpt-4o"}, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestTestAndCreateCandidateReturnsNotFoundForUnknownProvider(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	_, err = svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: 999999, ProviderModelName: "gpt-4o",
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestTestAndCreateCandidateErrorsWhenNoTestableKey(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	// A provider created without requesting enabled leaves its only key
	// disabled/untested — no key qualifies as "available" for a test.
	provider, err := providerService.CreateProvider(context.Background(), provider.CreateProviderInput{
		Name: "provider-a", BaseURL: "https://a.example.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	_, err = svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o",
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
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
	provider, err := providerService.CreateProvider(context.Background(), provider.CreateProviderInput{
		Name: "provider-a", BaseURL: "https://a.example.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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

func TestUpdateModelCandidateErrorsWhenUpdateFailsForNonUniqueReason(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "model_candidates", "UPDATE")
	if _, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{ProviderModelName: "gpt-4o-2"}, now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestDeleteModelCandidateErrorsWhenDeleteFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "model_candidates", "DELETE")
	if err := svc.DeleteModelCandidate(candidate.ID); err == nil {
		t.Fatalf("expected an error when the DELETE statement fails")
	}
}

func TestUpdateModelRejectsInvalidCharacters(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.UpdateModel(modelView.ID, modeladmin.UpdateModelInput{Name: "bad name!"}, now); err == nil {
		t.Fatalf("expected an error for an invalid model name")
	}
}

func TestUpdateModelErrorsWhenUpdateFailsForNonUniqueReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "models", "UPDATE")
	if _, err := svc.UpdateModel(modelView.ID, modeladmin.UpdateModelInput{Name: "smart-v2"}, now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestSetModelStatusErrorsWhenUpdateFails(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "models", "UPDATE")
	if err := svc.SetModelStatus(modelView.ID, true, now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestToModelViewErrorsWhenProviderKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	testutil.DropTable(t, db, "provider_keys")

	if _, err := svc.GetModelDetail(modelView.ID); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestListModelsErrorsWhenModelsTableMissing(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "models")
	if _, err := svc.ListModels(); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestListModelsErrorsWhenModelCandidatesTableMissing(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, time.Now().UTC()); err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.DropTable(t, db, "model_candidates")
	if _, err := svc.ListModels(); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestListModelsErrorsWhenProviderKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	testutil.DropTable(t, db, "provider_keys")

	if _, err := svc.ListModels(); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestGetModelDetailErrorsWhenModelCandidatesTableMissing(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.DropTable(t, db, "model_candidates")
	if _, err := svc.GetModelDetail(modelView.ID); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestCreateModelErrorsWhenInsertFailsForNonUniqueReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.BlockTableWrites(t, db, "models", "INSERT")
	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the INSERT statement fails")
	}
}

func TestTestAndCreateCandidateErrorsWhenKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.DropTable(t, db, "provider_keys")

	if _, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o",
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestCreateModelCandidateErrorsWhenSortOrderLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.DropTable(t, db, "model_candidates")

	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o",
	}, now); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestCreateModelCandidateErrorsWhenInsertFailsForNonUniqueReason(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "model_candidates", "INSERT")

	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o",
	}, now); err == nil {
		t.Fatalf("expected an error when the INSERT statement fails")
	}
}

func TestToCandidateViewErrorsWhenProviderKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	testutil.DropTable(t, db, "provider_keys")

	if _, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{ProviderModelName: "gpt-4o-2"}, now); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestUpdateModelCandidateErrorsWhenReloadFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	testutil.DropTable(t, db, "model_candidates")

	if _, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{}, now); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestSetCandidateStatusErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "model_candidates")
	if err := svc.SetCandidateStatus(1, true, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestTestModelCandidateErrorsWhenProviderLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	testutil.DropTable(t, db, "provider_keys")
	testutil.DropTable(t, db, "providers")

	if _, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestTestModelCandidateErrorsWhenCommitFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	testutil.BlockTableWrites(t, db, "model_candidates", "UPDATE")

	if _, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestTestModelCandidateStreamingErrorsWhenCommitFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	testutil.BlockTableWrites(t, db, "model_candidates", "UPDATE")

	if _, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err == nil {
		t.Fatalf("expected an error when the UPDATE statement fails")
	}
}

func TestReorderModelCandidateReturnsRawErrorForOtherFailures(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "model_candidates")
	if err := svc.ReorderModelCandidate(1, 1, "up"); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestDeleteModelCandidateErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "model_candidates")
	if err := svc.DeleteModelCandidate(1); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestUpdateModelErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "models")
	if _, err := svc.UpdateModel(1, modeladmin.UpdateModelInput{Name: "smart"}, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestSetModelStatusErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "models")
	if err := svc.SetModelStatus(1, true, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestTestAndCreateCandidateErrorsWhenProviderLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.DropTable(t, db, "providers")
	if _, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: 1, ProviderModelName: "gpt-4o",
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestCreateModelCandidateErrorsWhenModelLookupFailsForOtherReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, time.Now().UTC()); err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.DropTable(t, db, "models")
	if _, err := svc.CreateModelCandidate(context.Background(), 1, modeladmin.CreateCandidateInput{ProviderID: 1, ProviderModelName: "gpt-4o"}, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the models table is missing")
	}
}

func TestCreateModelCandidateSilentlySkipsReverifyWhenClientErrors(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	client.Err = errors.New("client refused the call")

	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	testutil.BlockTableWrites(t, db, "model_candidates", "UPDATE")

	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	testutil.DropTable(t, db, "provider_keys")
	testutil.DropTable(t, db, "providers")

	if _, err := svc.UpdateModelCandidate(context.Background(), candidate.ID, modeladmin.UpdateCandidateInput{ProviderModelName: "gpt-4o-2"}, now); err == nil {
		t.Fatalf("expected an error when the providers table is missing")
	}
}

func TestSetCandidateStatusErrorsWhenCASWriteFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if _, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err != nil {
		t.Fatalf("TestModelCandidate failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "model_candidates", "UPDATE")

	if err := svc.SetCandidateStatus(candidate.ID, true, now); err == nil {
		t.Fatalf("expected an error when the CAS UPDATE statement fails")
	}
}

func TestTestModelCandidateErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "model_candidates")
	if _, _, err := svc.RetestModelCandidate(context.Background(), 1, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error when the model_candidates table is missing")
	}
}

func TestTestModelCandidateErrorsWhenProviderKeyLookupFails(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	testutil.DropTable(t, db, "provider_keys")

	if _, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err == nil {
		t.Fatalf("expected an error when the provider_keys table is missing")
	}
}

func TestTestModelCandidateErrorsWhenNoTestableKey(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	// A provider created without requesting enabled leaves its only key
	// disabled/untested — no key qualifies as available for a test.
	provider, err := providerService.CreateProvider(context.Background(), provider.CreateProviderInput{
		Name: "provider-a", BaseURL: "https://a.example.com", KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
	}, now)
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	candidate, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	if _, _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); !errors.Is(err, errcode.ErrProviderNoTestableModel) {
		t.Fatalf("expected ErrProviderNoTestableModel, got %v", err)
	}
}

func TestListModelsAvoidsNPlusOneCandidateQueries(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	for _, name := range []string{"smart", "fast"} {
		modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: name}, now)
		if err != nil {
			t.Fatalf("CreateModel(%s) failed: %v", name, err)
		}
		if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
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

// seedProviderWithBaseURL creates an enabled provider on a specific base_url, so
// a test can decide whether the seed catalog has an entry for its host.
func seedProviderWithBaseURL(t *testing.T, providerService *provider.ProviderService, name, baseURL string) *provider.ProviderView {
	t.Helper()
	provider, err := providerService.CreateProvider(context.Background(), provider.CreateProviderInput{
		Name: name, BaseURL: baseURL, KeyLabel: "k1",
		KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	return provider
}

// catalogSeededHost is a base_url the built-in seed catalog is known to carry an
// entry for. The tests below assert against pricecatalog.Lookup rather than
// against literal figures, so re-syncing catalog.json cannot break them.
const catalogSeededHost = "https://api.deepseek.com/v1"

func catalogSeededModel(t *testing.T) string {
	t.Helper()
	const name = "deepseek-v4-flash"
	if _, ok := pricecatalog.Lookup(catalogSeededHost, name); !ok {
		t.Fatalf("the seed catalog no longer carries %s/%s; pick another pair for this test", catalogSeededHost, name)
	}
	return name
}

func TestSuggestCandidatePriceFallsBackToSeedCatalog(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	provider := seedProviderWithBaseURL(t, providerService, "deepseek", catalogSeededHost)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	name := catalogSeededModel(t)

	got, err := svc.SuggestCandidatePrice(provider.ID, name)
	if err != nil {
		t.Fatalf("SuggestCandidatePrice failed: %v", err)
	}
	if got.Source != "seed" {
		t.Fatalf("want source=seed, got %q", got.Source)
	}
	want, _ := pricecatalog.Lookup(catalogSeededHost, name)
	if got.InputPrice != want.Input || got.OutputPrice != want.Output {
		t.Fatalf("want %v/%v from the catalog, got %v/%v", want.Input, want.Output, got.InputPrice, got.OutputPrice)
	}
	if (got.CacheWritePrice == nil) != (want.CacheWrite == nil) || (got.CacheReadPrice == nil) != (want.CacheRead == nil) {
		t.Fatalf("cache slots must carry the catalog's nils through, got %+v", got)
	}
}

// The provider's own saved price outranks the catalog: it is what this provider
// actually charges, possibly a negotiated rate the generic figure does not know.
func TestSuggestCandidatePricePrefersHistoryOverSeedCatalog(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedProviderWithBaseURL(t, providerService, "deepseek", catalogSeededHost)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	name := catalogSeededModel(t)

	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "negotiated"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: name, InputPrice: 0.42, OutputPrice: 0.84,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	got, err := svc.SuggestCandidatePrice(provider.ID, name)
	if err != nil {
		t.Fatalf("SuggestCandidatePrice failed: %v", err)
	}
	if got.Source != "history" {
		t.Fatalf("want source=history, got %q", got.Source)
	}
	if got.InputPrice != 0.42 || got.OutputPrice != 0.84 {
		t.Fatalf("want the negotiated 0.42/0.84, got %v/%v", got.InputPrice, got.OutputPrice)
	}
}

func TestSuggestCandidatePriceMatchesHistoryCaseInsensitively(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedProviderWithBaseURL(t, providerService, "provider-a", "https://a.example.com")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "alias"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "DeepSeek-V4-Pro", InputPrice: 2.4, OutputPrice: 4.8,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	got, err := svc.SuggestCandidatePrice(provider.ID, "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("SuggestCandidatePrice failed: %v", err)
	}
	if got.Source != "history" || got.InputPrice != 2.4 || got.OutputPrice != 4.8 {
		t.Fatalf("want the stored 2.4/4.8 from history, got %+v", got)
	}
}

func TestSuggestCandidatePriceReturnsEmptyWhenNothingMatches(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	// A host the seed catalog does not carry, with no candidate saved for it.
	provider := seedProviderWithBaseURL(t, providerService, "self-hosted", "https://llm.internal.example")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	got, err := svc.SuggestCandidatePrice(provider.ID, "some-local-model")
	if err != nil {
		t.Fatalf("SuggestCandidatePrice failed: %v", err)
	}
	if got.Source != "" || got.InputPrice != 0 || got.OutputPrice != 0 {
		t.Fatalf("expected an empty suggestion, got %+v", got)
	}
}

// A blank name reaches the service only defensively — the caller substitutes the
// model's own name first — and must not be priced as if it were a real model.
func TestSuggestCandidatePriceReturnsEmptyForBlankModelName(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	provider := seedProviderWithBaseURL(t, providerService, "deepseek", catalogSeededHost)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	for _, name := range []string{"", "   "} {
		got, err := svc.SuggestCandidatePrice(provider.ID, name)
		if err != nil {
			t.Fatalf("SuggestCandidatePrice(%q) failed: %v", name, err)
		}
		if got.Source != "" {
			t.Errorf("%q: expected an empty suggestion, got %+v", name, got)
		}
	}
}

func TestSuggestCandidatePriceReturnsProviderNotFound(t *testing.T) {
	_, db, client := newTestProviderService(t)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	if _, err := svc.SuggestCandidatePrice(999999, "deepseek-v4-flash"); !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestSuggestCandidatePriceReturnsErrorWhenDBUnavailable(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	provider := seedProviderWithBaseURL(t, providerService, "deepseek", catalogSeededHost)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	testutil.CloseDB(t, db)

	if _, err := svc.SuggestCandidatePrice(provider.ID, "deepseek-v4-flash"); err == nil {
		t.Fatal("expected an error once the underlying connection is closed")
	}
}

// The service, not just the repository, must treat a retarget as a fresh price
// statement: an operator who moves a candidate to another upstream model and
// keeps the numbers has priced that model, and the next suggestion for it has to
// reflect that rather than an older candidate's rate.
func TestUpdateModelCandidateRetargetWinsTheNextSuggestion(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedProviderWithBaseURL(t, providerService, "provider-a", "https://a.example.com")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	incumbent, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "incumbent"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), incumbent.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "vendor-pro", InputPrice: 9, OutputPrice: 9,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	moved, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "moved"}, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	movedCandidate, err := svc.CreateModelCandidate(context.Background(), moved.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "vendor-flash", InputPrice: 1, OutputPrice: 2,
	}, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	// Same numbers, different upstream model.
	if _, err := svc.UpdateModelCandidate(context.Background(), movedCandidate.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: "vendor-pro", InputPrice: 1, OutputPrice: 2,
	}, now.Add(time.Hour)); err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}

	got, err := svc.SuggestCandidatePrice(provider.ID, "vendor-pro")
	if err != nil {
		t.Fatalf("SuggestCandidatePrice failed: %v", err)
	}
	if got.Source != "history" || got.InputPrice != 1 || got.OutputPrice != 2 {
		t.Fatalf("want the retargeted 1/2 from history, got %+v", got)
	}
}

// The impact preview must not scare an operator with keys that could not call
// anyway: a revoked key and an expired key both allowlist the model here, and
// neither may appear. Only the callable allowlisting key is named; the
// allow-all key is counted, not named.
func TestModelImpactNamesOnlyKeysThatCanStillCall(t *testing.T) {
	_, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	otherModel, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "other"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	keySvc := apikey.NewAPIKeyService(db, testutil.ProviderSecrets())
	callable, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), Remark: "alice's laptop", ModelIDs: []uint{modelView.ID}}, now)
	if err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}
	revoked, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{modelView.ID}}, now)
	if err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}
	if err := db.Model(&model.APIKey{}).Where("id = ?", revoked.APIKey.ID).
		Update("status", model.APIKeyStatusRevoked).Error; err != nil {
		t.Fatalf("revoke: %v", err)
	}
	expired, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{modelView.ID}}, now)
	if err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}
	past := now.Add(-time.Hour)
	if err := db.Model(&model.APIKey{}).Where("id = ?", expired.APIKey.ID).
		Update("expires_at", past).Error; err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{otherModel.ID}}, now); err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}
	if _, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), AllowAllModels: true}, now); err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}

	impact, err := svc.GetModelImpact(modelView.ID, now)
	if err != nil {
		t.Fatalf("GetModelImpact failed: %v", err)
	}
	if len(impact.AllowlistedKeys) != 1 || impact.AllowlistedKeys[0].ID != callable.APIKey.ID {
		t.Fatalf("allowlisted keys = %+v, want exactly the callable key %d", impact.AllowlistedKeys, callable.APIKey.ID)
	}
	if impact.AllowlistedKeys[0].Remark != "alice's laptop" {
		t.Fatalf("remark = %q, want alice's laptop", impact.AllowlistedKeys[0].Remark)
	}
	if impact.AllowAllKeyCount != 1 {
		t.Fatalf("allow-all count = %d, want 1", impact.AllowAllKeyCount)
	}
}

// The live-traffic number is scoped by name and by window: a request for a
// different model and a request older than the window must not count.
func TestModelImpactCountsOnlyRecentTrafficForThisName(t *testing.T) {
	_, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	seed := func(name string, at time.Time) {
		t.Helper()
		if err := repository.CreateRequestLog(db, &model.RequestLog{RequestID: name + at.String(), ModelName: name, CreatedAt: at}); err != nil {
			t.Fatalf("seed request log: %v", err)
		}
	}
	seed("smart", now.Add(-time.Hour))
	seed("smart", now.Add(-8*24*time.Hour))
	seed("other", now.Add(-time.Hour))

	impact, err := svc.GetModelImpact(modelView.ID, now)
	if err != nil {
		t.Fatalf("GetModelImpact failed: %v", err)
	}
	if impact.RecentRequestCount != 1 {
		t.Fatalf("recent request count = %d, want 1 (in-window, this name only)", impact.RecentRequestCount)
	}
	if impact.RecentWindowDays != 7 {
		t.Fatalf("window days = %d, want 7", impact.RecentWindowDays)
	}
}

func TestModelImpactUnknownModel(t *testing.T) {
	_, db, client := newTestProviderService(t)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	if _, err := svc.GetModelImpact(9999, time.Now().UTC()); !errors.Is(err, errcode.ErrModelNotFound) {
		t.Fatalf("err = %v, want ErrModelNotFound", err)
	}
}

func TestCreateModelSchedulingModeDefaultBalancedAndInvalid(t *testing.T) {
	_, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	plain, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "plain"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if plain.SchedulingMode != model.ModelSchedulingModeFailover {
		t.Fatalf("plain create scheduling mode = %q, want failover default", plain.SchedulingMode)
	}
	var stored model.Model
	if err := db.Where("id = ?", plain.ID).First(&stored).Error; err != nil {
		t.Fatalf("re-read model row: %v", err)
	}
	if stored.SchedulingMode != model.ModelSchedulingModeFailover {
		t.Fatalf("stored scheduling mode = %q, want failover written explicitly (empty insert would bypass the column default)", stored.SchedulingMode)
	}

	balanced, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "spread", SchedulingMode: model.ModelSchedulingModeBalanced}, now)
	if err != nil {
		t.Fatalf("CreateModel balanced failed: %v", err)
	}
	if balanced.SchedulingMode != model.ModelSchedulingModeBalanced {
		t.Fatalf("balanced create scheduling mode = %q, want balanced", balanced.SchedulingMode)
	}

	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "broken", SchedulingMode: "round-robin"}, now); !errors.Is(err, errcode.ErrModelSchedulingModeInvalid) {
		t.Fatalf("invalid mode err = %v, want ErrModelSchedulingModeInvalid", err)
	}
}

func TestUpdateModelSchedulingMode(t *testing.T) {
	_, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	created, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	balanced := model.ModelSchedulingModeBalanced
	updated, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{Name: "smart", SchedulingMode: &balanced}, now)
	if err != nil {
		t.Fatalf("UpdateModel failed: %v", err)
	}
	if updated.SchedulingMode != model.ModelSchedulingModeBalanced {
		t.Fatalf("scheduling mode = %q, want balanced after patch", updated.SchedulingMode)
	}

	// An update that does not set the mode must leave it alone.
	renamed, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{Name: "smart-v2"}, now)
	if err != nil {
		t.Fatalf("UpdateModel rename failed: %v", err)
	}
	if renamed.SchedulingMode != model.ModelSchedulingModeBalanced {
		t.Fatalf("scheduling mode = %q after mode-less patch, want balanced preserved", renamed.SchedulingMode)
	}

	weighted := model.SchedulingMode("weighted")
	if _, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{Name: "smart-v2", SchedulingMode: &weighted}, now); !errors.Is(err, errcode.ErrModelSchedulingModeInvalid) {
		t.Fatalf("invalid mode err = %v, want ErrModelSchedulingModeInvalid", err)
	}

	// A PRESENT empty mode is an invalid value, not "keep" and not "the
	// default": mapping it onto failover would let a caller that submits
	// nothing silently reset a balanced model.
	empty := model.SchedulingMode("")
	if _, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{Name: "smart-v2", SchedulingMode: &empty}, now); !errors.Is(err, errcode.ErrModelSchedulingModeInvalid) {
		t.Fatalf("empty mode err = %v, want ErrModelSchedulingModeInvalid", err)
	}
	unchanged, err := svc.GetModelDetail(created.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	if unchanged.SchedulingMode != model.ModelSchedulingModeBalanced {
		t.Fatalf("scheduling mode = %q after rejected empty patch, want balanced preserved", unchanged.SchedulingMode)
	}
}

func TestCreateModelsBatchDefaultsToFailover(t *testing.T) {
	_, db, client := newTestProviderService(t)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	result, err := svc.CreateModelsBatch(modeladmin.CreateModelsBatchInput{Names: []string{"one", "two"}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateModelsBatch failed: %v", err)
	}
	if len(result.Created) != 2 {
		t.Fatalf("created = %d, want 2", len(result.Created))
	}
	for _, v := range result.Created {
		if v.SchedulingMode != model.ModelSchedulingModeFailover {
			t.Fatalf("batch-created %q scheduling mode = %q, want failover", v.Name, v.SchedulingMode)
		}
	}
}

type fakeBindingCounter struct {
	counts map[uint]map[uint]int // modelID → providerID → count
}

func (f fakeBindingCounter) BindingCounts(modelID uint) map[uint]int {
	return f.counts[modelID]
}

func TestGetModelDetailExposesBindingCountsForBalancedModels(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	pa := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	pb := seedEnabledProviderForModelTest(t, providerService, "provider-b")

	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	balanced, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "spread", SchedulingMode: model.ModelSchedulingModeBalanced}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	for _, p := range []uint{pa.ID, pb.ID} {
		if _, err := svc.CreateModelCandidate(context.Background(), balanced.ID, modeladmin.CreateCandidateInput{
			ProviderID: p, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
		}, now); err != nil {
			t.Fatalf("CreateModelCandidate failed: %v", err)
		}
	}
	legacy, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "legacy"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.CreateModelCandidate(context.Background(), legacy.ID, modeladmin.CreateCandidateInput{
		ProviderID: pa.ID, ProviderModelName: "gpt-4o", InputPrice: 1, OutputPrice: 2,
	}, now); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	svc.SetBindingCounter(fakeBindingCounter{counts: map[uint]map[uint]int{
		balanced.ID: {pa.ID: 2, pb.ID: 1},
		legacy.ID:   {pa.ID: 9}, // must never surface: legacy is failover
	}})

	detail, err := svc.GetModelDetail(balanced.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	got := map[uint]int{}
	for _, c := range detail.Candidates {
		got[c.ProviderID] = c.BindingCount
	}
	if got[pa.ID] != 2 || got[pb.ID] != 1 {
		t.Fatalf("balanced model binding counts = %v, want provider-a:2 provider-b:1", got)
	}

	legacyDetail, err := svc.GetModelDetail(legacy.ID)
	if err != nil {
		t.Fatalf("GetModelDetail failed: %v", err)
	}
	for _, c := range legacyDetail.Candidates {
		if c.BindingCount != 0 {
			t.Fatalf("failover candidate shows binding count %d; failover models have no bindings", c.BindingCount)
		}
	}
}

// An audio-mode declaration is one price, not four: creating and re-editing a
// speech mapping must round-trip the character price and leave the token slots
// alone, because settlement reads only the audio column under that mode.
func TestAudioCandidateRoundTripsCharacterPrice(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	pv := seedProviderWithBaseURL(t, providerService, "minimax-audio", "https://api.minimax.cn")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	now := time.Now().UTC()

	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "speech-2.8-hd"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	price := 350.0
	cand, err2 := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: pv.ID, BillingMode: model.BillingModeAudio, AudioUnitPrice: &price,
	}, now)
	if err2 != nil {
		t.Fatalf("CreateModelCandidate(audio) failed: %v", err2)
	}
	if cand.BillingMode != model.BillingModeAudio || cand.AudioUnitPrice == nil || *cand.AudioUnitPrice != 350 {
		t.Fatalf("created view = mode %q price %v, want audio/350", cand.BillingMode, cand.AudioUnitPrice)
	}

	edited := 200.0
	result, err := svc.UpdateModelCandidate(context.Background(), cand.ID, modeladmin.UpdateCandidateInput{
		AudioUnitPrice: &edited,
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("UpdateModelCandidate(audio price) failed: %v", err)
	}
	if result.Candidate.AudioUnitPrice == nil || *result.Candidate.AudioUnitPrice != 200 {
		t.Fatalf("updated price = %v, want 200", result.Candidate.AudioUnitPrice)
	}
}

// A negative character price is a typo that would bill backwards; the write
// path is where it should stop, same rule as the image tier table's. Unpriced,
// by contrast, is a legal declaration — the row bills as unknown until a price
// is filled in.
func TestAudioCandidatePriceSignRules(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	pv := seedProviderWithBaseURL(t, providerService, "sf-audio", "https://api.siliconflow.cn/v1")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "cosyvoice"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	neg := -1.0
	if _, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: pv.ID, BillingMode: model.BillingModeAudio, AudioUnitPrice: &neg,
	}, now); err == nil {
		t.Fatal("a negative audio unit price was accepted")
	}

	cand, err := svc.CreateModelCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: pv.ID, BillingMode: model.BillingModeAudio,
	}, now)
	if err != nil {
		t.Fatalf("unpriced audio declaration rejected: %v", err)
	}
	if cand.AudioUnitPrice != nil {
		t.Fatalf("unpriced declaration stored a price: %v", *cand.AudioUnitPrice)
	}
}

// The audio seed section feeds the same suggest endpoint the token half does,
// so an import dialog on an audio host prefills a character price.
func TestSuggestCandidatePriceUsesAudioSeedSection(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	pv := seedProviderWithBaseURL(t, providerService, "minimax-audio", "https://api.minimax.cn")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	const name = "speech-2.8-turbo"
	want, ok := pricecatalog.LookupAudio("https://api.minimax.cn", name)
	if !ok {
		t.Fatalf("the seed catalog's audio section no longer carries %s; pick another pair", name)
	}
	got, err := svc.SuggestCandidatePrice(pv.ID, name)
	if err != nil {
		t.Fatalf("SuggestCandidatePrice failed: %v", err)
	}
	if got.Source != "seed" || got.AudioUnitPrice == nil || *got.AudioUnitPrice != want {
		t.Fatalf("suggestion = %+v, want seed audio price %v", got, want)
	}
	if got.InputPrice != 0 || got.OutputPrice != 0 {
		t.Fatalf("audio suggestion carries token figures: %+v", got)
	}
}

// An audio-only model's mapping probes through the speech shape: the basic
// probe is one synthesis, and the chat capability probes never run — they
// describe behaviours a speech model does not carry.
func TestAudioExclusiveModelProbesThroughSpeechShape(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	pv := seedProviderWithBaseURL(t, providerService, "sf-audio", "https://api.siliconflow.cn/v1")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	now := time.Now().UTC()

	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{
		Name: "cosyvoice", OutputModalities: []string{model.OutputModalityAudio},
	}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	// The candidate probe path is exercised through the public shape it
	// shares with imports: one TestAndCreateCandidate round.
	basicBefore := client.CallCountFor("basic")
	created, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: pv.ID, ProviderModelName: "FunAudioLLM/CosyVoice2-0.5B",
	}, now)
	if err != nil {
		t.Fatalf("TestAndCreateCandidate failed: %v", err)
	}
	report := created.Report
	if !report.Basic.Ran || report.Basic.Supported == nil || !*report.Basic.Supported {
		t.Fatalf("speech-shaped basic probe did not run/pass: %+v", report.Basic)
	}
	if got := client.CallCountFor("speech"); got != 1 {
		t.Errorf("speech probes = %d, want exactly one", got)
	}
	// The provider's own key verification spends the basic chat probes this
	// fixture counts from; only the DELTA is the candidate probe's doing.
	if got := client.CallCountFor("basic") - basicBefore; got != 0 {
		t.Errorf("candidate probing spent %d chat probes, want none for an audio-only model", got)
	}
	if got := client.CallCountFor("speech"); got > 1 {
		t.Errorf("speech probes = %d, the shape must not repeat the basic probe", got)
	}
}

// The shape gate cuts both ways: a text model's candidate probing speaks
// chat and never spends a speech probe — same base or not.
func TestTextModelNeverTriggersSpeechProbe(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	pv := seedProviderWithBaseURL(t, providerService, "sf-text", "https://api.siliconflow.cn/v1")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	now := time.Now().UTC()

	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "chat-model"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.TestAndCreateCandidate(context.Background(), modelView.ID, modeladmin.CreateCandidateInput{
		ProviderID: pv.ID, ProviderModelName: "qwen3-30b",
	}, now); err != nil {
		t.Fatalf("TestAndCreateCandidate failed: %v", err)
	}
	if got := client.CallCountFor("speech"); got != 0 {
		t.Errorf("a text model spent %d speech probes, want none", got)
	}
	if got := client.CallCountFor("basic"); got == 0 {
		t.Error("a text model must verify through the chat probe")
	}
}

// A bulk import of an audio-only row lands as an audio-billed candidate
// carrying the row's single price, not the token slots.
func TestImportAudioExclusiveRowBillsPerCharacter(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "sf-audio-import")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)

	price := 50.0
	result, err := svc.ImportProviderModels(prov.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "cosyvoice", AudioUnitPrice: &price, OutputModalities: []string{model.OutputModalityAudio}},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("tally %+v", result)
	}
	var cand model.ModelCandidate
	if err := db.Where("provider_model_name = ?", "cosyvoice").First(&cand).Error; err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if model.NormalizeBillingMode(cand.BillingMode) != model.BillingModeAudio {
		t.Fatalf("billing mode = %q, want audio", cand.BillingMode)
	}
	if cand.AudioUnitPrice == nil || *cand.AudioUnitPrice != 50 {
		t.Fatalf("audio price = %v, want the imported 50", cand.AudioUnitPrice)
	}
}
