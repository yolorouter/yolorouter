package database

import (
	"database/sql"
	"testing"

	"github.com/yolorouter/yolorouter/migrations"
)

// Migration 00056 adds the five capability-metadata columns and backfills
// input_modalities from the existing supports_image_input declaration. The
// replay below seeds the three pre-upgrade states (true / false / NULL) and
// re-applies, so the backfill is exercised against real rows — the empty-tree
// up/down test cannot catch a broken UPDATE.

func TestMigration00056ModelsCapabilityMetadataBackfill(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 55); err != nil {
		t.Fatalf("RollbackTo(55) to stage pre-migration data failed: %v", err)
	}

	seed := func(t *testing.T, id int, name string, imageInput any) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO models (id, name, management_status, supports_image_input, created_at, updated_at)
			VALUES (?, ?, 1, ?, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`, id, name, imageInput); err != nil {
			t.Fatalf("seed model %d: %v", id, err)
		}
	}
	seed(t, 1, "eyes", 1)
	seed(t, 2, "text-only", 0)
	seed(t, 3, "mystery", nil)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}

	readBack := func(t *testing.T, id int) (inputModalities sql.NullString, imageInput sql.NullInt64) {
		t.Helper()
		if err := db.QueryRow(`SELECT input_modalities, supports_image_input FROM models WHERE id = ?`, id).
			Scan(&inputModalities, &imageInput); err != nil {
			t.Fatalf("read model %d: %v", id, err)
		}
		return inputModalities, imageInput
	}

	// The backfill's three states: declared-able → array containing image,
	// declared-unable → a non-empty array without image, undeclared → NULL.
	// The old column itself must be untouched — the migration derives the new
	// column FROM it, never the other way round.
	if im, img := readBack(t, 1); im.String != `["text","image"]` || !img.Valid || img.Int64 != 1 {
		t.Fatalf("declared-able row: input_modalities=%+v supports_image_input=%+v, want [\"text\",\"image\"] and 1", im, img)
	}
	if im, img := readBack(t, 2); im.String != `["text"]` || !img.Valid || img.Int64 != 0 {
		t.Fatalf("declared-unable row: input_modalities=%+v supports_image_input=%+v, want [\"text\"] and 0", im, img)
	}
	if im, img := readBack(t, 3); im.Valid || img.Valid {
		t.Fatalf("undeclared row: input_modalities=%+v supports_image_input=%+v, want both NULL", im, img)
	}

	// A binary predating this migration inserts without naming the new
	// columns; they are all nullable with no default, so the insert must
	// succeed and leave every new column NULL.
	if _, err := db.Exec(`INSERT INTO models (id, name, management_status, created_at, updated_at)
		VALUES (4, 'legacy-row', 1, '2026-01-02 00:00:00', '2026-01-02 00:00:00')`); err != nil {
		t.Fatalf("old-binary insert failed: %v", err)
	}
	for _, col := range []string{"context_length", "input_modalities", "supports_tools", "supports_reasoning", "temperature"} {
		var notNull int
		if err := db.QueryRow("SELECT " + col + " IS NOT NULL FROM models WHERE id = 4").Scan(&notNull); err != nil {
			t.Fatalf("probe %s: %v", col, err)
		}
		if notNull != 0 {
			t.Fatalf("legacy row's %s is not NULL", col)
		}
	}

	// Down drops exactly the five columns — supports_image_input survives the
	// downgrade — and a rolling-back operator is not stranded.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 55); err != nil {
		t.Fatalf("RollbackTo(55) with rows present failed: %v", err)
	}
	for _, col := range []string{"context_length", "input_modalities", "supports_tools", "supports_reasoning", "temperature"} {
		if _, err := db.Exec("SELECT " + col + " FROM models WHERE id = 1"); err == nil {
			t.Fatalf("Down left %s behind", col)
		}
	}
	var img sql.NullInt64
	if err := db.QueryRow(`SELECT supports_image_input FROM models WHERE id = 1`).Scan(&img); err != nil || !img.Valid || img.Int64 != 1 {
		t.Fatalf("Down must keep supports_image_input intact, got %+v err=%v", img, err)
	}
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying after rollback failed: %v", err)
	}
	version, err := GetCurrentVersion(db, "sqlite")
	if err != nil {
		t.Fatalf("GetCurrentVersion failed: %v", err)
	}
	if want := maxEmbeddedMigrationVersion(t, migrations.SQLiteFS, "sqlite"); version != want {
		t.Fatalf("expected version %d after re-apply, got %d", want, version)
	}
}
