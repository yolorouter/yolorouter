package videotask

// The settlement and budget battery: what a priced submit reserves, what
// a completed task charges, what a failed one does not, and the
// once-only guarantees both rest on.

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// pricedFixture is one model whose candidate bills per second at a
// declared tier, plus the key that submits to it.
type pricedFixture struct {
	db        *gorm.DB
	svc       *Service
	apiKey    *model.APIKey
	candidate *model.ModelCandidate
}

func newPricedFixture(t *testing.T, sellPrice float64, limitMicros *int64) *pricedFixture {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	provider := &model.Provider{Name: "video-priced-provider", ProviderType: "openai", BaseURL: "https://up.test", DestinationVersion: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(provider).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	m := &model.Model{Name: "video-priced", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	tiers, err := model.MarshalVideoPricingTiers(&model.VideoPricingTiers{Tiers: []model.VideoPricingTier{
		{Resolution: "720P", PurchasePrice: 0, SellPrice: sellPrice},
		{Resolution: "1080P", PurchasePrice: 0, SellPrice: sellPrice * 2},
	}})
	if err != nil {
		t.Fatalf("marshal tiers: %v", err)
	}
	cand := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: provider.ID, ProviderModelName: "wan2.7-t2v",
		BillingMode: model.BillingModeVideo, VideoPricingTiers: tiers,
		ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: 1,
		VerificationStatus: model.ModelVerificationStatusPassed, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(cand).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	key := &model.APIKey{
		KeyHash: "hash-video-priced", KeyPrefix: "sk-yr-video-priced", Status: model.APIKeyStatusActive,
		BudgetLimitMicros: limitMicros, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(key).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return &pricedFixture{db: db, svc: NewService(db, nil), apiKey: key, candidate: cand}
}

func (f *pricedFixture) task(seconds int) *model.VideoTask {
	return &model.VideoTask{
		APIKeyID: f.apiKey.ID, ModelID: f.candidate.ModelID, ModelName: "video-priced",
		CandidateID: f.candidate.ID, ProviderID: f.candidate.ProviderID, ProviderModelName: "wan2.7-t2v",
		ProviderTaskID: "up-1", DestinationVersion: 1,
		Size: "720x1280", Seconds: seconds,
	}
}

func (f *pricedFixture) spent(t *testing.T) int64 {
	t.Helper()
	var key model.APIKey
	if err := f.db.Where("id = ?", f.apiKey.ID).First(&key).Error; err != nil {
		t.Fatalf("reload key: %v", err)
	}
	return key.BudgetSpentMicros
}

func TestSettlementChargesObservedSecondsOnce(t *testing.T) {
	f := newPricedFixture(t, 0.5, nil) // 0.5 yuan/s at 720P
	q := &stubQuerier{answers: []QueryResult{
		{Status: model.VideoTaskCompleted, ResultURL: "https://v", UsageSeconds: 8},
	}}
	f.svc.querier = q
	now := time.Now()
	task := f.task(8)
	if err := f.svc.Create(context.Background(), task, now); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The submit-time bound: 8s × 0.5 yuan = 4,000,000 micros.
	if task.EstimatedMicros != 4_000_000 {
		t.Fatalf("estimated = %d, want 4000000", task.EstimatedMicros)
	}

	got, err := f.svc.Get(context.Background(), f.apiKey.ID, task.ID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !got.Billed || got.BilledMicros != 4_000_000 {
		t.Fatalf("settlement must charge the observed seconds once, got billed=%v micros=%d", got.Billed, got.BilledMicros)
	}
	if f.spent(t) != 4_000_000 {
		t.Fatalf("the owning key's spend must rise by the charge, got %d", f.spent(t))
	}

	// A second poll of the same completed task settles nothing further.
	if _, err := f.svc.Get(context.Background(), f.apiKey.ID, task.ID, now.Add(2*time.Second)); err != nil {
		t.Fatalf("repoll: %v", err)
	}
	if f.spent(t) != 4_000_000 {
		t.Fatalf("double poll must not double charge, spend=%d", f.spent(t))
	}
}

func TestSettlementUsesSubmitTimeSnapshotPrice(t *testing.T) {
	f := newPricedFixture(t, 0.5, nil)
	q := &stubQuerier{answers: []QueryResult{{Status: model.VideoTaskCompleted, UsageSeconds: 4}}}
	f.svc.querier = q
	now := time.Now()
	task := f.task(4)
	_ = f.svc.Create(context.Background(), task, now)

	// The tier price doubles after submit: the snapshot was taken at
	// creation, so the bill must not follow the edit.
	doubled, _ := model.MarshalVideoPricingTiers(&model.VideoPricingTiers{Tiers: []model.VideoPricingTier{
		{Resolution: "720P", SellPrice: 9.9},
	}})
	if err := f.db.Model(&model.ModelCandidate{}).Where("id = ?", f.candidate.ID).Update("video_pricing_tiers", doubled).Error; err != nil {
		t.Fatalf("retier: %v", err)
	}

	got, _ := f.svc.Get(context.Background(), f.apiKey.ID, task.ID, now.Add(time.Second))
	if !got.Billed || got.BilledMicros != 2_000_000 {
		t.Fatalf("an in-flight task bills its submit-time price, got %+v", got.BilledMicros)
	}
	// And a task created AFTER the edit bills the new one.
	q2 := &stubQuerier{answers: []QueryResult{{Status: model.VideoTaskCompleted, UsageSeconds: 4}}}
	f.svc.querier = q2
	later := f.task(4)
	later.ProviderTaskID = "up-2"
	_ = f.svc.Create(context.Background(), later, now.Add(time.Minute))
	got, _ = f.svc.Get(context.Background(), f.apiKey.ID, later.ID, now.Add(61*time.Second))
	if !got.Billed || got.BilledMicros != 39_600_000 {
		t.Fatalf("a new task bills the edited price, got %d", got.BilledMicros)
	}
}

func TestFailuresBillNothing(t *testing.T) {
	for _, terminal := range []string{model.VideoTaskFailed, model.VideoTaskCancelled, model.VideoTaskExpired} {
		t.Run(terminal, func(t *testing.T) {
			f := newPricedFixture(t, 0.5, nil)
			f.svc.querier = &stubQuerier{answers: []QueryResult{{Status: terminal, ErrorCode: "x"}}}
			now := time.Now()
			task := f.task(8)
			_ = f.svc.Create(context.Background(), task, now)
			got, err := f.svc.Get(context.Background(), f.apiKey.ID, task.ID, now.Add(time.Second))
			if err != nil {
				t.Fatalf("poll: %v", err)
			}
			if got.Billed || got.BilledMicros != 0 {
				t.Fatalf("a %s task must bill nothing, got billed=%v micros=%d", terminal, got.Billed, got.BilledMicros)
			}
			if f.spent(t) != 0 {
				t.Fatalf("spend must be untouched, got %d", f.spent(t))
			}
		})
	}
}

func TestBudgetGateRejectsAndReleases(t *testing.T) {
	limit := int64(5_000_000) // enough for one 8s task at 0.5, not two
	f := newPricedFixture(t, 0.5, &limit)
	now := time.Now()

	first := f.task(8)
	if err := f.svc.Create(context.Background(), first, now); err != nil {
		t.Fatalf("first task must fit the budget: %v", err)
	}
	second := f.task(8)
	second.ProviderTaskID = "up-2"
	err := f.svc.Create(context.Background(), second, now)
	var budget *BudgetExceededError
	if !errors.As(err, &budget) {
		t.Fatalf("second task must exceed the budget, got %v", err)
	}
	if budget.InFlight != 4_000_000 || budget.Ask != 4_000_000 {
		t.Fatalf("the refusal must carry the numbers, got %+v", budget)
	}

	// The first task fails: its reservation releases, and the second now
	// fits.
	f.svc.querier = &stubQuerier{answers: []QueryResult{{Status: model.VideoTaskFailed}}}
	if _, err := f.svc.Get(context.Background(), f.apiKey.ID, first.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("poll first: %v", err)
	}
	third := f.task(8)
	third.ProviderTaskID = "up-3"
	if err := f.svc.Create(context.Background(), third, now.Add(2*time.Second)); err != nil {
		t.Fatalf("a failed task's reservation must release: %v", err)
	}
}

func TestUnpricedTasksSkipTheBudgetGate(t *testing.T) {
	zero := int64(1)
	f := newPricedFixture(t, 0.5, &zero)
	// A candidate with no video pricing bills nothing and reserves
	// nothing — the lenient reading, same as the image settlement.
	f.db.Model(&model.ModelCandidate{}).Where("id = ?", f.candidate.ID).
		Updates(map[string]any{"billing_mode": model.BillingModeToken, "video_pricing_tiers": ""})
	now := time.Now()
	task := f.task(8)
	if err := f.svc.Create(context.Background(), task, now); err != nil {
		t.Fatalf("an unpriced task must pass a tiny budget: %v", err)
	}
	if task.EstimatedMicros != 0 {
		t.Fatalf("unpriced means no reservation, got %d", task.EstimatedMicros)
	}
}

func TestCompletionBackstopSettlesUnbilledRows(t *testing.T) {
	// The crash window: completion recorded, settlement never landed.
	// Reading the task again settles it — exactly once, because the CAS
	// in the store, not the read, decides.
	f := newPricedFixture(t, 0.5, nil)
	now := time.Now()
	task := f.task(4)
	_ = f.svc.Create(context.Background(), task, now)
	if err := f.db.Model(&model.VideoTask{}).Where("id = ?", task.ID).Updates(map[string]any{
		"status": model.VideoTaskCompleted, "usage_seconds": 4, "result_url": "https://v",
	}).Error; err != nil {
		t.Fatalf("forge completion: %v", err)
	}
	got, err := f.svc.Get(context.Background(), f.apiKey.ID, task.ID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Billed || got.BilledMicros != 2_000_000 || f.spent(t) != 2_000_000 {
		t.Fatalf("the backstop must settle the row, got billed=%v micros=%d spent=%d", got.Billed, got.BilledMicros, f.spent(t))
	}
}

func TestSweepSettlesUnbilledCompletedRows(t *testing.T) {
	// The reconciliation half: a task that completed while nobody was
	// watching — crashed between observation and charge, or simply never
	// polled again — is settled by the reaper's tick, not left unbilled.
	f := newPricedFixture(t, 0.5, nil)
	now := time.Now()
	task := f.task(8)
	_ = f.svc.Create(context.Background(), task, now)
	if err := f.db.Model(&model.VideoTask{}).Where("id = ?", task.ID).Updates(map[string]any{
		"status": model.VideoTaskCompleted, "usage_seconds": 8, "result_url": "https://v",
	}).Error; err != nil {
		t.Fatalf("forge completion: %v", err)
	}
	if _, err := f.svc.SweepExpired(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if f.spent(t) != 4_000_000 {
		t.Fatalf("the sweep must settle the row, spent=%d", f.spent(t))
	}
	// And the sweep settles it once: a second tick charges nothing more.
	if _, err := f.svc.SweepExpired(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if f.spent(t) != 4_000_000 {
		t.Fatalf("repeated sweeps must not recharge, spent=%d", f.spent(t))
	}
}

func TestBudgetReservesForUnsettledCompletions(t *testing.T) {
	// A completed-but-unsettled task's charge is coming; the reservation
	// must hold its bound until the charge lands, or the window between
	// observation and settle is a hole the next submit slips through.
	limit := int64(5_000_000)
	f := newPricedFixture(t, 0.5, &limit)
	now := time.Now()
	first := f.task(8)
	_ = f.svc.Create(context.Background(), first, now)
	if err := f.db.Model(&model.VideoTask{}).Where("id = ?", first.ID).Updates(map[string]any{
		"status": model.VideoTaskCompleted, "usage_seconds": 8, "billed": false,
	}).Error; err != nil {
		t.Fatalf("forge unsettled completion: %v", err)
	}
	second := f.task(4)
	second.ProviderTaskID = "up-2"
	err := f.svc.Create(context.Background(), second, now.Add(time.Second))
	var budget *BudgetExceededError
	if !errors.As(err, &budget) {
		t.Fatalf("the unsettled completion must still reserve, got %v", err)
	}
	if budget.InFlight != 4_000_000 {
		t.Fatalf("reservation must count the unbilled completion, got %+v", budget)
	}
}
