package modeladmin_test

// The price-declaration and probe-debt state columns: what a stored zero
// cannot say on its own (unpriced vs explicitly free) and what the queue
// still owes a row (a probe vs an enable). Covers the import price
// semantics, the tier-pending carve-out for image/video output, the
// no-auto-enable guard for unpriced rows, restart self-heal keyed on the
// probe debt, the conditional requeue re-arm, and the derivation on every
// candidate write path.

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/service/providerclient/providerclienttest"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

func newPriceDeclarationTestService(t *testing.T) (*modeladmin.ModelService, *gorm.DB, *provider.ProviderService, *providerclienttest.Fake) {
	t.Helper()
	providerService, db, client := newTestProviderService(t)
	return modeladmin.NewModelService(db, testutil.ProviderSecrets(), client), db, providerService, client
}

// TC-03: an absent price slot is unpriced (stored 0, declaration false), a
// submitted one is a price claim even at 0, and the token slots stay the
// legacy rows' only voice.
func TestImportPriceSemanticsPerRowShape(t *testing.T) {
	svc, db, _, _ := newPriceDeclarationTestService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-price-semantics")

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "row-all-empty"},
		{ProviderModelName: "row-input-only", InputPrice: ptrFloat(5)},
		{ProviderModelName: "row-audio-empty", OutputModalities: []string{model.OutputModalityAudio}},
		{ProviderModelName: "row-legacy-zero", InputPrice: ptrFloat(0), OutputPrice: ptrFloat(0)},
		{ProviderModelName: "row-legacy-audio", OutputModalities: []string{model.OutputModalityAudio}},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	for _, item := range result.Items {
		if item.Status != modeladmin.ImportStatusCreated {
			t.Fatalf("expected every row created, got %+v", item)
		}
	}

	assertRow := func(name string, check func(c model.ModelCandidate) bool, why string) {
		t.Helper()
		var c model.ModelCandidate
		if err := db.Where("provider_model_name = ?", name).First(&c).Error; err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if !check(c) {
			t.Fatalf("%s: %s — got declared=%v armed=%v prices=%v/%v/%v/%v audio=%v mode=%s",
				name, why, c.PricesDeclared, c.AutoEnableOnPass, c.InputPrice, c.OutputPrice,
				c.CacheWritePrice, c.CacheReadPrice, c.AudioUnitPrice, c.BillingMode)
		}
	}
	// 1. Prices all empty: stored zeros and NULL caches, undeclared, never armed.
	assertRow("row-all-empty", func(c model.ModelCandidate) bool {
		return c.InputPrice == 0 && c.OutputPrice == 0 && c.CacheWritePrice == nil && c.CacheReadPrice == nil &&
			!c.PricesDeclared && !c.AutoEnableOnPass && c.ProbeOwed
	}, "all-empty token row must store zeros, undeclared and disarmed")
	// 2. Only the input price filled: the output slot is still missing.
	assertRow("row-input-only", func(c model.ModelCandidate) bool {
		return c.InputPrice == 5 && c.OutputPrice == 0 && !c.PricesDeclared && !c.AutoEnableOnPass
	}, "input-only row must stay undeclared")
	// 3. An audio row with the character price left empty: NULL, undeclared.
	assertRow("row-audio-empty", func(c model.ModelCandidate) bool {
		return c.BillingMode == model.BillingModeAudio && c.AudioUnitPrice == nil &&
			!c.PricesDeclared && !c.AutoEnableOnPass
	}, "audio row with no character price must stay undeclared")
	// 4. The legacy form — token prices explicitly 0, caches omitted: a
	// free-price claim, declared and armed exactly as the pre-column write.
	assertRow("row-legacy-zero", func(c model.ModelCandidate) bool {
		return c.InputPrice == 0 && c.OutputPrice == 0 && c.PricesDeclared && c.AutoEnableOnPass && c.ArmedAt != nil
	}, "explicit-zero token row must be declared and armed")
	// 5. The legacy frontend's audio form — modality switched, character
	// price omitted: undeclared, and the guard keeps it disarmed.
	assertRow("row-legacy-audio", func(c model.ModelCandidate) bool {
		return c.BillingMode == model.BillingModeAudio && !c.PricesDeclared && !c.AutoEnableOnPass
	}, "audio row with omitted character price must stay undeclared and disarmed")
}

