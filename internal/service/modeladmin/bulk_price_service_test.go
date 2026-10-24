package modeladmin_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/pricecatalog"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedBulkPriceModel creates one empty external model row and returns its id.
func seedBulkPriceModel(t *testing.T, svc *modeladmin.ModelService, name string, at time.Time) uint {
	t.Helper()
	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: name}, at)
	if err != nil {
		t.Fatalf("seed model %s: %v", name, err)
	}
	return view.ID
}

// seedBulkPriceStandaloneRow creates a fresh model row and one candidate of
// the given upstream name on it — the shape most scale tests need, since a
// model maps each provider at most once (UNIQUE(model_id, provider_id)).
func seedBulkPriceStandaloneRow(t *testing.T, svc *modeladmin.ModelService, providerID uint, modelName, upstream string, at time.Time, mut func(*modeladmin.CreateCandidateInput)) uint {
	t.Helper()
	m := seedBulkPriceModel(t, svc, modelName, at)
	return seedBulkPriceCandidate(t, svc, providerID, m, upstream, at, mut)
}

// seedBulkPriceCandidate stores one candidate through the regular create path
// (billing declaration resolution and prices_declared derivation included).
// The mut hook adjusts the input per test; a nil hook stores an unpriced
// token-mode row.
func seedBulkPriceCandidate(t *testing.T, svc *modeladmin.ModelService, providerID, modelID uint, name string, at time.Time, mut func(*modeladmin.CreateCandidateInput)) uint {
	t.Helper()
	in := modeladmin.CreateCandidateInput{ProviderID: providerID, ProviderModelName: name}
	if mut != nil {
		mut(&in)
	}
	view, err := svc.CreateModelCandidate(context.Background(), modelID, in, at)
	if err != nil {
		t.Fatalf("seed candidate %s: %v", name, err)
	}
	return view.ID
}

func bulkPriceTokenPrices(in, out float64, cacheWrite, cacheRead *float64) func(*modeladmin.CreateCandidateInput) {
	return func(input *modeladmin.CreateCandidateInput) {
		input.InputPrice = ptrFloat(in)
		input.OutputPrice = ptrFloat(out)
		input.CacheWritePrice = cacheWrite
		input.CacheReadPrice = cacheRead
	}
}

// loadBulkPriceCandidate re-reads one candidate row for write assertions.
func loadBulkPriceCandidate(t *testing.T, db *gorm.DB, id uint) model.ModelCandidate {
	t.Helper()
	var c model.ModelCandidate
	if err := db.First(&c, id).Error; err != nil {
		t.Fatalf("reload candidate %d: %v", id, err)
	}
	return c
}

// snapshotBulkPriceCandidates reads every candidate row ordered by id, the
// before/after snapshot the zero-write assertions compare.
func snapshotBulkPriceCandidates(t *testing.T, db *gorm.DB) []model.ModelCandidate {
	t.Helper()
	var rows []model.ModelCandidate
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("snapshot candidates: %v", err)
	}
	return rows
}

func TestBulkPriceScaleTokenRowScalesAllFourSlotsKeepsNullsAndStrayAudio(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	m := seedBulkPriceModel(t, svc, "scale-token", now)
	cw := 0.5
	id := seedBulkPriceCandidate(t, svc, p.ID, m, "gpt-x", now, func(in *modeladmin.CreateCandidateInput) {
		bulkPriceTokenPrices(2, 8, &cw, nil)(in)
		// A residue from an earlier audio declaration: the token-mode scale
		// must not touch it.
		in.AudioUnitPrice = ptrFloat(7)
	})
	clockBefore := loadBulkPriceCandidate(t, db, id).PriceUpdatedAt

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{id}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(1.5), DryRun: ptrBool(false),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	if result.Applied != 1 || result.Skipped != 0 || len(result.Items) != 1 {
		t.Fatalf("expected 1 applied row, got %+v", result)
	}
	item := result.Items[0]
	if !item.Changed || item.SkipReason != "" {
		t.Fatalf("expected a changed row without skip reason, got %+v", item)
	}
	if item.Before.InputPrice != 2 || item.Before.OutputPrice != 8 || item.Before.CacheWritePrice == nil || *item.Before.CacheWritePrice != 0.5 || item.Before.CacheReadPrice != nil {
		t.Fatalf("before slots must mirror the stored row, got %+v", item.Before)
	}
	if item.After.InputPrice != 3 || item.After.OutputPrice != 12 {
		t.Fatalf("token slots must scale, got %+v", item.After)
	}
	if item.After.CacheWritePrice == nil || *item.After.CacheWritePrice != 0.75 {
		t.Fatalf("cache write price must scale, got %+v", item.After)
	}
	// A null cache slot stays null.
	if item.After.CacheReadPrice != nil {
		t.Fatalf("cache read null must stay null, got %+v", item.After)
	}
	after := loadBulkPriceCandidate(t, db, id)
	if after.InputPrice != 3 || after.OutputPrice != 12 || after.CacheWritePrice == nil || *after.CacheWritePrice != 0.75 || after.CacheReadPrice != nil {
		t.Fatalf("stored row must carry the scaled slots, got %+v", after)
	}
	if after.AudioUnitPrice == nil || *after.AudioUnitPrice != 7 {
		t.Fatalf("stray audio_unit_price must stay untouched, got %+v", after.AudioUnitPrice)
	}
	if !after.PriceUpdatedAt.After(clockBefore) {
		t.Fatalf("a real change must advance price_updated_at (%v -> %v)", clockBefore, after.PriceUpdatedAt)
	}
	if !after.PricesDeclared {
		t.Fatalf("a scaled priced row stays declared")
	}
}

