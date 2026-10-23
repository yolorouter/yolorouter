package database

import (
	"testing"

	"github.com/yolorouter/yolorouter/migrations"
)

// Migration 00059 adds the two derived state columns (prices_declared,
// probe_owed) and disarms rows whose pricing or output declaration must not
// auto-enable. The replay below seeds the six legacy row shapes the upgrade
// meets in the wild and re-applies, so the backfill is exercised against real
// rows — the zero-flip property (no stored state changes across the upgrade
// beyond the two new columns and the deliberate disarm) only proves itself
// there.

func TestMigration00059CandidatePriceDeclarationBackfill(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 58); err != nil {
		t.Fatalf("RollbackTo(58) to stage pre-migration data failed: %v", err)
	}

	seed := func(t *testing.T, id int64, name, outputModalities string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO models (id, name, management_status, output_modalities, created_at, updated_at)
			VALUES (?, ?, 1, ?, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`, id, name, outputModalities); err != nil {
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
		autoEnable := 0
		if armed {
			autoEnable = 1
		}
		if _, err := db.Exec(`INSERT INTO model_candidates
			(id, model_id, provider_id, provider_model_name, input_price, output_price,
			 cache_write_price, cache_read_price, max_output, billing_mode, audio_unit_price,
			 management_status, sort_order, verification_status, last_tested_at,
			 auto_enable_on_pass, armed_at, created_at, updated_at, price_updated_at)
			VALUES (?, ?, 1, ?, ?, ?, NULL, NULL, 0, ?, ?,
			 2, 1, 0, NULL, ?, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
			id, modelID, upstream, input, output, billingMode, audio, autoEnable); err != nil {
			t.Fatalf("seed candidate %d: %v", id, err)
		}
	}
	// 1. A token row with all four prices zero, armed, untested, unstamped:
	// the shape every pre-redesign import left behind for an unpriced model.
	candidate(t, 1, 1, "text-model", 0, 0, nil, "token", true)
	// 2. A token row with prices, armed, untested, unstamped.
	candidate(t, 2, 2, "priced-text-model", 2, 8, nil, "token", true)
	// 3. An audio row whose character price is set but whose token slots are
	// zero — the token slots were its stored price state, and they read as
	// unpriced exactly as they always did.
	candidate(t, 3, 3, "audio-model", 0, 0, 12.5, "audio", true)
	// 4. An image-exclusive model's row with zero token prices (per-image
	// billing keeps the token slots inert), armed.
	candidate(t, 4, 4, "image-model", 0, 0, nil, "image", true)
	// 5. A video-exclusive model's row, same shape, armed.
	candidate(t, 5, 5, "video-model", 0, 0, nil, "video", true)
	// 6. A manually created row saved as disabled without probing — never
	// armed, deliberately left unprobed.
	candidate(t, 6, 6, "manual-model", 1, 2, nil, "token", false)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}

	readBack := func(t *testing.T, id int64) (declared, owed, armed bool) {
		t.Helper()
		if err := db.QueryRow(`SELECT prices_declared, probe_owed, auto_enable_on_pass FROM model_candidates WHERE id = ?`, id).
			Scan(&declared, &owed, &armed); err != nil {
			t.Fatalf("read candidate %d: %v", id, err)
		}
		return declared, owed, armed
	}

	// Zero-flip table: the six legacy shapes and what the upgrade must leave
	// behind. probe_owed is computed from the ORIGINAL armed state, so the
	// disarmed rows are still owed the probe their queue entry promised.
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

	// The stored prices are untouched by the backfill: a requeue may never
	// reprice, and neither may the upgrade.
	var input, output float64
	if err := db.QueryRow(`SELECT input_price, output_price FROM model_candidates WHERE id = 2`).Scan(&input, &output); err != nil {
		t.Fatalf("read priced candidate: %v", err)
	}
	if input != 2 || output != 8 {
		t.Fatalf("backfill disturbed stored prices: %v/%v", input, output)
	}

	// Down drops exactly the two columns.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 58); err != nil {
		t.Fatalf("RollbackTo(58) after the backfill failed: %v", err)
	}
	for _, col := range []string{"prices_declared", "probe_owed"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('model_candidates') WHERE name = ?", col).Scan(&n); err != nil {
			t.Fatalf("probe column %s: %v", col, err)
		}
		if n != 0 {
			t.Fatalf("down migration left column %s behind", col)
		}
	}
}
