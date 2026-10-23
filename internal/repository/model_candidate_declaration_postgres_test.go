package repository

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
)

// The price-declaration columns' Postgres arm. Every repository test runs on
// SQLite; this drives the one statement whose correlated-subquery UPDATE the
// conditional requeue re-arm introduced — plus the debt/promise key split in
// the recovery query — against a real Postgres. Gated on TEST_POSTGRES_DSN
// like the other Postgres repository suites.

func TestCandidatePriceDeclarationRoundTripOnPostgres(t *testing.T) {
	db := newPostgresRepositoryDB(t)
	now := time.Now().UTC()

	if err := db.Exec(`INSERT INTO providers (id, name, base_url, created_at, updated_at)
		VALUES (900, 'prov-pg', 'https://example.invalid', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00'),
		       (901, 'prov-pg-2', 'https://example.invalid', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00'),
		       (902, 'prov-pg-3', 'https://example.invalid', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00')`).Error; err != nil {
		t.Fatalf("seed providers: %v", err)
	}

	m := &model.Model{
		Name: "declaration-pg", ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover, OutputModalities: `["text"]`,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := CreateModel(db, m); err != nil {
		t.Fatalf("create model: %v", err)
	}
	imageModel := &model.Model{
		Name: "declaration-pg-image", ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover, OutputModalities: `["image"]`,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := CreateModel(db, imageModel); err != nil {
		t.Fatalf("create image model: %v", err)
	}

	sortOrder := 0
	candidate := func(t *testing.T, providerID uint, parent *model.Model, declared, armed bool) *model.ModelCandidate {
		t.Helper()
		sortOrder++
		c := &model.ModelCandidate{
			ModelID: parent.ID, ProviderID: providerID, ProviderModelName: parent.Name,
			BillingMode:    model.BillingModeToken,
			PricesDeclared: declared, ProbeOwed: true,
			AutoEnableOnPass: armed, ArmedAt: &now,
			ManagementStatus:   model.ModelCandidateStatusDisabled,
			VerificationStatus: model.ModelVerificationStatusUntested,
			SortOrder:          sortOrder,
			CreatedAt:          now, UpdatedAt: now, PriceUpdatedAt: now,
		}
		if err := CreateModelCandidate(db, c); err != nil {
			t.Fatalf("create candidate: %v", err)
		}
		return c
	}

	declaredText := candidate(t, 900, m, true, true)
	unpricedText := candidate(t, 901, m, false, false)
	declaredImage := candidate(t, 902, imageModel, true, true)

	// The conditional requeue re-arm: declared rows on models without
	// image/video output re-arm, everything else requeues disarmed, the debt
	// restores, and the stored prices and declared fact stay untouched.
	if err := ClearCandidatesProbeResidue(db, []uint{declaredText.ID, unpricedText.ID, declaredImage.ID}, "rq-pg-1", now.Add(time.Second)); err != nil {
		t.Fatalf("ClearCandidatesProbeResidue: %v", err)
	}
	for _, tc := range []struct {
		c                                 *model.ModelCandidate
		wantArmed, wantDeclared, wantOwed bool
	}{
		{declaredText, true, true, true},
		{unpricedText, false, false, true},
		{declaredImage, false, true, true},
	} {
		got, err := FindModelCandidateByID(db, tc.c.ID)
		if err != nil {
			t.Fatalf("read candidate %d: %v", tc.c.ID, err)
		}
		if got.AutoEnableOnPass != tc.wantArmed || got.PricesDeclared != tc.wantDeclared || got.ProbeOwed != tc.wantOwed {
			t.Errorf("candidate %d: armed=%v declared=%v owed=%v, want %v/%v/%v",
				tc.c.ID, got.AutoEnableOnPass, got.PricesDeclared, got.ProbeOwed, tc.wantArmed, tc.wantDeclared, tc.wantOwed)
		}
		if got.InputPrice != 0 || got.OutputPrice != 0 {
			t.Errorf("candidate %d: the requeue must never reprice, got %v/%v", tc.c.ID, got.InputPrice, got.OutputPrice)
		}
		// The disarmed row's arming history is untouchable: the requeue
		// judged it not re-armed, so its seeded non-NULL armed_at must pass
		// through unchanged — neither nulled nor restamped with the requeue
		// time (both would fabricate a false arming history). Compared at
		// the column's microsecond precision: the seed's nanoseconds do not
		// survive Postgres storage.
		if tc.c == unpricedText && (got.ArmedAt == nil || !got.ArmedAt.Equal(tc.c.ArmedAt.Truncate(time.Microsecond))) {
			t.Errorf("candidate %d: disarmed requeue must keep the seeded armed_at %v, got %v", tc.c.ID, tc.c.ArmedAt, got.ArmedAt)
		}
	}

	// The recovery query's key split: after settling two of the rows, the
	// still-owed untested image row is collected on the PROBE-OWED leg and
	// the settled-but-armed text row on the ARMED ENABLE leg — while the
	// settled unarmed row is collected by neither.
	settled := now.Add(2 * time.Second)
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", declaredText.ID).
		Updates(map[string]interface{}{
			"verification_status": model.ModelVerificationStatusPassed,
			"last_tested_at":      settled,
			"probe_owed":          false,
			"armed_at":            settled,
			"updated_at":          settled,
		}).Error; err != nil {
		t.Fatalf("settle armed text candidate: %v", err)
	}
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", unpricedText.ID).
		Updates(map[string]interface{}{
			"verification_status": model.ModelVerificationStatusPassed,
			"last_tested_at":      settled,
			"probe_owed":          false,
		}).Error; err != nil {
		t.Fatalf("settle unarmed text candidate: %v", err)
	}
	ids, err := ListUnprobedCandidateIDs(db)
	if err != nil {
		t.Fatalf("ListUnprobedCandidateIDs: %v", err)
	}
	want := map[uint]bool{declaredText.ID: true, declaredImage.ID: true}
	if len(ids) != len(want) {
		t.Fatalf("expected %d recovered ids, got %v", len(want), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected recovery id %d", id)
		}
	}
}