func TestBulkPriceScaleAudioRowScalesOnlyCharPrice(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	m := seedBulkPriceModel(t, svc, "scale-audio", now)
	id := seedBulkPriceCandidate(t, svc, p.ID, m, "tts-x", now, func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeAudio
		in.AudioUnitPrice = ptrFloat(3)
	})

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{id}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(2), DryRun: ptrBool(false),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	item := result.Items[0]
	if !item.Changed {
		t.Fatalf("expected the audio row to change, got %+v", item)
	}
	if item.After.AudioUnitPrice == nil || *item.After.AudioUnitPrice != 6 {
		t.Fatalf("char price must scale to 6, got %+v", item.After)
	}
	if item.After.InputPrice != 0 || item.After.OutputPrice != 0 || item.After.CacheWritePrice != nil || item.After.CacheReadPrice != nil {
		t.Fatalf("audio rows write the char slot only, got %+v", item.After)
	}
	after := loadBulkPriceCandidate(t, db, id)
	if after.AudioUnitPrice == nil || *after.AudioUnitPrice != 6 {
		t.Fatalf("stored char price must be 6, got %+v", after.AudioUnitPrice)
	}
	if after.InputPrice != 0 || after.OutputPrice != 0 {
		t.Fatalf("token slots of an audio row must stay untouched, got %+v", after)
	}
	if !after.PricesDeclared {
		t.Fatalf("audio row keeps its declared fact")
	}
}

func TestBulkPriceScaleAfterIsRawFloatProduct(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	id := seedBulkPriceStandaloneRow(t, svc, p.ID, "scale-raw", "raw-x", now, bulkPriceTokenPrices(0.1, 0.2, nil, nil))

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{id}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(3), DryRun: ptrBool(false),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	// The raw float64 product, no semantic rounding. Runtime multiplication,
	// not constant folding: 0.1*3 as an expression is exact at compile time,
	// which is precisely the rounding this assertion must not use.
	in, out, factor := 0.1, 0.2, 3.0
	if result.Items[0].After.InputPrice != in*factor || result.Items[0].After.OutputPrice != out*factor {
		t.Fatalf("after must be the bare product, got %+v (want %v/%v)", result.Items[0].After, in*factor, out*factor)
	}
}

func TestBulkPriceScaleSkipsUnpricedUnsupportedNotFound(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	unpriced := seedBulkPriceStandaloneRow(t, svc, p.ID, "m-unpriced", "unpriced-x", now, nil)
	imageRow := seedBulkPriceStandaloneRow(t, svc, p.ID, "m-image", "image-x", now, func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeImage
		in.ImagePricingTiers = &model.ImagePricingTiers{Mode: "per_image",
			Tiers: []model.ImagePricingTier{{Quality: "standard", Size: "1024x1024", Price: 0.04}}}
	})
	videoRow := seedBulkPriceStandaloneRow(t, svc, p.ID, "m-video", "video-x", now, func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeVideo
		in.VideoPricingTiers = &model.VideoPricingTiers{
			Tiers: []model.VideoPricingTier{{Resolution: "720p", PurchasePrice: 0.1, SellPrice: 0.2}}}
	})
	audioUnpriced := seedBulkPriceStandaloneRow(t, svc, p.ID, "m-audio-unpriced", "audio-unpriced-x", now, func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeAudio
	})
	before := snapshotBulkPriceCandidates(t, db)

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{unpriced, imageRow, videoRow, audioUnpriced, 99999},
		Operation:    modeladmin.BulkPriceOperationScale, Factor: ptrFloat(2), DryRun: ptrBool(false),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	if result.Applied != 0 || result.Skipped != 5 {
		t.Fatalf("expected 0 applied / 5 skipped, got %+v", result)
	}
	byID := map[uint]modeladmin.BulkPriceItem{}
	for _, item := range result.Items {
		byID[item.CandidateID] = item
	}
	for id, want := range map[uint]string{
		unpriced:      modeladmin.BulkPriceSkipUnpriced,
		imageRow:      modeladmin.BulkPriceSkipUnsupportedBillingMode,
		videoRow:      modeladmin.BulkPriceSkipUnsupportedBillingMode,
		audioUnpriced: modeladmin.BulkPriceSkipUnpriced,
		99999:         modeladmin.BulkPriceSkipNotFound,
	} {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("no result row for candidate %d", id)
		}
		if got.SkipReason != want {
			t.Errorf("candidate %d skip reason = %q, want %q", id, got.SkipReason, want)
		}
		if got.Changed {
			t.Errorf("candidate %d must not report changed", id)
		}
	}
	if after := snapshotBulkPriceCandidates(t, db); !reflect.DeepEqual(before, after) {
		t.Fatalf("all-skipped scale must write nothing")
	}
}

