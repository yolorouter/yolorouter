package modeladmin_test

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/apikey"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// countRows is the deletion suite's assertion helper: a plain scoped count,
// deliberately not going through the repository under test, so a bug in the
// repository's own predicates cannot make the cascade verify itself.
func countRows(t *testing.T, db *gorm.DB, table, where string, args ...interface{}) int64 {
	t.Helper()
	var n int64
	if err := db.Table(table).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// seedModelCandidate inserts one candidate row directly. UNIQUE(model_id,
// provider_id) means a model's N candidates need N distinct providers;
// sort_order appends after whatever the model already has, satisfying
// UNIQUE(model_id, sort_order) without the caller counting rows.
func seedModelCandidate(t *testing.T, db *gorm.DB, modelID, providerID uint, status int) {
	t.Helper()
	now := time.Now().UTC()
	var existing int64
	if err := db.Table("model_candidates").Where("model_id = ?", modelID).Count(&existing).Error; err != nil {
		t.Fatalf("count candidates for sort order: %v", err)
	}
	c := &model.ModelCandidate{
		ModelID: modelID, ProviderID: providerID, ProviderModelName: "upstream-name",
		ManagementStatus: status, SortOrder: int(existing) + 1,
		VerificationStatus: model.ModelVerificationStatusUntested,
		CreatedAt:          now, UpdatedAt: now, PriceUpdatedAt: now,
	}
	if err := db.Create(c).Error; err != nil {
		t.Fatalf("seed candidate (model %d, provider %d): %v", modelID, providerID, err)
	}
}

// setVisionFallbackForTest CAS-writes the vision-fallback pair from whatever
// the seeded state is and returns the committed version, so tests never
// assume a seed version.
func setVisionFallbackForTest(t *testing.T, db *gorm.DB, name, prompt string) int64 {
	t.Helper()
	_, version, err := repository.GetVisionFallback(db)
	if err != nil {
		t.Fatalf("read vision fallback baseline: %v", err)
	}
	_, committed, err := repository.UpdateVisionFallback(db, version, name, prompt)
	if err != nil {
		t.Fatalf("set vision fallback to %q: %v", name, err)
	}
	return committed
}

// The cascade four-piece: the models row goes, every candidate goes, every
// key-allowlist reference goes, and a vision-fallback setting naming the
// model is cleared — all while request history stays behind under the model
// name and everything owned by OTHER models is untouched. The model is
// deleted while still enabled: there is no disable-first precondition.
func TestDeleteModelCascadesConfigAndKeepsHistory(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	if view.ManagementStatus != model.ModelStatusEnabled {
		t.Fatalf("fixture expects an enabled model, got status %d", view.ManagementStatus)
	}
	other, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "other"}, now)
	if err != nil {
		t.Fatalf("CreateModel(other) failed: %v", err)
	}

	providerA := testutil.SeedProvider(t, db, "provider-a")
	providerB := testutil.SeedProvider(t, db, "provider-b")
	seedModelCandidate(t, db, view.ID, providerA, model.ModelCandidateStatusEnabled)
	// A disabled candidate cascades exactly like an enabled one.
	seedModelCandidate(t, db, view.ID, providerB, model.ModelCandidateStatusDisabled)
	seedModelCandidate(t, db, other.ID, providerA, model.ModelCandidateStatusEnabled)

	keySvc := apikey.NewAPIKeyService(db, testutil.ProviderSecrets())
	if _, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{view.ID}}, now); err != nil {
		t.Fatalf("CreateAPIKey(smart) failed: %v", err)
	}
	if _, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{other.ID}}, now); err != nil {
		t.Fatalf("CreateAPIKey(other) failed: %v", err)
	}

	versionAfterSet := setVisionFallbackForTest(t, db, "smart", "describe the image")

	testutil.SeedRequestLog(t, db, "req-del", now, func(r *model.RequestLog) { r.ModelName = "smart" })
	testutil.SeedRequestLog(t, db, "req-keep", now, func(r *model.RequestLog) { r.ModelName = "other" })

	if err := svc.DeleteModel(view.ID); err != nil {
		t.Fatalf("DeleteModel failed: %v", err)
	}

	if n := countRows(t, db, "models", "id = ?", view.ID); n != 0 {
		t.Fatalf("models row still present after delete")
	}
	if n := countRows(t, db, "model_candidates", "model_id = ?", view.ID); n != 0 {
		t.Fatalf("expected cascade to remove candidates, %d remain", n)
	}
	if n := countRows(t, db, "api_key_models", "model_id = ?", view.ID); n != 0 {
		t.Fatalf("expected cascade to remove key allowlist rows, %d remain", n)
	}

	// The vision-fallback model reference is cleared in the same delete; the
	// prompt survives (it describes the job, not the model), and the pair's
	// shared version advances so CAS writers and cache refreshes see it.
	vf, versionAfterDelete, err := repository.GetVisionFallback(db)
	if err != nil {
		t.Fatalf("read vision fallback after delete: %v", err)
	}
	if vf.Model != "" {
		t.Fatalf("vision fallback model = %q, want cleared", vf.Model)
	}
	if vf.Prompt != "describe the image" {
		t.Fatalf("vision fallback prompt = %q, want it preserved", vf.Prompt)
	}
	if versionAfterDelete != versionAfterSet+1 {
		t.Fatalf("vision fallback version = %d, want %d (advanced past the fixture's set)", versionAfterDelete, versionAfterSet+1)
	}

	// History survives under the name; other models lose nothing.
	if n := countRows(t, db, "request_logs", "model_name = ?", "smart"); n != 1 {
		t.Fatalf("expected the request log to survive the delete, found %d", n)
	}
	if n := countRows(t, db, "models", "id = ?", other.ID); n != 1 {
		t.Fatalf("other model vanished: %d rows", n)
	}
	if n := countRows(t, db, "model_candidates", "model_id = ?", other.ID); n != 1 {
		t.Fatalf("other model's candidate vanished: %d rows", n)
	}
	if n := countRows(t, db, "api_key_models", "model_id = ?", other.ID); n != 1 {
		t.Fatalf("other model's allowlist row vanished: %d rows", n)
	}
	if n := countRows(t, db, "request_logs", "model_name = ?", "other"); n != 1 {
		t.Fatalf("other model's history vanished: %d rows", n)
	}
}