// TC-04: an explicit 0 is a free-price claim — declared, and the import's
// auto-enable promise survives (cache slots left empty do not break
// completeness; the audio row declares by its character price).
func TestImportExplicitZeroPricesAreFreeAndArm(t *testing.T) {
	svc, db, _, _ := newPriceDeclarationTestService(t)
	now := time.Now().UTC()
	p := seedEnabledProvider(t, db, "prov-explicit-zero")

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "free-token", InputPrice: ptrFloat(0), OutputPrice: ptrFloat(0)},
		{ProviderModelName: "free-audio", OutputModalities: []string{model.OutputModalityAudio}, AudioUnitPrice: ptrFloat(0)},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	for _, item := range result.Items {
		if item.Status != modeladmin.ImportStatusCreated {
			t.Fatalf("expected every row created, got %+v", item)
		}
		var c model.ModelCandidate
		if err := db.First(&c, item.CandidateID).Error; err != nil {
			t.Fatalf("imported candidate not stored: %v", err)
		}
		if !c.PricesDeclared {
			t.Fatalf("%s: an explicit zero is a free-price claim, got declared=false", item.Name)
		}
		if !c.AutoEnableOnPass || c.ArmedAt == nil {
			t.Fatalf("%s: a fully priced free row must carry the auto-enable promise, got armed=%v", item.Name, c.AutoEnableOnPass)
		}
	}
}

// TC-04b: any output declaration carrying image or video stops the row from
// auto-enabling — those bill through tier tables the import never carries, so
// a pass must land them at Passed+Disabled until the ladder is configured.
func TestImportImageVideoOutputRowsStopDisabledPendingTiers(t *testing.T) {
	svc, db, providerSvc, _ := newPriceDeclarationTestService(t)
	p := seedEnabledProviderForModelTest(t, providerSvc, "prov-tier-pending")

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "img-exclusive", OutputModalities: []string{model.OutputModalityImage}, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
		{ProviderModelName: "video-exclusive", OutputModalities: []string{model.OutputModalityVideo}, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
		{ProviderModelName: "text-and-image", OutputModalities: []string{model.OutputModalityText, model.OutputModalityImage}, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
		{ProviderModelName: "image-and-audio", OutputModalities: []string{model.OutputModalityImage, model.OutputModalityAudio}, AudioUnitPrice: ptrFloat(3.5)},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	ids := make([]uint, 0, len(result.Items))
	for _, item := range result.Items {
		if item.Status != modeladmin.ImportStatusCreated {
			t.Fatalf("expected every row created, got %+v", item)
		}
		ids = append(ids, item.CandidateID)
	}

	// Declared per the row's primary price slots, disarmed across the board.
	for _, id := range ids {
		c := loadCandidate(t, db, id)
		if !c.PricesDeclared {
			t.Fatalf("candidate %d: its primary price slots are filled, expected declared", id)
		}
		if c.AutoEnableOnPass {
			t.Fatalf("candidate %d: an output declaration with image or video must never arm", id)
		}
		if !c.ProbeOwed {
			t.Fatalf("candidate %d: an imported row is owed its probe regardless", id)
		}
		// A queued row's probe runs and the pass lands without the enable.
		if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
			t.Fatalf("ProbeQueuedCandidate(%d) failed: %v", id, err)
		}
		c = loadCandidate(t, db, id)
		if c.VerificationStatus != model.ModelVerificationStatusPassed || c.ManagementStatus != model.ModelCandidateStatusDisabled {
			t.Fatalf("candidate %d: expected Passed+Disabled pending tiers, got %d/%d", id, c.VerificationStatus, c.ManagementStatus)
		}
	}
}

// TC-05: an unpriced row probes to a verdict but never auto-enables, and the
// response DTO carries the two new facts.
func TestImportUnpricedRowProbesPassedWithoutEnabling(t *testing.T) {
	svc, db, providerSvc, _ := newPriceDeclarationTestService(t)
	p := seedEnabledProviderForModelTest(t, providerSvc, "prov-unpriced-pass")

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "unpriced-pass"},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	item := result.Items[0]
	if err := svc.ProbeQueuedCandidate(context.Background(), item.CandidateID, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}

	c := loadCandidate(t, db, item.CandidateID)
	if c.ManagementStatus != model.ModelCandidateStatusDisabled || c.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected Passed+Disabled, got %d/%d", c.ManagementStatus, c.VerificationStatus)
	}
	if c.PricesDeclared || c.AutoEnableOnPass || c.ProbeOwed {
		t.Fatalf("expected declared=false armed=false owed=false after the stamp, got %v/%v/%v", c.PricesDeclared, c.AutoEnableOnPass, c.ProbeOwed)
	}

	list, err := svc.ListProviderCandidates(p.ID)
	if err != nil {
		t.Fatalf("ListProviderCandidates failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one listed row, got %+v", list)
	}
	if list[0].PricesDeclared || list[0].ProbeOwed || list[0].AutoEnableOnPass {
		t.Fatalf("DTO must carry the settled facts, got declared=%v owed=%v armed=%v",
			list[0].PricesDeclared, list[0].ProbeOwed, list[0].AutoEnableOnPass)
	}
}