func TestBulkPriceScaleCacheOnlyChangeCountsAsChanged(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	cw := 2.0
	// A declared free row (both token slots explicitly 0) with a cache write
	// price: scaling changes the cache slot only, which is still a change.
	id := seedBulkPriceStandaloneRow(t, svc, p.ID, "m-free-cache", "free-with-cache", now, func(in *modeladmin.CreateCandidateInput) {
		bulkPriceTokenPrices(0, 0, &cw, nil)(in)
	})

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{id}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(2), DryRun: ptrBool(false),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	if result.Applied != 1 || !result.Items[0].Changed {
		t.Fatalf("a cache-only change is still a change, got %+v", result)
	}
	after := loadBulkPriceCandidate(t, db, id)
	if after.CacheWritePrice == nil || *after.CacheWritePrice != 4 {
		t.Fatalf("cache write price must scale to 4, got %+v", after.CacheWritePrice)
	}
	if after.InputPrice != 0 || after.OutputPrice != 0 {
		t.Fatalf("free token slots stay 0, got %v/%v", after.InputPrice, after.OutputPrice)
	}
}

func TestBulkPriceScaleUnchangedCountsIntoSkippedAndKeepsPriceClock(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	id := seedBulkPriceStandaloneRow(t, svc, p.ID, "m-unchanged", "same-x", now, bulkPriceTokenPrices(2, 8, nil, nil))
	clockBefore := loadBulkPriceCandidate(t, db, id).PriceUpdatedAt

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{id}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(1), DryRun: ptrBool(false),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	if result.Applied != 0 || result.Skipped != 1 {
		t.Fatalf("unchanged counts into skipped, got %+v", result)
	}
	if result.Items[0].SkipReason != modeladmin.BulkPriceSkipUnchanged {
		t.Fatalf("skip reason = %q, want unchanged", result.Items[0].SkipReason)
	}
	after := loadBulkPriceCandidate(t, db, id)
	if !after.PriceUpdatedAt.Equal(clockBefore) {
		t.Fatalf("unchanged row must keep its price clock (%v -> %v)", clockBefore, after.PriceUpdatedAt)
	}
}

func TestBulkPriceDedupsRepeatedIDs(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	id := seedBulkPriceStandaloneRow(t, svc, p.ID, "m-dedup", "dup-x", now, bulkPriceTokenPrices(1, 2, nil, nil))

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{id, id, id}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(2), DryRun: ptrBool(false),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("repeated ids collapse to one result row, got %+v", result.Items)
	}
	after := loadBulkPriceCandidate(t, db, id)
	if after.InputPrice != 2 || after.OutputPrice != 4 {
		t.Fatalf("the row must be scaled exactly once, got %v/%v", after.InputPrice, after.OutputPrice)
	}
}

func TestBulkPriceDryRunWritesNothing(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	token := seedBulkPriceStandaloneRow(t, svc, p.ID, "m-dry-token", "tok-x", now, bulkPriceTokenPrices(2, 8, nil, nil))
	audio := seedBulkPriceStandaloneRow(t, svc, p.ID, "m-dry-audio", "aud-x", now, func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeAudio
		in.AudioUnitPrice = ptrFloat(3)
	})
	before := snapshotBulkPriceCandidates(t, db)

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{token, audio}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(2), DryRun: ptrBool(true),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	// would-apply / would-skip counting, with the after slots filled in.
	if result.Applied != 2 || result.Skipped != 0 || len(result.Items) != 2 {
		t.Fatalf("expected 2 would-apply rows, got %+v", result)
	}
	for _, item := range result.Items {
		if !item.Changed || item.SkipReason != "" {
			t.Fatalf("preview rows report changed without skip reason, got %+v", item)
		}
	}
	if result.Items[0].After.InputPrice != 4 || result.Items[0].After.OutputPrice != 16 {
		t.Fatalf("preview after slots must carry the scaled values, got %+v", result.Items[0].After)
	}
	if result.Items[1].After.AudioUnitPrice == nil || *result.Items[1].After.AudioUnitPrice != 6 {
		t.Fatalf("preview after char price must carry 6, got %+v", result.Items[1].After)
	}
	if after := snapshotBulkPriceCandidates(t, db); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry_run must write nothing")
	}
}

