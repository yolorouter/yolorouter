package database

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/yolorouter/yolorouter/migrations"
)

// newForeignKeyDB opens a file-backed SQLite database with foreign-key
// enforcement switched on via the same per-connection pragma the production
// DSN uses. The plain ":memory:" helper leaves enforcement off, which would
// make every assertion in this file about constraint behaviour vacuous.
func newForeignKeyDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "fk.db") + "?_pragma=foreign_keys(1)"
	return newTestDB(t, dsn)
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func insertProvider(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO providers (name, provider_type, base_url, created_at, updated_at)
		 VALUES (?, 'openai', 'https://api.example.com', '2026-01-02 03:04:05', '2026-01-02 03:04:05')`,
		name,
	)
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("provider id: %v", err)
	}
	return id
}

func insertMinimalRequestLog(t *testing.T, db *sql.DB, requestID string, providerID int64) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO request_logs (request_id, model_name, provider_id, status_code, created_at)
		 VALUES (?, 'test-model', ?, 200, '2026-01-02 03:04:05')`,
		requestID, providerID,
	)
	if err != nil {
		t.Fatalf("insert request log: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("request log id: %v", err)
	}
	return id
}

// TestDeletingAProviderLeavesItsRequestLogsBehind pins the schema contract
// behind provider deletion: request_logs rows are history, not children —
// deleting the provider they point at must succeed and leave every row
// untouched, provider_id value included, so per-provider aggregates keep
// working after the delete. The provider_keys constraint doubles as a
// liveness guard: the same connection must still reject deleting a provider
// that has keys, proving enforcement is on and the request_logs outcome is
// a schema fact rather than a disabled-pragma artifact.
func TestDeletingAProviderLeavesItsRequestLogsBehind(t *testing.T) {
	db := newForeignKeyDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	keyed := insertProvider(t, db, "keyed-provider")
	mustExec(t, db,
		`INSERT INTO provider_keys (provider_id, label, encrypted_key, key_prefix, sort_order,
		    test_model, authorized_destination_version, created_at, updated_at)
		 VALUES (?, 'k1', 'enc', 'sk-', 1, 'test-model', 1, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`,
		keyed,
	)
	if _, err := db.Exec(`DELETE FROM providers WHERE id = ?`, keyed); err == nil {
		t.Fatal("deleting a provider that still has keys succeeded — foreign keys are not enforced, the test setup is broken")
	}

	logged := insertProvider(t, db, "logged-provider")
	logID := insertMinimalRequestLog(t, db, "req-history", logged)

	if _, err := db.Exec(`DELETE FROM providers WHERE id = ?`, logged); err != nil {
		t.Fatalf("deleting a provider referenced only by request_logs must succeed, got: %v", err)
	}

	var gotProviderID int64
	if err := db.QueryRow(`SELECT provider_id FROM request_logs WHERE id = ?`, logID).Scan(&gotProviderID); err != nil {
		t.Fatalf("request log row vanished after provider delete: %v", err)
	}
	if gotProviderID != logged {
		t.Fatalf("request log provider_id changed after provider delete: got %d, want %d", gotProviderID, logged)
	}
}

// requestLogIndexes returns the names of all indexes on request_logs.
func requestLogIndexes(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'request_logs'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexes: %v", err)
	}
	return names
}