// TC-05c: restart self-heal keys on the probe debt — an unpriced import row
// is re-collected and probed (never enabled), a deliberately unprobed manual
// row and a retargeted row are left alone, and an armed Passed+Disabled row's
// enable promise is still delivered.
func TestRestartSelfHealKeysOnProbeOwed(t *testing.T) {
	svc, db, providerSvc, client := newPriceDeclarationTestService(t)
	p := seedEnabledProviderForModelTest(t, providerSvc, "prov-restart-heal")
	now := time.Now().UTC()

	// The unpriced import row: probed but never enabled.
	imported, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "heal-unpriced"},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	unpricedID := imported.Items[0].CandidateID

	// A manually created row saved as disabled — deliberately unprobed.
	manualView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "heal-manual"}, now)
	if err != nil {
		t.Fatalf("seed manual model: %v", err)
	}
	manual, err := svc.CreateModelCandidate(context.Background(), manualView.ID, modeladmin.CreateCandidateInput{
		ProviderID: p.ID, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
	}, now)
	if err != nil {
		t.Fatalf("seed manual candidate: %v", err)
	}

	// A retargeted row: the reset clears the debt with the verification.
	retargetView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "heal-retarget"}, now)
	if err != nil {
		t.Fatalf("seed retarget model: %v", err)
	}
	retargeted, err := svc.CreateModelCandidate(context.Background(), retargetView.ID, modeladmin.CreateCandidateInput{
		ProviderID: p.ID, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
	}, now)
	if err != nil {
		t.Fatalf("seed retarget candidate: %v", err)
	}
	if _, err := svc.UpdateModelCandidate(context.Background(), retargeted.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: ptrString("renamed-target"),
	}, now); err != nil {
		t.Fatalf("retarget failed: %v", err)
	}

	// A priced Passed+Disabled row still armed: owed its ENABLE. The pass
	// was seeded raw with the arming alignment intact (armed_at equals the
	// row clock) — the alignment the fulfill's enable checks.
	owedEnable := seedUntestedCandidate(t, svc, db, p.ID, "heal-owed-enable")
	landed := time.Now().UTC()
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", owedEnable).
		Updates(map[string]interface{}{
			"verification_status": model.ModelVerificationStatusPassed,
			"last_tested_at":      landed,
			"armed_at":            landed,
			"updated_at":          landed,
		}).Error; err != nil {
		t.Fatalf("seed owed-enable state: %v", err)
	}

	queue := modeladmin.NewProbeQueue(svc, 1)
	if err := queue.RecoverPending(); err != nil {
		t.Fatalf("RecoverPending failed: %v", err)
	}
	states := queue.CandidateQueueStates([]uint{unpricedID, manual.ID, retargeted.ID, owedEnable})
	if states[unpricedID] != modeladmin.QueueStateQueued {
		t.Fatalf("the unpriced import row must be re-collected by its probe debt, got %q", states[unpricedID])
	}
	if states[manual.ID] != "" || states[retargeted.ID] != "" {
		t.Fatalf("the manual and retargeted rows must stay out of recovery, got %q/%q", states[manual.ID], states[retargeted.ID])
	}
	if states[owedEnable] != modeladmin.QueueStateQueued {
		t.Fatalf("the armed Passed+Disabled row must be re-collected for its enable, got %q", states[owedEnable])
	}

	basicBefore := client.CallCountFor("basic")
	if err := svc.ProbeQueuedCandidate(context.Background(), unpricedID, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate(unpriced) failed: %v", err)
	}
	c := loadCandidate(t, db, unpricedID)
	if c.VerificationStatus != model.ModelVerificationStatusPassed || c.ManagementStatus != model.ModelCandidateStatusDisabled || c.AutoEnableOnPass {
		t.Fatalf("the healed unpriced row must stop at Passed+Disabled disarmed, got %d/%d armed=%v",
			c.VerificationStatus, c.ManagementStatus, c.AutoEnableOnPass)
	}

	// The enable promise is delivered without spending another probe.
	if err := svc.ProbeQueuedCandidate(context.Background(), owedEnable, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate(owedEnable) failed: %v", err)
	}
	enabled := loadCandidate(t, db, owedEnable)
	if enabled.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("the armed Passed+Disabled row must be enabled on recovery, got %d", enabled.ManagementStatus)
	}
	if client.CallCountFor("basic")-basicBefore != 1 {
		t.Fatalf("the owed-enable delivery must not spend a probe, basic calls delta=%d", client.CallCountFor("basic")-basicBefore)
	}
}