func TestBulkPriceApplySuggestedTokenHistoryCopiesCacheAndDeclared(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	base := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	// The priced sibling is the price history for the folded upstream name;
	// the target is a second mapping of the same name under a fresh model.
	histModel := seedBulkPriceModel(t, svc, "hist-a", base)
	cw, cr := 0.5, 0.25
	seedBulkPriceCandidate(t, svc, p.ID, histModel, "shared-x", base, bulkPriceTokenPrices(2, 8, &cw, &cr))
	targetModel := seedBulkPriceModel(t, svc, "target-a", base)
	target := seedBulkPriceCandidate(t, svc, p.ID, targetModel, "shared-x", base.Add(time.Minute), nil)

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{target}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	item := result.Items[0]
	if !item.Changed || item.SkipReason != "" {
		t.Fatalf("the history hit must apply, got %+v", item)
	}
	if item.After.InputPrice != 2 || item.After.OutputPrice != 8 {
		t.Fatalf("after must mirror the history row, got %+v", item.After)
	}
	// Cache prices follow the suggestion source: the history row's values.
	if item.After.CacheWritePrice == nil || *item.After.CacheWritePrice != 0.5 || item.After.CacheReadPrice == nil || *item.After.CacheReadPrice != 0.25 {
		t.Fatalf("cache slots must follow the history row, got %+v", item.After)
	}
	after := loadBulkPriceCandidate(t, db, target)
	if after.InputPrice != 2 || after.OutputPrice != 8 || after.CacheWritePrice == nil || *after.CacheWritePrice != 0.5 {
		t.Fatalf("stored row must carry the suggestion, got %+v", after)
	}
	if !after.PricesDeclared {
		t.Fatalf("a filled unpriced row flips prices_declared")
	}
}

// Both arms of the cache-half of the full-record semantics: a null-cache
// history hit must not degrade its nulls into fabricated zeros, and a PRICED
// target holding cache values of its own must have them OVERWRITTEN by the
// hit's nulls — the row adopts the source's complete record, nulls included,
// the same way the single-row PATCH applies it.
func TestBulkPriceApplySuggestedHistoryCacheNullStaysNull(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	base := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")

	// Arm 1: an unpriced target adopts a null-cache history row as-is.
	histModel := seedBulkPriceModel(t, svc, "hist-b", base)
	seedBulkPriceCandidate(t, svc, p.ID, histModel, "shared-y", base, bulkPriceTokenPrices(1, 4, nil, nil))
	targetModel := seedBulkPriceModel(t, svc, "target-b", base)
	target := seedBulkPriceCandidate(t, svc, p.ID, targetModel, "shared-y", base.Add(time.Minute), nil)

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{target}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	item := result.Items[0]
	// A total break of suggestion resolution (no_suggestion with a zero-valued
	// After) would also carry nil caches — only a changed row without a skip
	// reason proves the history row actually resolved and applied.
	if !item.Changed || item.SkipReason != "" {
		t.Fatalf("the history hit must apply, got %+v", item)
	}
	if item.After.InputPrice != 1 || item.After.OutputPrice != 4 {
		t.Fatalf("after must mirror the history row, got %+v", item.After)
	}
	if item.After.CacheWritePrice != nil || item.After.CacheReadPrice != nil {
		t.Fatalf("a null-cache history row writes null caches, got %+v", item.After)
	}
	after := loadBulkPriceCandidate(t, db, target)
	if after.CacheWritePrice != nil || after.CacheReadPrice != nil {
		t.Fatalf("stored row must carry the null caches, got %v/%v", after.CacheWritePrice, after.CacheReadPrice)
	}

	// Arm 2: a priced target's own cache values lose to the hit's nulls.
	pricedModel := seedBulkPriceModel(t, svc, "target-b2", base)
	cw, cr := 0.9, 0.7
	priced := seedBulkPriceCandidate(t, svc, p.ID, pricedModel, "shared-y2", base.Add(time.Minute), func(in *modeladmin.CreateCandidateInput) {
		bulkPriceTokenPrices(5, 5, &cw, &cr)(in)
	})
	hist2Model := seedBulkPriceModel(t, svc, "hist-b2", base)
	seedBulkPriceCandidate(t, svc, p.ID, hist2Model, "shared-y2", base, bulkPriceTokenPrices(1, 4, nil, nil))

	result2, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{priced}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates (priced arm) failed: %v", err)
	}
	item2 := result2.Items[0]
	if !item2.Changed || item2.SkipReason != "" {
		t.Fatalf("the priced target must be overwritten by the hit, got %+v", item2)
	}
	if item2.After.InputPrice != 1 || item2.After.OutputPrice != 4 {
		t.Fatalf("after must come from the history row, got %+v", item2.After)
	}
	if item2.After.CacheWritePrice != nil || item2.After.CacheReadPrice != nil {
		t.Fatalf("the hit's null caches must overwrite the row's own values, got %+v", item2.After)
	}
	stored := loadBulkPriceCandidate(t, db, priced)
	if stored.InputPrice != 1 || stored.CacheWritePrice != nil || stored.CacheReadPrice != nil {
		t.Fatalf("stored row must lose its cache values to the hit's nulls, got %+v", stored)
	}
}

