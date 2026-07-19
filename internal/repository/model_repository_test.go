package repository

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
)

func TestCreateModelAndFindByID(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	m := &model.Model{Name: "smart", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}

	if err := CreateModel(db, m); err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if m.ID == 0 {
		t.Fatalf("expected m.ID to be populated")
	}

	reloaded, err := FindModelByID(db, m.ID)
	if err != nil {
		t.Fatalf("FindModelByID failed: %v", err)
	}
	if reloaded.Name != "smart" {
		t.Fatalf("expected name 'smart', got %q", reloaded.Name)
	}
}

func TestCreateModelRejectsDuplicateName(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := CreateModel(db, &model.Model{Name: "smart", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("first CreateModel failed: %v", err)
	}
	err := CreateModel(db, &model.Model{Name: "smart", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now})
	if err == nil {
		t.Fatalf("expected a UNIQUE(name) violation on duplicate model name")
	}
}

func TestFindModelByNameReturnsNotFoundForUnknownName(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, err := FindModelByName(db, "does-not-exist")
	if err == nil {
		t.Fatalf("expected an error for an unknown model name")
	}
}

func TestListModelsOrdersByID(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := CreateModel(db, &model.Model{Name: "b-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateModel(b) failed: %v", err)
	}
	if err := CreateModel(db, &model.Model{Name: "a-model", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateModel(a) failed: %v", err)
	}
	list, err := ListModels(db)
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(list) != 2 || list[0].Name != "b-model" || list[1].Name != "a-model" {
		t.Fatalf("expected id-ascending order [b-model, a-model], got %+v", list)
	}
}

func TestUpdateModelNameStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	m := &model.Model{Name: "smart", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := CreateModel(db, m); err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	if err := UpdateModelNameStatus(db, m.ID, "smart-v2", model.ModelStatusDisabled, now); err != nil {
		t.Fatalf("UpdateModelNameStatus failed: %v", err)
	}
	reloaded, err := FindModelByID(db, m.ID)
	if err != nil {
		t.Fatalf("FindModelByID failed: %v", err)
	}
	if reloaded.Name != "smart-v2" || reloaded.ManagementStatus != model.ModelStatusDisabled {
		t.Fatalf("expected name='smart-v2' status=disabled, got %+v", reloaded)
	}
}

// seedModelWithCandidate creates a Model and one ModelCandidate pointing at
// a freshly-seeded Provider+Key (via the existing seedProviderWithKey
// helper), for tests that need a realistic candidate row to act on.
func seedModelWithCandidate(t *testing.T, db *gorm.DB, modelName, providerName string) (*model.Model, *model.Provider, *model.ModelCandidate) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	m := &model.Model{Name: modelName, ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := CreateModel(db, m); err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	provider, _ := seedProviderWithKey(t, db, providerName)
	candidate := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: provider.ID, ProviderModelName: "gpt-4o", SortOrder: 1,
		ManagementStatus: model.ModelCandidateStatusDisabled, VerificationStatus: model.ModelVerificationStatusUntested,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := CreateModelCandidate(db, candidate); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}
	return m, provider, candidate
}

func TestCreateModelCandidateRejectsDuplicateProviderOnSameModel(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m, provider, _ := seedModelWithCandidate(t, db, "smart", "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	dup := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: provider.ID, ProviderModelName: "gpt-4o-mini", SortOrder: 2,
		ManagementStatus: model.ModelCandidateStatusDisabled, CreatedAt: now, UpdatedAt: now,
	}
	if err := CreateModelCandidate(db, dup); err == nil {
		t.Fatalf("expected a UNIQUE(model_id, provider_id) violation")
	}
}

func TestListModelCandidatesByModelIDPreloadsProvider(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m, provider, _ := seedModelWithCandidate(t, db, "smart", "provider-a")

	candidates, err := ListModelCandidatesByModelID(db, m.ID)
	if err != nil {
		t.Fatalf("ListModelCandidatesByModelID failed: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Provider == nil || candidates[0].Provider.Name != provider.Name {
		t.Fatalf("expected Provider to be preloaded with name %q, got %+v", provider.Name, candidates[0].Provider)
	}
}

func TestListModelCandidatesByModelIDsReturnsNilForEmptyInput(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	candidates, err := ListModelCandidatesByModelIDs(db, nil)
	if err != nil {
		t.Fatalf("ListModelCandidatesByModelIDs failed: %v", err)
	}
	if candidates != nil {
		t.Fatalf("expected nil for empty input, got %+v", candidates)
	}
}

func TestListModelCandidatesByModelIDsGroupsAcrossMultipleModels(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m1, _, _ := seedModelWithCandidate(t, db, "smart", "provider-a")
	m2, _, _ := seedModelWithCandidate(t, db, "fast", "provider-b")

	candidates, err := ListModelCandidatesByModelIDs(db, []uint{m1.ID, m2.ID})
	if err != nil {
		t.Fatalf("ListModelCandidatesByModelIDs failed: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates across both models, got %d", len(candidates))
	}
}

func TestNextCandidateSortOrderStartsAtOne(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	m := &model.Model{Name: "smart", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := CreateModel(db, m); err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	next, err := NextCandidateSortOrder(db, m.ID)
	if err != nil {
		t.Fatalf("NextCandidateSortOrder failed: %v", err)
	}
	if next != 1 {
		t.Fatalf("expected 1 for a model with no candidates yet, got %d", next)
	}
}

func TestNextCandidateSortOrderIncrementsAfterExistingCandidate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m, _, _ := seedModelWithCandidate(t, db, "smart", "provider-a")
	next, err := NextCandidateSortOrder(db, m.ID)
	if err != nil {
		t.Fatalf("NextCandidateSortOrder failed: %v", err)
	}
	if next != 2 {
		t.Fatalf("expected 2 after one existing candidate at sort_order=1, got %d", next)
	}
}

func TestUpdateModelCandidate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, _, candidate := seedModelWithCandidate(t, db, "smart", "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	cacheWrite := 0.5
	cacheRead := 0.1

	if err := UpdateModelCandidate(db, candidate.ID, "gpt-4o-2024", 1.5, 3.0, &cacheWrite, &cacheRead, 4096, now); err != nil {
		t.Fatalf("UpdateModelCandidate failed: %v", err)
	}
	reloaded, err := FindModelCandidateByID(db, candidate.ID)
	if err != nil {
		t.Fatalf("FindModelCandidateByID failed: %v", err)
	}
	if reloaded.ProviderModelName != "gpt-4o-2024" || reloaded.InputPrice != 1.5 || reloaded.OutputPrice != 3.0 || reloaded.MaxOutput != 4096 {
		t.Fatalf("expected updated fields, got %+v", reloaded)
	}
	if reloaded.CacheWritePrice == nil || *reloaded.CacheWritePrice != 0.5 {
		t.Fatalf("expected cache_write_price=0.5, got %+v", reloaded.CacheWritePrice)
	}
}

func TestSetModelCandidateManagementStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, _, candidate := seedModelWithCandidate(t, db, "smart", "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	if err := SetModelCandidateManagementStatus(db, candidate.ID, model.ModelCandidateStatusEnabled, now); err != nil {
		t.Fatalf("SetModelCandidateManagementStatus failed: %v", err)
	}
	reloaded, err := FindModelCandidateByID(db, candidate.ID)
	if err != nil {
		t.Fatalf("FindModelCandidateByID failed: %v", err)
	}
	if reloaded.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected management_status=enabled, got %d", reloaded.ManagementStatus)
	}
}