// TestMigration00036RebuildCarriesRequestLogRowsAndIndexes replays the real
// upgrade path: build the pre-00036 schema, write a fully-populated
// request_logs row under the old constraint, migrate forward, and assert
// the rebuild carried every column value, kept every index, and did not
// reset the autoincrement counter. The downgrade direction is replayed too:
// rolling back must restore the provider_id constraint (enforced, not just
// declared) and carry the rows again.
func TestMigration00036RebuildCarriesRequestLogRowsAndIndexes(t *testing.T) {
	db := newForeignKeyDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 35); err != nil {
		t.Fatalf("RollbackTo(35) failed: %v", err)
	}

	providerID := insertProvider(t, db, "upgraded-provider")
	res, err := db.Exec(
		`INSERT INTO request_logs (
		    request_id, api_key_id, model_name, provider_id, is_stream, status_code,
		    input_tokens, output_tokens, cost_micros, cost_known, fail_reason,
		    attempts, attempts_detail, duration_ms, created_at,
		    cache_write_tokens, cache_read_tokens,
		    cache_read_saved_micros, cache_write_extra_micros,
		    compress_estimated_tokens_saved, compress_estimated_cost_saved_micros,
		    compress_skip_reason, compressors_applied,
		    request_path, upstream_url, facts_json, source, parent_request_id,
		    user_id, settled_input_price, settled_output_price,
		    settled_cache_write_price, settled_cache_read_price
		 ) VALUES (
		    'req-full', NULL, 'model-x', ?, 1, 200,
		    11, 22, 33, 1, 'boom',
		    2, '[{"provider_name":"upgraded-provider"}]', 44, '2026-01-02 03:04:05',
		    5, 6,
		    7, 8,
		    9, 10,
		    'skip-reason', 'trim',
		    '/v1/chat/completions', 'https://up.example.com', '{"f":1}', 'vision_fallback', 'req-parent',
		    NULL, 1.5, 2.5,
		    3.5, 4.5
		 )`,
		providerID,
	)
	if err != nil {
		t.Fatalf("insert full request log at version 35: %v", err)
	}
	fullID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("full row id: %v", err)
	}

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("migrating forward over 00036 failed: %v", err)
	}

	// Every column of the seeded row must have survived the rebuild. A
	// forgotten column in the copy would zero or default that field and
	// drop this count to 0.
	var carried int
	err = db.QueryRow(
		`SELECT COUNT(*) FROM request_logs WHERE id = ? AND request_id = 'req-full'
		    AND api_key_id IS NULL AND model_name = 'model-x' AND provider_id = ?
		    AND is_stream = 1 AND status_code = 200 AND input_tokens = 11
		    AND output_tokens = 22 AND cost_micros = 33 AND cost_known = 1
		    AND fail_reason = 'boom' AND attempts = 2
		    AND attempts_detail = '[{"provider_name":"upgraded-provider"}]'
		    AND duration_ms = 44 AND created_at = '2026-01-02 03:04:05'
		    AND cache_write_tokens = 5 AND cache_read_tokens = 6
		    AND cache_read_saved_micros = 7 AND cache_write_extra_micros = 8
		    AND compress_estimated_tokens_saved = 9 AND compress_estimated_cost_saved_micros = 10
		    AND compress_skip_reason = 'skip-reason' AND compressors_applied = 'trim'
		    AND request_path = '/v1/chat/completions' AND upstream_url = 'https://up.example.com'
		    AND facts_json = '{"f":1}' AND source = 'vision_fallback' AND parent_request_id = 'req-parent'
		    AND user_id IS NULL AND settled_input_price = 1.5 AND settled_output_price = 2.5
		    AND settled_cache_write_price = 3.5 AND settled_cache_read_price = 4.5`,
		fullID, providerID,
	).Scan(&carried)
	if err != nil {
		t.Fatalf("verify carried row: %v", err)
	}
	if carried != 1 {
		t.Fatalf("seeded request_logs row did not survive the 00036 rebuild intact")
	}

	indexes := requestLogIndexes(t, db)
	for _, want := range []string{
		"idx_request_logs_api_key_id",
		"idx_request_logs_created_at",
		"idx_request_logs_model_name",
		"idx_request_logs_created_at_status",
		"idx_request_logs_request_id",
		"idx_request_logs_user_id",
		"idx_request_logs_cache_metering_evidence",
		"idx_request_logs_cache_savings_evidence",
	} {
		if !indexes[want] {
			t.Fatalf("index %s missing after the 00036 rebuild (got %v)", want, indexes)
		}
	}

	// The autoincrement counter must carry over: a fresh insert may not
	// reuse the copied row's id.
	freshID := insertMinimalRequestLog(t, db, "req-after-rebuild", providerID)
	if freshID <= fullID {
		t.Fatalf("autoincrement reset by the rebuild: fresh id %d not above carried id %d", freshID, fullID)
	}

	// Downgrade replay: rolling back rebuilds the constrained table, carries
	// the rows, and actually enforces the restored foreign key.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 35); err != nil {
		t.Fatalf("RollbackTo(35) after upgrade failed: %v", err)
	}
	var afterDown int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&afterDown); err != nil {
		t.Fatalf("count after downgrade: %v", err)
	}
	if afterDown != 2 {
		t.Fatalf("expected 2 request_logs rows after downgrade, got %d", afterDown)
	}
	if _, err := db.Exec(`DELETE FROM providers WHERE id = ?`, providerID); err == nil {
		t.Fatal("downgrade did not restore the provider_id foreign key: provider delete succeeded despite referencing logs")
	}

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying 00036 after downgrade failed: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM providers WHERE id = ?`, providerID); err != nil {
		t.Fatalf("provider delete after re-upgrade must succeed, got: %v", err)
	}
}