// The reset semantics: an already-priced row never suggests from itself. The
// target is the NEWEST history row for its folded name, so a self-inclusive
// look-up would echo its own 5/5 back as the "suggestion" and the operation
// would be a no-op for every priced row. Excluding the target itself lets the
// older sibling's 2/8 overwrite it.
func TestBulkPriceApplySuggestedExcludesSelfAndResetsFromOlderHistory(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	base := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	siblingModel := seedBulkPriceModel(t, svc, "hist-c", base)
	seedBulkPriceCandidate(t, svc, p.ID, siblingModel, "shared-z", base, bulkPriceTokenPrices(2, 8, nil, nil))
	targetModel := seedBulkPriceModel(t, svc, "target-c", base)
	target := seedBulkPriceCandidate(t, svc, p.ID, targetModel, "shared-z", base.Add(time.Minute), bulkPriceTokenPrices(5, 5, nil, nil))

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{target}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	item := result.Items[0]
	if !item.Changed {
		t.Fatalf("the reset must overwrite the row's own price, got %+v", item)
	}
	if item.Before.InputPrice != 5 || item.After.InputPrice != 2 || item.After.OutputPrice != 8 {
		t.Fatalf("after must come from the older history row, got before %+v after %+v", item.Before, item.After)
	}
}

// The self-hit trap's zero-price arm: an unpriced target row is itself the
// newest history row for its folded name. A naive look-up would 0-price-hit
// and short-circuit the catalog; the criterion excludes the row itself and
// falls back to the catalog.
func TestBulkPriceApplySuggestedZeroPriceSelfHitFallsBackToCatalog(t *testing.T) {
	base := time.Now().UTC()
	providerService, pdb, client := newTestProviderService(t)
	prov := seedProviderWithBaseURL(t, providerService, "deepseek", catalogSeededHost)
	svc := modeladmin.NewModelService(pdb, testutil.ProviderSecrets(), client)
	name := catalogSeededModel(t)
	m := seedBulkPriceModel(t, svc, "self-hit", base)
	// The target row IS the newest (and only) history row for its folded
	// name: unpriced 0/0.
	target := seedBulkPriceCandidate(t, svc, prov.ID, m, name, base, nil)

	want, ok := pricecatalog.Lookup(catalogSeededHost, name)
	if !ok {
		t.Fatalf("catalog lost %s", name)
	}
	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{target}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	item := result.Items[0]
	if !item.Changed || item.SkipReason != "" {
		t.Fatalf("the catalog fallback must fill the row, got %+v", item)
	}
	if item.After.InputPrice != want.Input || item.After.OutputPrice != want.Output {
		t.Fatalf("after must carry the catalog price %v/%v, got %+v", want.Input, want.Output, item.After)
	}
	if (item.After.CacheWritePrice == nil) != (want.CacheWrite == nil) || (item.After.CacheReadPrice == nil) != (want.CacheRead == nil) {
		t.Fatalf("cache slots must follow the catalog's nils, got %+v want %+v", item.After, want)
	}
	after := loadBulkPriceCandidate(t, pdb, target)
	if !after.PricesDeclared {
		t.Fatalf("the catalog fill flips prices_declared")
	}
}

// The non-zero criterion's skip arm: the newest non-self history row for the
// folded name may be an all-zero record, which is unpriced history, not a
// price to copy — the resolution must walk past it and adopt the older
// non-zero sibling instead of stopping at the zero or ending no_suggestion.
func TestBulkPriceApplySuggestedSkipsAllZeroSiblingForOlderNonZero(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	base := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	// Oldest: a priced 2/8 row. Middle: an explicitly declared 0/0 row — the
	// newest non-self history for the folded name. Newest: the unpriced
	// target, which the self-exclusion removes on its own.
	nonZeroModel := seedBulkPriceModel(t, svc, "hist-g", base)
	seedBulkPriceCandidate(t, svc, p.ID, nonZeroModel, "shared-w", base, bulkPriceTokenPrices(2, 8, nil, nil))
	zeroModel := seedBulkPriceModel(t, svc, "hist-h", base)
	seedBulkPriceCandidate(t, svc, p.ID, zeroModel, "shared-w", base.Add(time.Minute), bulkPriceTokenPrices(0, 0, nil, nil))
	targetModel := seedBulkPriceModel(t, svc, "target-g", base)
	target := seedBulkPriceCandidate(t, svc, p.ID, targetModel, "shared-w", base.Add(2*time.Minute), nil)

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{target}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	item := result.Items[0]
	if !item.Changed || item.SkipReason != "" {
		t.Fatalf("the non-zero sibling must win over the all-zero one, got %+v", item)
	}
	if item.After.InputPrice != 2 || item.After.OutputPrice != 8 {
		t.Fatalf("after must come from the non-zero sibling, not the zero one, got %+v", item.After)
	}
	after := loadBulkPriceCandidate(t, db, target)
	if after.InputPrice != 2 || after.OutputPrice != 8 {
		t.Fatalf("stored row must carry the non-zero sibling's prices, got %v/%v", after.InputPrice, after.OutputPrice)
	}
}