// A delete that is NOT the configured vision-fallback model must leave the
// setting byte-for-byte alone — value and version both.
func TestDeleteModelKeepsVisionFallbackWhenAnotherModelConfigured(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	smart, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	versionAfterSet := setVisionFallbackForTest(t, db, "watcher", "describe the image")

	if err := svc.DeleteModel(smart.ID); err != nil {
		t.Fatalf("DeleteModel failed: %v", err)
	}
	vf, version, err := repository.GetVisionFallback(db)
	if err != nil {
		t.Fatalf("read vision fallback: %v", err)
	}
	if vf.Model != "watcher" {
		t.Fatalf("vision fallback model = %q, want watcher untouched", vf.Model)
	}
	if vf.Prompt != "describe the image" {
		t.Fatalf("vision fallback prompt = %q, want untouched", vf.Prompt)
	}
	if version != versionAfterSet {
		t.Fatalf("vision fallback version = %d, want %d (untouched)", version, versionAfterSet)
	}
}

func TestDeleteModelUnknownIDReturnsNotFound(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	if err := svc.DeleteModel(9999); !errors.Is(err, errcode.ErrModelNotFound) {
		t.Fatalf("err = %v, want ErrModelNotFound", err)
	}
	if n := countRows(t, db, "models", "1 = 1"); n != 0 {
		t.Fatalf("a failed delete must remove nothing, found %d models", n)
	}
}

// Deleting frees the unique name, and a same-name recreate is a NEW row that
// the name-keyed history simply continues: the recreate's impact preview
// already sees the old traffic and none of the old configuration.
func TestDeleteModelFreesNameAndRecreateContinuesHistory(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	first, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	provider := testutil.SeedProvider(t, db, "provider-a")
	seedModelCandidate(t, db, first.ID, provider, model.ModelCandidateStatusEnabled)
	testutil.SeedRequestLog(t, db, "req-reborn", now.Add(-time.Hour), func(r *model.RequestLog) { r.ModelName = "smart" })

	if err := svc.DeleteModel(first.ID); err != nil {
		t.Fatalf("DeleteModel failed: %v", err)
	}

	recreated, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("recreate under the freed name failed: %v", err)
	}
	if recreated.ID == first.ID {
		t.Fatalf("recreated model reused the deleted id %d", first.ID)
	}
	impact, err := svc.GetModelImpact(recreated.ID, now)
	if err != nil {
		t.Fatalf("GetModelImpact(recreated) failed: %v", err)
	}
	if impact.RecentRequestCount != 1 {
		t.Fatalf("recent request count = %d, want 1 (history continues under the name)", impact.RecentRequestCount)
	}
	if impact.CandidateCount != 0 {
		t.Fatalf("candidate count = %d, want 0 (the recreate is a fresh shell)", impact.CandidateCount)
	}
}

