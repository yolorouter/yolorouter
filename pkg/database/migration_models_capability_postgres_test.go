package database

import (
	"database/sql"
	"testing"

	"github.com/yolorouter/yolorouter/migrations"
)

// Postgres twin of TestMigration00056ModelsCapabilityMetadataBackfill: the
// backfill UPDATEs and the BOOLEAN/DOUBLE PRECISION column shapes only prove
// themselves on this backend — SQLite's loose typing would happily store a
// BOOLEAN where Postgres rejects the DDL outright. Gated on TEST_POSTGRES_DSN
// like the rest of the Postgres suite.
func TestPostgresMigration00056ModelsCapabilityMetadataBackfill(t *testing.T) {
	db := newTestPostgresDB(t)
	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if err := RollbackTo(db, "postgres", migrations.PostgresFS, "postgres", 55); err != nil {
		t.Fatalf("RollbackTo(55) to stage pre-migration data failed: %v", err)
	}

	seed := func(t *testing.T, id int, name string, imageInput any) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO models (id, name, management_status, supports_image_input, created_at, updated_at)
			VALUES ($1, $2, 1, $3, '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00')`, id, name, imageInput); err != nil {
			t.Fatalf("seed model %d: %v", id, err)
		}
	}
	seed(t, 1, "eyes", true)
	seed(t, 2, "text-only", false)
	seed(t, 3, "mystery", nil)

	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}

	readBack := func(t *testing.T, id int) (inputModalities sql.NullString, imageInput sql.NullBool) {
		t.Helper()
		if err := db.QueryRow(`SELECT input_modalities, supports_image_input FROM models WHERE id = $1`, id).
			Scan(&inputModalities, &imageInput); err != nil {
			t.Fatalf("read model %d: %v", id, err)
		}
		return inputModalities, imageInput
	}

	if im, img := readBack(t, 1); im.String != `["text","image"]` || !img.Valid || !img.Bool {
		t.Fatalf("declared-able row: input_modalities=%+v supports_image_input=%+v, want [\"text\",\"image\"] and true", im, img)
	}
	if im, img := readBack(t, 2); im.String != `["text"]` || !img.Valid || img.Bool {
		t.Fatalf("declared-unable row: input_modalities=%+v supports_image_input=%+v, want [\"text\"] and false", im, img)
	}
	if im, img := readBack(t, 3); im.Valid || img.Valid {
		t.Fatalf("undeclared row: input_modalities=%+v supports_image_input=%+v, want both NULL", im, img)
	}

	// The tri-state bool columns must be real BOOLEANs — the runtime binds
	// them as Go bools (model.Model.SupportsTools), and an integer-typed
	// column would make every `supports_tools = $1` predicate fail with a
	// type error on this backend only.
	var toolsCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM models WHERE supports_tools IS NOT DISTINCT FROM $1`, nil).Scan(&toolsCount); err != nil {
		t.Fatalf("null-bound tri-state predicate failed (column type mismatch?): %v", err)
	}
	if toolsCount != 3 {
		t.Fatalf("expected all 3 rows undeclared on supports_tools, got %d", toolsCount)
	}

	// temperature must be DOUBLE PRECISION, not a scaled NUMERIC:
	// model.Model.Temperature is a *float64 and the sqlite tree stores an
	// IEEE double, so a fixed scale here would silently round a 3-decimal
	// declaration (0.705 → 0.71) on this backend only. The repository
	// round-trip (TestModelCapabilityMetadataRoundTripOnPostgres) pins the
	// behaviour with a 3-decimal value; this pins the DDL itself.
	var temperatureType string
	if err := db.QueryRow(`SELECT data_type FROM information_schema.columns
		WHERE table_name = 'models' AND column_name = 'temperature'`).Scan(&temperatureType); err != nil {
		t.Fatalf("read temperature column type: %v", err)
	}
	if temperatureType != "double precision" {
		t.Fatalf("temperature column type = %q, want double precision (a scaled NUMERIC would round 3-decimal declarations on this backend only)", temperatureType)
	}

	// An old-binary insert omits the new columns; they must all default to
	// NULL rather than fail a NOT NULL constraint mid-rolling-upgrade.
	if _, err := db.Exec(`INSERT INTO models (id, name, management_status, created_at, updated_at)
		VALUES (4, 'legacy-row', 1, now(), now())`); err != nil {
		t.Fatalf("old-binary insert failed: %v", err)
	}

	if err := RollbackTo(db, "postgres", migrations.PostgresFS, "postgres", 55); err != nil {
		t.Fatalf("RollbackTo(55) with rows present failed: %v", err)
	}
	for _, col := range []string{"context_length", "input_modalities", "supports_tools", "supports_reasoning", "temperature"} {
		if _, err := db.Exec("SELECT " + col + " FROM models WHERE id = 1"); err == nil {
			t.Fatalf("Down left %s behind", col)
		}
	}
	var img sql.NullBool
	if err := db.QueryRow(`SELECT supports_image_input FROM models WHERE id = 1`).Scan(&img); err != nil || !img.Valid || !img.Bool {
		t.Fatalf("Down must keep supports_image_input intact, got %+v err=%v", img, err)
	}
	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("re-applying after rollback failed: %v", err)
	}
}