// The non-zero criterion's fallback arms: a target whose ONLY non-self
// history is an all-zero sibling has no usable history — a catalog-carried
// name must fall through to the catalog, a name the catalog does not carry
// must end no_suggestion. Neither may adopt the zero record as its price.
func TestBulkPriceApplySuggestedZeroOnlyHistoryFallsBackOrStaysUnresolved(t *testing.T) {
	base := time.Now().UTC()
	providerService, pdb, client := newTestProviderService(t)
	prov := seedProviderWithBaseURL(t, providerService, "deepseek", catalogSeededHost)
	svc := modeladmin.NewModelService(pdb, testutil.ProviderSecrets(), client)

	// Catalog leg: the zero sibling is skipped and the catalog answers.
	name := catalogSeededModel(t)
	m := seedBulkPriceModel(t, svc, "zero-catalog-h", base)
	seedBulkPriceCandidate(t, svc, prov.ID, m, name, base, bulkPriceTokenPrices(0, 0, nil, nil))
	m2 := seedBulkPriceModel(t, svc, "zero-catalog-t", base)
	catalogTarget := seedBulkPriceCandidate(t, svc, prov.ID, m2, name, base.Add(time.Minute), nil)

	// No-catalog leg: the same shape on a host the catalog does not carry.
	plain := seedProviderWithBaseURL(t, providerService, "plain", "https://example.invalid")
	plainName := "zero-only-plain-x"
	pm := seedBulkPriceModel(t, svc, "zero-plain-h", base)
	seedBulkPriceCandidate(t, svc, plain.ID, pm, plainName, base, bulkPriceTokenPrices(0, 0, nil, nil))
	pm2 := seedBulkPriceModel(t, svc, "zero-plain-t", base)
	plainTarget := seedBulkPriceCandidate(t, svc, plain.ID, pm2, plainName, base.Add(time.Minute), nil)

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{catalogTarget, plainTarget}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	byID := map[uint]modeladmin.BulkPriceItem{}
	for _, item := range result.Items {
		byID[item.CandidateID] = item
	}
	want, ok := pricecatalog.Lookup(catalogSeededHost, name)
	if !ok {
		t.Fatalf("catalog lost %s", name)
	}
	catItem := byID[catalogTarget]
	if !catItem.Changed || catItem.SkipReason != "" {
		t.Fatalf("the catalog fallback must fill the row past the zero sibling, got %+v", catItem)
	}
	if catItem.After.InputPrice != want.Input || catItem.After.OutputPrice != want.Output {
		t.Fatalf("after must carry the catalog price %v/%v, got %+v", want.Input, want.Output, catItem.After)
	}
	plainItem := byID[plainTarget]
	if plainItem.Changed || plainItem.SkipReason != modeladmin.BulkPriceSkipNoSuggestion {
		t.Fatalf("a zero-only history off the catalog must end no_suggestion, got %+v", plainItem)
	}
	if plainItem.After.InputPrice != 0 || plainItem.After.OutputPrice != 0 {
		t.Fatalf("an unresolved row must not carry the zero sibling's prices, got %+v", plainItem.After)
	}
}

func TestBulkPriceApplySuggestedRejectsModeMismatchedHistory(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	base := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")

	// Token target whose newest history row is AUDIO mode: the mismatched
	// suggestion must not be adopted, and with no catalog entry the row ends
	// no_suggestion.
	audioHistModel := seedBulkPriceModel(t, svc, "hist-d", base)
	seedBulkPriceCandidate(t, svc, p.ID, audioHistModel, "voice-x", base, func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeAudio
		in.AudioUnitPrice = ptrFloat(3)
	})
	tokenTargetModel := seedBulkPriceModel(t, svc, "target-d", base)
	tokenTarget := seedBulkPriceCandidate(t, svc, p.ID, tokenTargetModel, "voice-x", base.Add(time.Minute), nil)

	// Audio target whose newest history row is TOKEN mode: same rejection.
	tokenHistModel := seedBulkPriceModel(t, svc, "hist-e", base)
	seedBulkPriceCandidate(t, svc, p.ID, tokenHistModel, "chat-x", base, bulkPriceTokenPrices(2, 8, nil, nil))
	audioTargetModel := seedBulkPriceModel(t, svc, "target-e", base)
	audioTarget := seedBulkPriceCandidate(t, svc, p.ID, audioTargetModel, "chat-x", base.Add(time.Minute), func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeAudio
	})

	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{tokenTarget, audioTarget}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	if result.Applied != 0 || result.Skipped != 2 {
		t.Fatalf("both mismatched rows must skip, got %+v", result)
	}
	for _, item := range result.Items {
		if item.SkipReason != modeladmin.BulkPriceSkipNoSuggestion {
			t.Errorf("candidate %d skip reason = %q, want no_suggestion", item.CandidateID, item.SkipReason)
		}
		if item.Changed {
			t.Errorf("candidate %d must not change", item.CandidateID)
		}
	}
}