// TC-07C step C: a requeue re-arms only rows whose stored pricing supports
// it, never reprices, and always restores the probe debt.
func TestRequeueConditionalRearm(t *testing.T) {
	svc, db, providerSvc, _ := newPriceDeclarationTestService(t)
	p := seedEnabledProviderForModelTest(t, providerSvc, "prov-requeue-rearm")
	now := time.Now().UTC()

	result, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "requeue-priced", InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
		{ProviderModelName: "requeue-unpriced"},
		{ProviderModelName: "requeue-image", OutputModalities: []string{model.OutputModalityImage}, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2)},
	}, now)
	if err != nil {
		t.Fatalf("ImportProviderModels failed: %v", err)
	}
	byName := map[string]modeladmin.ImportItemResult{}
	for _, item := range result.Items {
		byName[item.Name] = item
	}

	// The arming stamps BEFORE the requeue: the priced row was armed at its
	// creation, the unpriced and image rows were never armed (NULL). These
	// are the old values a disarming requeue must leave untouched.
	armedBefore := map[string]*time.Time{}
	for _, name := range []string{"requeue-priced", "requeue-unpriced", "requeue-image"} {
		var c model.ModelCandidate
		if err := db.First(&c, byName[name].CandidateID).Error; err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		armedBefore[name] = c.ArmedAt
	}
	sameStamp := func(a, b *time.Time) bool {
		if a == nil || b == nil {
			return a == b
		}
		return a.Equal(*b)
	}

	// The re-import submits DIFFERENT prices to prove the requeue never
	// reprices: only the residue clears, the debt restores, and the arm
	// follows the stored declaration.
	requeued, err := svc.ImportProviderModels(p.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "requeue-priced", InputPrice: ptrFloat(9), OutputPrice: ptrFloat(9)},
		{ProviderModelName: "requeue-unpriced", InputPrice: ptrFloat(9), OutputPrice: ptrFloat(9)},
		{ProviderModelName: "requeue-image", InputPrice: ptrFloat(9), OutputPrice: ptrFloat(9)},
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("requeuing import failed: %v", err)
	}
	for _, item := range requeued.Items {
		if item.Status != modeladmin.ImportStatusSkipped || item.CandidateID == 0 {
			t.Fatalf("expected an exists skip carrying the requeue id, got %+v", item)
		}
	}

	wantArmed := map[string]bool{"requeue-priced": true, "requeue-unpriced": false, "requeue-image": false}
	wantDeclared := map[string]bool{"requeue-priced": true, "requeue-unpriced": false, "requeue-image": true}

	for _, name := range []string{"requeue-priced", "requeue-unpriced", "requeue-image"} {
		var c model.ModelCandidate
		if err := db.First(&c, byName[name].CandidateID).Error; err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if !c.ProbeOwed {
			t.Fatalf("%s: a requeue must restore the probe debt", name)
		}
		if c.AutoEnableOnPass != wantArmed[name] {
			t.Fatalf("%s: requeue arm mismatch, got %v want %v", name, c.AutoEnableOnPass, wantArmed[name])
		}
		if c.PricesDeclared != wantDeclared[name] {
			t.Fatalf("%s: requeue must not touch the declared fact, got %v want %v", name, c.PricesDeclared, wantDeclared[name])
		}
		// armed_at is judged by the SAME predicate as the flag: a re-armed
		// row takes a refreshed stamp aligned with its row clock (the
		// alignment the probe commit's enable checks), while a disarmed row
		// keeps its previous arming history — NULL for these never-armed
		// rows — instead of being stamped with an armed_at it must never
		// carry.
		if wantArmed[name] {
			if c.ArmedAt == nil {
				t.Fatalf("%s: a re-armed row must carry a fresh armed_at, got NULL", name)
			}
			if sameStamp(c.ArmedAt, armedBefore[name]) {
				t.Fatalf("%s: the requeue must refresh the armed stamp, got %v", name, *c.ArmedAt)
			}
			if !c.ArmedAt.Equal(c.UpdatedAt) {
				t.Fatalf("%s: the refreshed armed_at must stay aligned with updated_at, got armed=%v updated=%v",
					name, *c.ArmedAt, c.UpdatedAt)
			}
		} else if !sameStamp(c.ArmedAt, armedBefore[name]) {
			t.Fatalf("%s: a disarmed row must keep its previous arming history (NULL here), got armed_at=%v",
				name, c.ArmedAt)
		}
		if name == "requeue-priced" && (c.InputPrice != 1 || c.OutputPrice != 2) {
			t.Fatalf("%s: requeue must never reprice, got %v/%v", name, c.InputPrice, c.OutputPrice)
		}

		// The requeued probes run to their verdicts.
		if err := svc.ProbeQueuedCandidate(context.Background(), c.ID, time.Now().UTC()); err != nil {
			t.Fatalf("ProbeQueuedCandidate(%s) failed: %v", name, err)
		}
		c = loadCandidate(t, db, c.ID)
		if c.VerificationStatus != model.ModelVerificationStatusPassed {
			t.Fatalf("%s: expected the probe to pass, got %d", name, c.VerificationStatus)
		}
		wantStatus := model.ModelCandidateStatusDisabled
		if wantArmed[name] {
			wantStatus = model.ModelCandidateStatusEnabled
		}
		if c.ManagementStatus != wantStatus {
			t.Fatalf("%s: pass must land management status %d, got %d", name, wantStatus, c.ManagementStatus)
		}
	}
}