// TestSetModelCandidateManagementStatusIfVerifiedAppliesWhenVerified and
// TestSetModelCandidateManagementStatusIfVerifiedSkipsWhenUntested are the
// direct regression tests for the CAS guard on enabling a candidate — same
// check-then-act race class M2 round 4 found and fixed for provider keys,
// applied here from the start.
func TestSetModelCandidateManagementStatusIfVerifiedAppliesWhenVerified(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, _, candidate := seedModelWithCandidate(t, db, "smart", "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", candidate.ID).
		Update("verification_status", model.ModelVerificationStatusPassed).Error; err != nil {
		t.Fatalf("seed verification_status failed: %v", err)
	}

	applied, err := SetModelCandidateManagementStatusIfVerified(db, candidate.ID, model.ModelCandidateStatusEnabled, now)
	if err != nil {
		t.Fatalf("SetModelCandidateManagementStatusIfVerified failed: %v", err)
	}
	if !applied {
		t.Fatalf("expected applied=true when verification_status=Passed")
	}
}

func TestSetModelCandidateManagementStatusIfVerifiedSkipsWhenUntested(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, _, candidate := seedModelWithCandidate(t, db, "smart", "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	applied, err := SetModelCandidateManagementStatusIfVerified(db, candidate.ID, model.ModelCandidateStatusEnabled, now)
	if err != nil {
		t.Fatalf("SetModelCandidateManagementStatusIfVerified failed: %v", err)
	}
	if applied {
		t.Fatalf("expected applied=false when verification_status is still Untested")
	}
	reloaded, err := FindModelCandidateByID(db, candidate.ID)
	if err != nil {
		t.Fatalf("FindModelCandidateByID failed: %v", err)
	}
	if reloaded.ManagementStatus == model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the write to be skipped, but management_status was changed to Enabled")
	}
}