func TestBulkPriceApplySuggestedAudioFromHistoryAndCatalog(t *testing.T) {
	base := time.Now().UTC()
	// History leg: an audio sibling's char price fills the target audio row.
	svc, db, _ := newTestModelService(t)
	p := seedEnabledProvider(t, db, "prov-a")
	histModel := seedBulkPriceModel(t, svc, "hist-f", base)
	seedBulkPriceCandidate(t, svc, p.ID, histModel, "tts-shared", base, func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeAudio
		in.AudioUnitPrice = ptrFloat(2)
	})
	targetModel := seedBulkPriceModel(t, svc, "target-f", base)
	audioTarget := seedBulkPriceCandidate(t, svc, p.ID, targetModel, "tts-shared", base.Add(time.Minute), func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeAudio
	})
	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{audioTarget}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	item := result.Items[0]
	if !item.Changed || item.After.AudioUnitPrice == nil || *item.After.AudioUnitPrice != 2 {
		t.Fatalf("audio history hit must fill the char price with 2, got %+v", item)
	}
	after := loadBulkPriceCandidate(t, db, audioTarget)
	if after.AudioUnitPrice == nil || *after.AudioUnitPrice != 2 || !after.PricesDeclared {
		t.Fatalf("stored audio row must carry the suggestion and declare it, got %+v", after)
	}

	// Catalog leg: an audio-catalog host fills an unpriced audio row with no
	// history at all.
	providerService, pdb, client := newTestProviderService(t)
	catalogSvc := modeladmin.NewModelService(pdb, testutil.ProviderSecrets(), client)
	prov := seedProviderWithBaseURL(t, providerService, "minimax", "https://api.minimax.cn")
	m := seedBulkPriceModel(t, catalogSvc, "tts-catalog", base)
	catalogTarget := seedBulkPriceCandidate(t, catalogSvc, prov.ID, m, "Speech-2.8-HD", base, func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeAudio
	})
	cresult, err := catalogSvc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{catalogTarget}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	citem := cresult.Items[0]
	if !citem.Changed || citem.After.AudioUnitPrice == nil || *citem.After.AudioUnitPrice != 350 {
		t.Fatalf("audio catalog fallback must fill 350, got %+v", citem)
	}
}

func TestBulkPriceApplySuggestedImageRowUnsupported(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	base := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	m := seedBulkPriceModel(t, svc, "apply-image", base)
	imageRow := seedBulkPriceCandidate(t, svc, p.ID, m, "img-x", base, func(in *modeladmin.CreateCandidateInput) {
		in.BillingMode = model.BillingModeImage
		in.ImagePricingTiers = &model.ImagePricingTiers{Mode: "per_image",
			Tiers: []model.ImagePricingTier{{Quality: "standard", Size: "1024x1024", Price: 0.04}}}
	})
	result, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{imageRow}, Operation: modeladmin.BulkPriceOperationApplySuggested,
		DryRun: ptrBool(false),
	}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("BulkPriceCandidates failed: %v", err)
	}
	// Image/video rows are unsupported_billing_mode under BOTH operations —
	// they never reach the no_suggestion endpoint.
	if result.Items[0].SkipReason != modeladmin.BulkPriceSkipUnsupportedBillingMode {
		t.Fatalf("skip reason = %q, want unsupported_billing_mode", result.Items[0].SkipReason)
	}
}

// AC4a: the apply recomputes from the CURRENT stored row, not from the
// preview's snapshot — a price changed between preview and apply is what the
// factor multiplies.
func TestBulkPriceRecalculatesFromCurrentRowAtApply(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	base := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-a")
	m := seedBulkPriceModel(t, svc, "recalc", base)
	id := seedBulkPriceCandidate(t, svc, p.ID, m, "recalc-x", base, bulkPriceTokenPrices(2, 4, nil, nil))

	preview, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{id}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(2), DryRun: ptrBool(true),
	}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("preview failed: %v", err)
	}
	if preview.Items[0].After.InputPrice != 4 {
		t.Fatalf("preview after = %+v, want input 4", preview.Items[0].After)
	}

	// Someone reprices the row between preview and apply.
	if _, err := svc.UpdateModelCandidate(context.Background(), id, modeladmin.UpdateCandidateInput{
		InputPrice: ptrFloat(5), OutputPrice: ptrFloat(7),
	}, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("interleave price edit failed: %v", err)
	}

	applied, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{id}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(2), DryRun: ptrBool(false),
	}, base.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	// after = the NEW current value x factor, not the preview's snapshot.
	if applied.Items[0].After.InputPrice != 10 || applied.Items[0].After.OutputPrice != 14 {
		t.Fatalf("apply must scale the current row (10/14), got %+v", applied.Items[0].After)
	}
	after := loadBulkPriceCandidate(t, db, id)
	if after.InputPrice != 10 || after.OutputPrice != 14 {
		t.Fatalf("stored row must carry 10/14, got %v/%v", after.InputPrice, after.OutputPrice)
	}
}

