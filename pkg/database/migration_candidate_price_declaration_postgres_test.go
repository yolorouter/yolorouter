package database

import (
	"testing"

	"github.com/yolorouter/yolorouter/migrations"
)

// Postgres twin of TestMigration00059CandidatePriceDeclarationBackfill: the
// correlated EXISTS backfill and the BOOLEAN column shapes only prove
// themselves on this backend — SQLite's loose typing would happily accept a
// statement Postgres rejects outright. Gated on TEST_POSTGRES_DSN like the
// rest of the Postgres suite; the seeded shapes and the expected outcomes are
// the six legacy rows the sqlite twin asserts.
func TestPostgresMigration00059CandidatePriceDeclarationBackfill(t *testing.T) {
	db := newTestPostgresDB(t)
	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if err := RollbackTo(db, "postgres", migrations.PostgresFS, "postgres", 58); err != nil {
		t.Fatalf("RollbackTo(58) to stage pre-migration data failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO providers (id, name, base_url, created_at, updated_at)
		VALUES (1, 'prov-a', 'https://example.invalid', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00')`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	seed := func(t *testing.T, id int64, name, outputModalities string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO models (id, name, management_status, output_modalities, created_at, updated_at)
			VALUES ($1, $2, 1, $3, '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00')`, id, name, outputModalities); err != nil {
			t.Fatalf("seed model %d: %v", id, err)
		}
	}
	seed(t, 1, "text-model", `["text"]`)
	seed(t, 2, "priced-text-model", `["text"]`)
	seed(t, 3, "audio-model", `["audio"]`)
	seed(t, 4, "image-model", `["image"]`)
	seed(t, 5, "video-model", `["video"]`)
	seed(t, 6, "manual-model", `["text"]`)

	candidate := func(t *testing.T, id, modelID int64, upstream string, input, output float64,
		audio any, billingMode string, armed bool) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO model_candidates
			(id, model_id, provider_id, provider_model_name, input_price, output_price,
			 cache_write_price, cache_read_price, max_output, billing_mode, audio_unit_price,
			 management_status, sort_order, verification_status, last_tested_at,
			 auto_enable_on_pass, armed_at, created_at, updated_at, price_updated_at)
			VALUES ($1, $2, 1, $3, $4, $5, NULL, NULL, 0, $6, $7,
			 2, 1, 0, NULL, $8, '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00')`,
			id, modelID, upstream, input, output, billingMode, audio, armed); err != nil {
			t.Fatalf("seed candidate %d: %v", id, err)
		}
	}
	candidate(t, 1, 1, "text-model", 0, 0, nil, "token", true)
	candidate(t, 2, 2, "priced-text-model", 2, 8, nil, "token", true)
	candidate(t, 3, 3, "audio-model", 0, 0, 12.5, "audio", true)
	candidate(t, 4, 4, "image-model", 0, 0, nil, "image", true)
	candidate(t, 5, 5, "video-model", 0, 0, nil, "video", true)
	candidate(t, 6, 6, "manual-model", 1, 2, nil, "token", false)

	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}

	readBack := func(t *testing.T, id int64) (declared, owed, armed bool) {
		t.Helper()
		if err := db.QueryRow(`SELECT prices_declared, probe_owed, auto_enable_on_pass FROM model_candidates WHERE id = $1`, id).
			Scan(&declared, &owed, &armed); err != nil {
			t.Fatalf("read candidate %d: %v", id, err)
		}
		return declared, owed, armed
	}

	for _, tc := range []struct {
		id                                int64
		wantDeclared, wantOwed, wantArmed bool
	}{
		{1, false, true, false}, // unpriced token: badge stays, disarmed, still probed
		{2, true, true, true},   // priced token: fully intact
		{3, false, true, false}, // audio char-priced: token slots read unpriced, disarmed
		{4, true, true, false},  // image-exclusive: declared, disarmed (tiers pending)
		{5, true, true, false},  // video-exclusive: declared, disarmed (tiers pending)
		{6, true, false, false}, // unarmed manual row: not collected by recovery
	} {
		declared, owed, armed := readBack(t, tc.id)
		if declared != tc.wantDeclared || owed != tc.wantOwed || armed != tc.wantArmed {
			t.Errorf("candidate %d: prices_declared=%v probe_owed=%v auto_enable_on_pass=%v, want %v/%v/%v",
				tc.id, declared, owed, armed, tc.wantDeclared, tc.wantOwed, tc.wantArmed)
		}
	}

	var input, output float64
	if err := db.QueryRow(`SELECT input_price, output_price FROM model_candidates WHERE id = 2`).Scan(&input, &output); err != nil {
		t.Fatalf("read priced candidate: %v", err)
	}
	if input != 2 || output != 8 {
		t.Fatalf("backfill disturbed stored prices: %v/%v", input, output)
	}

	if err := RollbackTo(db, "postgres", migrations.PostgresFS, "postgres", 58); err != nil {
		t.Fatalf("RollbackTo(58) after the backfill failed: %v", err)
	}
	for _, col := range []string{"prices_declared", "probe_owed"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
			WHERE table_name = 'model_candidates' AND column_name = $1`, col).Scan(&n); err != nil {
			t.Fatalf("probe column %s: %v", col, err)
		}
		if n != 0 {
			t.Fatalf("down migration left column %s behind", col)
		}
	}
}