// The manual create path derives the declared fact from the submitted slots:
// absent means unpriced, explicit zero means free.
func TestManualCreateDerivesPricesDeclared(t *testing.T) {
	svc, db, providerSvc, _ := newPriceDeclarationTestService(t)
	now := time.Now().UTC()
	p := seedEnabledProviderForModelTest(t, providerSvc, "prov-manual-create")

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "manual-derive"}, now)
	if err != nil {
		t.Fatalf("seed model: %v", err)
	}

	empty, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p.ID,
	}, now)
	if err != nil {
		t.Fatalf("create empty-price candidate: %v", err)
	}
	if empty.PricesDeclared {
		t.Fatal("a create with every price omitted must store undeclared")
	}
	p2 := seedEnabledProviderForModelTest(t, providerSvc, "prov-manual-create-2")
	free, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p2.ID, InputPrice: ptrFloat(0), OutputPrice: ptrFloat(0),
	}, now)
	if err != nil {
		t.Fatalf("create explicit-zero candidate: %v", err)
	}
	if !free.PricesDeclared {
		t.Fatal("a create with explicit zeros must store declared")
	}

	// The stored rows confirm what the views report.
	var emptyRow, freeRow model.ModelCandidate
	if err := db.First(&emptyRow, empty.ID).Error; err != nil {
		t.Fatalf("load empty row: %v", err)
	}
	if emptyRow.PricesDeclared || emptyRow.ProbeOwed || emptyRow.InputPrice != 0 || emptyRow.OutputPrice != 0 {
		t.Fatalf("empty-price create stored the wrong state: %+v", emptyRow)
	}
	if err := db.First(&freeRow, free.ID).Error; err != nil {
		t.Fatalf("load free row: %v", err)
	}
	if !freeRow.PricesDeclared || freeRow.ProbeOwed {
		t.Fatalf("explicit-zero create stored the wrong state: %+v", freeRow)
	}
}