// AC4b: a storage failure injected at the second write rolls the whole batch
// back — zero rows committed, no half-applied price batch.
func TestBulkPriceSingleTransactionRollsBackAllOnWriteFailure(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	base := time.Now().UTC()
	pA := seedEnabledProvider(t, db, "prov-a")
	pB := seedEnabledProvider(t, db, "prov-b")
	m := seedBulkPriceModel(t, svc, "atomic", base)
	rowA := seedBulkPriceCandidate(t, svc, pA.ID, m, "boom-a", base, bulkPriceTokenPrices(1, 1, nil, nil))
	rowB := seedBulkPriceCandidate(t, svc, pB.ID, m, "boom-b", base, bulkPriceTokenPrices(2, 2, nil, nil))
	before := snapshotBulkPriceCandidates(t, db)

	calls := 0
	if err := db.Callback().Update().Before("gorm:update").Register("fail_on_boom_bulk_price", func(tx *gorm.DB) {
		calls++
		if calls == 2 {
			_ = tx.AddError(errors.New("injected storage failure"))
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	_, err := svc.BulkPriceCandidates(modeladmin.BulkPriceInput{
		CandidateIDs: []uint{rowA, rowB}, Operation: modeladmin.BulkPriceOperationScale,
		Factor: ptrFloat(3), DryRun: ptrBool(false),
	}, base.Add(time.Minute))
	if err == nil {
		t.Fatal("expected the injected storage failure to surface")
	}
	if after := snapshotBulkPriceCandidates(t, db); !reflect.DeepEqual(before, after) {
		t.Fatalf("a failed batch must commit zero rows")
	}
}

func TestBulkPriceCandidatesValidatesInput(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	cases := []struct {
		name string
		in   modeladmin.BulkPriceInput
	}{
		{"empty ids", modeladmin.BulkPriceInput{Operation: modeladmin.BulkPriceOperationScale, Factor: ptrFloat(2), DryRun: ptrBool(false)}},
		{"over cap ids", modeladmin.BulkPriceInput{CandidateIDs: make([]uint, modeladmin.BulkPriceMaxCandidates+1), Operation: modeladmin.BulkPriceOperationScale, Factor: ptrFloat(2), DryRun: ptrBool(false)}},
		{"nil dry_run", modeladmin.BulkPriceInput{CandidateIDs: []uint{1}, Operation: modeladmin.BulkPriceOperationScale, Factor: ptrFloat(2)}},
		{"missing operation", modeladmin.BulkPriceInput{CandidateIDs: []uint{1}, DryRun: ptrBool(false)}},
		{"unknown operation", modeladmin.BulkPriceInput{CandidateIDs: []uint{1}, Operation: "multiply", Factor: ptrFloat(2), DryRun: ptrBool(false)}},
		{"scale without factor", modeladmin.BulkPriceInput{CandidateIDs: []uint{1}, Operation: modeladmin.BulkPriceOperationScale, DryRun: ptrBool(false)}},
		{"scale zero factor", modeladmin.BulkPriceInput{CandidateIDs: []uint{1}, Operation: modeladmin.BulkPriceOperationScale, Factor: ptrFloat(0), DryRun: ptrBool(false)}},
		{"scale negative factor", modeladmin.BulkPriceInput{CandidateIDs: []uint{1}, Operation: modeladmin.BulkPriceOperationScale, Factor: ptrFloat(-1.5), DryRun: ptrBool(false)}},
		{"apply_suggested with zero factor", modeladmin.BulkPriceInput{CandidateIDs: []uint{1}, Operation: modeladmin.BulkPriceOperationApplySuggested, Factor: ptrFloat(0), DryRun: ptrBool(false)}},
		{"apply_suggested with positive factor", modeladmin.BulkPriceInput{CandidateIDs: []uint{1}, Operation: modeladmin.BulkPriceOperationApplySuggested, Factor: ptrFloat(1.5), DryRun: ptrBool(false)}},
	}
	for _, tc := range cases {
		_, err := svc.BulkPriceCandidates(tc.in, now)
		if !errors.Is(err, modeladmin.ErrBulkPriceInvalid) {
			t.Errorf("%s: err = %v, want ErrBulkPriceInvalid", tc.name, err)
		}
	}
}
