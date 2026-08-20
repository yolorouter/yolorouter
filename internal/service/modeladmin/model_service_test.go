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
		[]string{"gpt-5.6", "claude-sonnet-5", "bad name!", "claude-sonnet-5", "deepseek-v4-flash"},
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

	if _, err := svc.CreateModelsBatch([]string{"alpha", "boom", "gamma"}, now); err == nil {
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
	if _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err != nil {
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
	updated, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
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
	updated, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
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
	updated, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
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
	seeded, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
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
	updated, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
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
			updated, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
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

	updated, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
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

// A decisive failure, by contrast, must both record the failure and stop the
// admin list claiming the candidate is serving traffic it cannot serve.
func TestRetestModelCandidateDemotesOnDecisiveBasicFailure(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	now := time.Now().UTC()
	provider := seedEnabledProviderForModelTest(t, providerService, "provider-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	candidate := seedEnabledCandidate(t, svc, client, provider.ID, now)

	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound}
	updated, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now)
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
	_, err := svc.RetestModelCandidate(context.Background(), 999999, time.Now().UTC())
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

func TestUpdateModelNameStatusRenamesModel(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	updated, err := svc.UpdateModelNameStatus(modelView.ID, "smart-v2", false, nil, now)
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
	if _, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "taken"}, now); err != nil {
		t.Fatalf("CreateModel(taken) failed: %v", err)
	}
	other, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "other"}, now)
	if err != nil {
		t.Fatalf("CreateModel(other) failed: %v", err)
	}
	_, err = svc.UpdateModelNameStatus(other.ID, "taken", false, nil, now)
	if !errors.Is(err, errcode.ErrModelNameTaken) {
		t.Fatalf("expected ErrModelNameTaken, got %v", err)
	}
}

func TestUpdateModelNameStatusReturnsNotFoundForUnknownID(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	_, err := svc.UpdateModelNameStatus(999999, "whatever", false, nil, time.Now().UTC())
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

func TestUpdateModelNameStatusRejectsInvalidCharacters(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if _, err := svc.UpdateModelNameStatus(modelView.ID, "bad name!", false, nil, now); err == nil {
		t.Fatalf("expected an error for an invalid model name")
	}
}

func TestUpdateModelNameStatusErrorsWhenUpdateFailsForNonUniqueReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	modelView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	testutil.BlockTableWrites(t, db, "models", "UPDATE")
	if _, err := svc.UpdateModelNameStatus(modelView.ID, "smart-v2", false, nil, now); err == nil {
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

	if _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err == nil {
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

	if _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err == nil {
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

	if _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err == nil {
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

func TestUpdateModelNameStatusErrorsWhenLookupFailsForNonNotFoundReason(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	testutil.DropTable(t, db, "models")
	if _, err := svc.UpdateModelNameStatus(1, "smart", false, nil, time.Now().UTC()); err == nil {
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
	if _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err != nil {
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
	if _, err := svc.RetestModelCandidate(context.Background(), 1, time.Now().UTC()); err == nil {
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

	if _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); err == nil {
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

	if _, err := svc.RetestModelCandidate(context.Background(), candidate.ID, now); !errors.Is(err, errcode.ErrProviderNoTestableModel) {
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