// The test-and-create path derives the same fact; a disabled save with
// omitted prices lands undeclared and owes nothing.
func TestTestAndCreateDerivesPricesDeclared(t *testing.T) {
	svc, db, providerSvc, _ := newPriceDeclarationTestService(t)
	now := time.Now().UTC()
	p := seedEnabledProviderForModelTest(t, providerSvc, "prov-test-create")

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "test-create-derive"}, now)
	if err != nil {
		t.Fatalf("seed model: %v", err)
	}
	result, err := svc.TestAndCreateCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p.ID,
	}, now)
	if err != nil {
		t.Fatalf("TestAndCreateCandidate failed: %v", err)
	}
	if !result.Created {
		t.Fatalf("expected the candidate created, got %+v", result)
	}
	if result.Candidate.PricesDeclared {
		t.Fatal("a test-and-create with every price omitted must store undeclared")
	}
	if result.Candidate.ProbeOwed {
		t.Fatal("a test-and-create row must not owe the queue anything")
	}
	var row model.ModelCandidate
	if err := db.First(&row, result.Candidate.ID).Error; err != nil {
		t.Fatalf("load created row: %v", err)
	}
	if row.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the inline probe to pass, got %d", row.VerificationStatus)
	}
}

// The update path re-derives by submission-side judgment: submitted slots
// judge by their submission, unsubmitted slots inherit the row's current
// verdict, and a save touching no pricing never re-derives.
func TestUpdateModelCandidateSubmissionSideDerivation(t *testing.T) {
	svc, db, providerSvc, _ := newPriceDeclarationTestService(t)
	now := time.Now().UTC()
	p := seedEnabledProviderForModelTest(t, providerSvc, "prov-update-derive")

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "update-derive"}, now)
	if err != nil {
		t.Fatalf("seed model: %v", err)
	}
	undeclared, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p.ID,
	}, now)
	if err != nil {
		t.Fatalf("seed undeclared candidate: %v", err)
	}
	p2 := seedEnabledProviderForModelTest(t, providerSvc, "prov-update-derive-2")
	declared, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p2.ID, InputPrice: ptrFloat(0), OutputPrice: ptrFloat(0),
	}, now)
	if err != nil {
		t.Fatalf("seed declared candidate: %v", err)
	}

	// From undeclared: one slot submitted is not enough, both are.
	if _, err := svc.UpdateModelCandidate(context.Background(), undeclared.ID, modeladmin.UpdateCandidateInput{
		InputPrice: ptrFloat(3),
	}, now); err != nil {
		t.Fatalf("one-slot update failed: %v", err)
	}
	oneSlot := loadCandidate(t, db, undeclared.ID)
	if oneSlot.PricesDeclared {
		t.Fatal("submitting only the input slot must not declare the row")
	}
	if _, err := svc.UpdateModelCandidate(context.Background(), undeclared.ID, modeladmin.UpdateCandidateInput{
		InputPrice: ptrFloat(3), OutputPrice: ptrFloat(4),
	}, now); err != nil {
		t.Fatalf("two-slot update failed: %v", err)
	}
	if !loadCandidate(t, db, undeclared.ID).PricesDeclared {
		t.Fatal("submitting both slots must declare the row")
	}

	// From declared: an unrelated edit flips nothing in either direction.
	if _, err := svc.UpdateModelCandidate(context.Background(), declared.ID, modeladmin.UpdateCandidateInput{
		MaxOutput: ptrInt(4096),
	}, now); err != nil {
		t.Fatalf("unrelated update failed: %v", err)
	}
	if !loadCandidate(t, db, declared.ID).PricesDeclared {
		t.Fatal("an unrelated edit must not undeclare the row")
	}
	// A single-slot submission on a declared row inherits the other slot.
	if _, err := svc.UpdateModelCandidate(context.Background(), declared.ID, modeladmin.UpdateCandidateInput{
		OutputPrice: ptrFloat(7),
	}, now); err != nil {
		t.Fatalf("single-slot update failed: %v", err)
	}
	if !loadCandidate(t, db, declared.ID).PricesDeclared {
		t.Fatal("a declared row's unsubmitted slot counts as filled")
	}
}