func TestCommitModelCandidateBasicTestResult(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, _, candidate := seedModelWithCandidate(t, db, "smart", "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	outcome := 0

	if err := CommitModelCandidateBasicTestResult(db, candidate.ID, model.ModelVerificationStatusPassed, &outcome, 42, now); err != nil {
		t.Fatalf("CommitModelCandidateBasicTestResult failed: %v", err)
	}
	reloaded, err := FindModelCandidateByID(db, candidate.ID)
	if err != nil {
		t.Fatalf("FindModelCandidateByID failed: %v", err)
	}
	if reloaded.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification_status=passed, got %d", reloaded.VerificationStatus)
	}
	if reloaded.LastTestResult == nil || *reloaded.LastTestResult != 0 {
		t.Fatalf("expected last_test_result=0, got %+v", reloaded.LastTestResult)
	}
	if reloaded.LastTestDurationMs == nil || *reloaded.LastTestDurationMs != 42 {
		t.Fatalf("expected last_test_duration_ms=42, got %+v", reloaded.LastTestDurationMs)
	}
}

// TestCommitModelCandidateCapabilityTestResultDoesNotTouchVerificationStatus
// is the direct regression test for design doc §3's rule: streaming and
// function-calling test results are independent of the basic-text
// verification gate.
func TestCommitModelCandidateCapabilityTestResultDoesNotTouchVerificationStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, _, candidate := seedModelWithCandidate(t, db, "smart", "provider-a")
	now := time.Now().UTC().Truncate(time.Second)
	outcome := 0

	if err := CommitModelCandidateCapabilityTestResult(db, candidate.ID, "streaming", true, &outcome, 10, now); err != nil {
		t.Fatalf("CommitModelCandidateCapabilityTestResult(streaming) failed: %v", err)
	}
	reloaded, err := FindModelCandidateByID(db, candidate.ID)
	if err != nil {
		t.Fatalf("FindModelCandidateByID failed: %v", err)
	}
	if !reloaded.SupportsStreaming {
		t.Fatalf("expected supports_streaming=true")
	}
	if reloaded.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected verification_status to remain untested, got %d", reloaded.VerificationStatus)
	}

	if err := CommitModelCandidateCapabilityTestResult(db, candidate.ID, "function_calling", true, &outcome, 15, now); err != nil {
		t.Fatalf("CommitModelCandidateCapabilityTestResult(function_calling) failed: %v", err)
	}
	reloaded, err = FindModelCandidateByID(db, candidate.ID)
	if err != nil {
		t.Fatalf("FindModelCandidateByID failed: %v", err)
	}
	if !reloaded.SupportsFunctionCalling {
		t.Fatalf("expected supports_function_calling=true")
	}
	if !reloaded.SupportsStreaming {
		t.Fatalf("expected supports_streaming to remain true after a separate function_calling test")
	}
}