// The root anchor: the foreign keys on model_candidates and api_key_models
// are what make the cascade's steps load-bearing. With either cleanup step
// skipped, the models DELETE itself is rejected by the database and the
// whole transaction rolls back — removing a step from DeleteModelCascade can
// only turn every delete into an error, never leave dangling configuration.
func TestDeleteModelStepsAreEnforcedByForeignKeys(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	provider := testutil.SeedProvider(t, db, "provider-a")
	seedModelCandidate(t, db, view.ID, provider, model.ModelCandidateStatusEnabled)
	keySvc := apikey.NewAPIKeyService(db, testutil.ProviderSecrets())
	if _, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: testutil.SeedKeyOwner(t, db), ModelIDs: []uint{view.ID}}, now); err != nil {
		t.Fatalf("CreateAPIKey failed: %v", err)
	}

	// Skip the candidates step: the allowlist cleanup and row delete alone
	// must be rejected.
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("model_id = ?", view.ID).Delete(&model.APIKeyModel{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", view.ID).Delete(&model.Model{}).Error
	})
	if err == nil {
		t.Fatalf("models DELETE with candidates remaining was accepted; the candidates foreign key is not enforcing")
	}

	// Skip the allowlist step: the candidates cleanup and row delete alone
	// must be rejected likewise.
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("model_id = ?", view.ID).Delete(&model.ModelCandidate{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", view.ID).Delete(&model.Model{}).Error
	})
	if err == nil {
		t.Fatalf("models DELETE with allowlist rows remaining was accepted; the api_key_models foreign key is not enforcing")
	}

	// Both rejected transactions rolled back completely.
	if n := countRows(t, db, "models", "id = ?", view.ID); n != 1 {
		t.Fatalf("model row did not survive the rejected deletes: %d rows", n)
	}
	if n := countRows(t, db, "model_candidates", "model_id = ?", view.ID); n != 1 {
		t.Fatalf("candidates did not survive the rejected deletes: %d rows", n)
	}
	if n := countRows(t, db, "api_key_models", "model_id = ?", view.ID); n != 1 {
		t.Fatalf("allowlist rows did not survive the rejected deletes: %d rows", n)
	}

	// The real cascade still succeeds afterwards.
	if err := svc.DeleteModel(view.ID); err != nil {
		t.Fatalf("DeleteModel after the anchor probes failed: %v", err)
	}
}

// The impact preview's candidate figure counts every candidate row, disabled
// ones included — the cascade removes them all alike, so a smaller figure
// would understate the delete — and only the named model's rows.
func TestModelImpactCountsAllCandidatesIncludingDisabled(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "smart"}, now)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}
	other, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "other"}, now)
	if err != nil {
		t.Fatalf("CreateModel(other) failed: %v", err)
	}
	providerA := testutil.SeedProvider(t, db, "provider-a")
	providerB := testutil.SeedProvider(t, db, "provider-b")
	providerC := testutil.SeedProvider(t, db, "provider-c")
	seedModelCandidate(t, db, view.ID, providerA, model.ModelCandidateStatusEnabled)
	seedModelCandidate(t, db, view.ID, providerB, model.ModelCandidateStatusDisabled)
	seedModelCandidate(t, db, view.ID, providerC, model.ModelCandidateStatusDisabled)
	seedModelCandidate(t, db, other.ID, providerA, model.ModelCandidateStatusEnabled)

	impact, err := svc.GetModelImpact(view.ID, now)
	if err != nil {
		t.Fatalf("GetModelImpact failed: %v", err)
	}
	if impact.CandidateCount != 3 {
		t.Fatalf("candidate count = %d, want 3 (every candidate row, disabled included)", impact.CandidateCount)
	}
	// The pre-existing fields keep their meanings on the same response.
	if impact.AllowAllKeyCount != 0 {
		t.Fatalf("allow-all count = %d, want 0", impact.AllowAllKeyCount)
	}
	if len(impact.AllowlistedKeys) != 0 {
		t.Fatalf("allowlisted keys = %+v, want none", impact.AllowlistedKeys)
	}
	if impact.RecentWindowDays != 7 {
		t.Fatalf("window days = %d, want 7", impact.RecentWindowDays)
	}
}