// A billing-mode switch starts the declared derivation over: the stored
// verdict was read from the slots the OLD mode prices with, so inheriting it
// across the switch would hand a declared token row's fact to an audio row
// with no character price — and the next import's requeue would re-arm that
// row, letting a pass enable a mapping its missing price cannot support.
// Within one mode the inheritance is untouched.
func TestUpdateModelCandidateModeSwitchResetsDeclaredInheritance(t *testing.T) {
	svc, db, providerSvc, _ := newPriceDeclarationTestService(t)
	now := time.Now().UTC()

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "switch-inherit"}, now)
	if err != nil {
		t.Fatalf("seed model: %v", err)
	}
	p2 := seedEnabledProviderForModelTest(t, providerSvc, "prov-switch-inherit-2")
	declared, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p2.ID, InputPrice: ptrFloat(0), OutputPrice: ptrFloat(0),
	}, now)
	if err != nil {
		t.Fatalf("seed declared candidate: %v", err)
	}
	if !loadCandidate(t, db, declared.ID).PricesDeclared {
		t.Fatal("seed failed: the token row must start declared")
	}

	// The exploit shape: a declared token row PATCHed to audio mode with the
	// character price omitted lands undeclared.
	if _, err := svc.UpdateModelCandidate(context.Background(), declared.ID, modeladmin.UpdateCandidateInput{
		BillingMode: ptrString(model.BillingModeAudio),
	}, now); err != nil {
		t.Fatalf("audio-mode switch failed: %v", err)
	}
	switched := loadCandidate(t, db, declared.ID)
	if switched.BillingMode != model.BillingModeAudio || switched.PricesDeclared {
		t.Fatalf("token->audio without a character price must land undeclared, got mode=%s declared=%v",
			switched.BillingMode, switched.PricesDeclared)
	}

	// And the fix closes the exploit's second half: the next import's
	// requeue must leave the switched row disarmed, so a pass cannot enable
	// a mapping with no character price.
	requeued, err := svc.ImportProviderModels(p2.ID, []modeladmin.ImportModelItem{
		{ProviderModelName: "switch-inherit"},
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("requeuing import failed: %v", err)
	}
	if requeued.Items[0].Status != modeladmin.ImportStatusSkipped || requeued.Items[0].CandidateID != declared.ID {
		t.Fatalf("expected an exists skip requeuing the switched row, got %+v", requeued.Items[0])
	}
	if err := svc.ProbeQueuedCandidate(context.Background(), declared.ID, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}
	settled := loadCandidate(t, db, declared.ID)
	if settled.AutoEnableOnPass || settled.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("the requeued unpriced audio row must stay disarmed through a pass, got armed=%v status=%d",
			settled.AutoEnableOnPass, settled.ManagementStatus)
	}

	// Same mode, values resubmitted as-is: the inheritance does not flip.
	p3 := seedEnabledProviderForModelTest(t, providerSvc, "prov-switch-inherit-3")
	sameMode, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p3.ID, InputPrice: ptrFloat(0), OutputPrice: ptrFloat(0),
	}, now)
	if err != nil {
		t.Fatalf("seed same-mode candidate: %v", err)
	}
	if _, err := svc.UpdateModelCandidate(context.Background(), sameMode.ID, modeladmin.UpdateCandidateInput{
		BillingMode: ptrString(model.BillingModeToken),
		OutputPrice: ptrFloat(0),
	}, now); err != nil {
		t.Fatalf("same-mode save failed: %v", err)
	}
	if !loadCandidate(t, db, sameMode.ID).PricesDeclared {
		t.Fatal("a same-mode save must keep the stored verdict")
	}
}