func TestSwapModelCandidateSortOrderSwapsNeighbors(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m, _, first := seedModelWithCandidate(t, db, "smart", "provider-a")
	now := time.Now().UTC().Truncate(time.Second)

	secondProvider, _ := seedProviderWithKey(t, db, "provider-b")
	second := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: secondProvider.ID, ProviderModelName: "gpt-4o", SortOrder: 2,
		ManagementStatus: model.ModelCandidateStatusDisabled, CreatedAt: now, UpdatedAt: now,
	}
	if err := CreateModelCandidate(db, second); err != nil {
		t.Fatalf("CreateModelCandidate failed: %v", err)
	}

	applied, err := SwapModelCandidateSortOrder(db, m.ID, second.ID, "up")
	if err != nil {
		t.Fatalf("SwapModelCandidateSortOrder failed: %v", err)
	}
	if !applied {
		t.Fatalf("expected applied=true")
	}

	reloadedFirst, err := FindModelCandidateByID(db, first.ID)
	if err != nil {
		t.Fatalf("FindModelCandidateByID failed: %v", err)
	}
	if reloadedFirst.SortOrder != 2 {
		t.Fatalf("expected first candidate to now have sort_order=2, got %d", reloadedFirst.SortOrder)
	}
	reloadedSecond, err := FindModelCandidateByID(db, second.ID)
	if err != nil {
		t.Fatalf("FindModelCandidateByID failed: %v", err)
	}
	if reloadedSecond.SortOrder != 1 {
		t.Fatalf("expected second candidate to now have sort_order=1, got %d", reloadedSecond.SortOrder)
	}
}

func TestSwapModelCandidateSortOrderNoopAtBoundary(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m, _, first := seedModelWithCandidate(t, db, "smart", "provider-a")

	applied, err := SwapModelCandidateSortOrder(db, m.ID, first.ID, "up")
	if err != nil {
		t.Fatalf("SwapModelCandidateSortOrder failed: %v", err)
	}
	if applied {
		t.Fatalf("expected applied=false at the top boundary")
	}
}

func TestSwapModelCandidateSortOrderReturnsErrorForUnknownCandidate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m, _, _ := seedModelWithCandidate(t, db, "smart", "provider-a")

	_, err := SwapModelCandidateSortOrder(db, m.ID, 999999, "up")
	if err == nil {
		t.Fatalf("expected an error for an unknown candidate ID")
	}
}

func TestFindModelByIDReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.CloseDB(t, db)
	if _, err := FindModelByID(db, 1); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestFindModelByNameReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.CloseDB(t, db)
	if _, err := FindModelByName(db, "smart"); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestListModelsReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.CloseDB(t, db)
	if _, err := ListModels(db); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestListModelCandidatesByModelIDReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.CloseDB(t, db)
	if _, err := ListModelCandidatesByModelID(db, 1); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestListModelCandidatesByModelIDsReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.CloseDB(t, db)
	if _, err := ListModelCandidatesByModelIDs(db, []uint{1}); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestNextCandidateSortOrderReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.CloseDB(t, db)
	if _, err := NextCandidateSortOrder(db, 1); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestSetModelCandidateManagementStatusIfVerifiedReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	testutil.CloseDB(t, db)
	if _, err := SetModelCandidateManagementStatusIfVerified(db, 1, model.ModelCandidateStatusEnabled, time.Now().UTC()); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestSwapModelCandidateSortOrderReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m, _, first := seedModelWithCandidate(t, db, "smart", "provider-a")
	testutil.CloseDB(t, db)

	if _, err := SwapModelCandidateSortOrder(db, m.ID, first.ID, "up"); err == nil {
		t.Fatalf("expected an error once the underlying connection is closed")
	}
}

func TestDeleteModelCandidate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, _, candidate := seedModelWithCandidate(t, db, "smart", "provider-a")

	if err := DeleteModelCandidate(db, candidate.ID); err != nil {
		t.Fatalf("DeleteModelCandidate failed: %v", err)
	}
	if _, err := FindModelCandidateByID(db, candidate.ID); err == nil {
		t.Fatalf("expected the candidate to be gone after deletion")
	}
}