// An explicit disable (status toggle or a whole-record resend disabled)
// cancels the probe debt; a retarget clears it with the verification reset.
func TestDisableAndRetargetCancelProbeOwed(t *testing.T) {
	svc, db, providerSvc, _ := newPriceDeclarationTestService(t)
	now := time.Now().UTC()
	p := seedEnabledProviderForModelTest(t, providerSvc, "prov-debt-cancel")

	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "debt-cancel"}, now)
	if err != nil {
		t.Fatalf("seed model: %v", err)
	}
	toggled, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p.ID, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
	}, now)
	if err != nil {
		t.Fatalf("seed toggled candidate: %v", err)
	}
	p2 := seedEnabledProviderForModelTest(t, providerSvc, "prov-debt-cancel-2")
	resend, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p2.ID, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
	}, now)
	if err != nil {
		t.Fatalf("seed resend candidate: %v", err)
	}
	p3 := seedEnabledProviderForModelTest(t, providerSvc, "prov-debt-cancel-3")
	retarget, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: p3.ID, InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
	}, now)
	if err != nil {
		t.Fatalf("seed retarget candidate: %v", err)
	}

	// The debt only exists where a flow promised a probe, so seed it directly
	// — the shapes under test are the settled ones the queue must respect.
	if err := db.Model(&model.ModelCandidate{}).Where("id IN ?", []uint{toggled.ID, resend.ID, retarget.ID}).
		Update("probe_owed", true).Error; err != nil {
		t.Fatalf("seed debts: %v", err)
	}

	if err := svc.SetCandidateStatus(toggled.ID, false, now); err != nil {
		t.Fatalf("status disable failed: %v", err)
	}
	if loadCandidate(t, db, toggled.ID).ProbeOwed {
		t.Fatal("a status-toggle disable must cancel the probe debt")
	}

	if _, err := svc.UpdateModelCandidate(context.Background(), resend.ID, modeladmin.UpdateCandidateInput{
		InputPrice: ptrFloat(1), OutputPrice: ptrFloat(2),
		ManagementStatus: ptrInt(model.ModelCandidateStatusDisabled),
	}, now); err != nil {
		t.Fatalf("disabled resend failed: %v", err)
	}
	if loadCandidate(t, db, resend.ID).ProbeOwed {
		t.Fatal("a whole-record disabled resend must cancel the probe debt")
	}

	if _, err := svc.UpdateModelCandidate(context.Background(), retarget.ID, modeladmin.UpdateCandidateInput{
		ProviderModelName: ptrString("fresh-target"),
	}, now); err != nil {
		t.Fatalf("retarget failed: %v", err)
	}
	row := loadCandidate(t, db, retarget.ID)
	if row.ProbeOwed {
		t.Fatal("a retarget must clear the probe debt with the verification reset")
	}
	if row.VerificationStatus != model.ModelVerificationStatusUntested || row.LastTestedAt != nil {
		t.Fatalf("the retargeted row must read untested and unstamped, got %d/%v", row.VerificationStatus, row.LastTestedAt)
	}
}
