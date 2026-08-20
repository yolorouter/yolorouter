package database

import (
	"database/sql"
	"io/fs"
	"testing"

	// database.go (same package) imports github.com/glebarez/sqlite, which
	// transitively imports github.com/glebarez/go-sqlite and registers the
	// "sqlite" database/sql driver via its init(). Blank-importing
	// modernc.org/sqlite here as well (as the plan's illustrative test does)
	// panics with "sql: Register called twice for driver sqlite" because
	// both packages register the exact same driver name — so we rely on the
	// registration already pulled in transitively instead of duplicating it.

	"github.com/yolorouter/yolorouter/migrations"
)

// newTestDB opens a SQLite database (":memory:" or a file path) for a
// single test and registers its Close via t.Cleanup, so callers don't each
// repeat the open-or-fail-and-defer-close boilerplate.
func newTestDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite %q: %v", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newMemoryDB is newTestDB with the in-memory DSN most tests here want.
func newMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	return newTestDB(t, ":memory:")
}

// maxEmbeddedMigrationVersion adapts the production maxMigrationVersion to
// the fatal-on-error shape its four test call sites (here and in the
// postgres migration tests) all want. The derive-from-FS rationale lives on
// the production function.
func maxEmbeddedMigrationVersion(t *testing.T, migrationsFS fs.FS, dir string) int64 {
	t.Helper()
	v, err := maxMigrationVersion(migrationsFS, dir)
	if err != nil {
		t.Fatalf("derive max migration version from %q: %v", dir, err)
	}
	return v
}

func TestRunMigrationsAppliesBaselineOnSQLite(t *testing.T) {
	db := newMemoryDB(t)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	// the goose metadata table must exist and the version must be at least 1
	// (baseline migration applied)
	var version int64
	row := db.QueryRow("SELECT version_id FROM goose_db_version ORDER BY id DESC LIMIT 1")
	if err := row.Scan(&version); err != nil {
		t.Fatalf("query goose_db_version: %v", err)
	}
	if version < 1 {
		t.Fatalf("expected version >= 1 after baseline migration, got %d", version)
	}
}

func TestRunMigrationsIsIdempotent(t *testing.T) {
	db := newMemoryDB(t)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("first RunMigrations failed: %v", err)
	}
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("second RunMigrations (idempotency check) failed: %v", err)
	}
}

func TestGetCurrentVersionOnFreshSQLiteDB(t *testing.T) {
	db := newMemoryDB(t)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	version, err := GetCurrentVersion(db, "sqlite")
	if err != nil {
		t.Fatalf("GetCurrentVersion failed: %v", err)
	}
	// This asserts the current highest migration version, not just the
	// baseline migration. The expected value is derived from the embedded
	// filenames (see maxEmbeddedMigrationVersion) so it can never go stale
	// when a migration is added.
	want := maxEmbeddedMigrationVersion(t, migrations.SQLiteFS, "sqlite")
	if version != want {
		t.Fatalf("expected version %d after all migrations, got %d", want, version)
	}
}

func TestRunMigrationsAppliesUserAuthTablesOnSQLite(t *testing.T) {
	db := newMemoryDB(t)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	for _, table := range []string{"users", "user_sessions"} {
		var name string
		row := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table)
		if err := row.Scan(&name); err != nil {
			t.Fatalf("table %q not found after migration: %v", table, err)
		}
	}
	// The single-admin tables must be gone after the multi-account
	// migration — code binding to them would only fail at runtime.
	for _, table := range []string{"admins", "admin_sessions"} {
		var name string
		row := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table)
		if err := row.Scan(&name); err == nil {
			t.Fatalf("table %q should have been dropped by the multi-account migration", table)
		}
	}
}

// TestMigration00023CarriesAdminAndSessionsOverToUsers replays a real
// upgrade: a database at the pre-multi-account version with an admin row
// and a live session must come out of the migration with that account as
// an enabled local admin user (same id, same password hash) and the
// session still attached — an upgrade must not log the administrator out
// or, worse, orphan the account.
func TestMigration00023CarriesAdminAndSessionsOverToUsers(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 22); err != nil {
		t.Fatalf("RollbackTo(22) failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO admins (id, username, password_hash, failed_login_count, created_at, updated_at)
		VALUES (7, 'boss', 'bcrypt-hash', 2, '2026-01-01 00:00:00', '2026-01-02 00:00:00')`); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO admin_sessions (id, admin_id, expires_at, created_at)
		VALUES ('session-hash-value', 7, '2027-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-running migrations failed: %v", err)
	}

	var (
		username, passwordHash, role string
		status, failedCount          int
		isLocal                      bool
	)
	row := db.QueryRow(`SELECT username, password_hash, role, status, is_local, failed_login_count FROM users WHERE id = 7`)
	if err := row.Scan(&username, &passwordHash, &role, &status, &isLocal, &failedCount); err != nil {
		t.Fatalf("migrated user row not found: %v", err)
	}
	if username != "boss" || passwordHash != "bcrypt-hash" || failedCount != 2 {
		t.Fatalf("carried-over fields wrong: username=%q hash=%q failed=%d", username, passwordHash, failedCount)
	}
	if role != "admin" || status != 1 || !isLocal {
		t.Fatalf("expected enabled local admin, got role=%q status=%d is_local=%v", role, status, isLocal)
	}

	var sessionUserID int64
	row = db.QueryRow(`SELECT user_id FROM user_sessions WHERE id = 'session-hash-value'`)
	if err := row.Scan(&sessionUserID); err != nil {
		t.Fatalf("migrated session row not found: %v", err)
	}
	if sessionUserID != 7 {
		t.Fatalf("expected session to stay attached to user 7, got %d", sessionUserID)
	}

	// The copied explicit id must not collide with the next generated one.
	res, err := db.Exec(`INSERT INTO users (username, role, status, created_at, updated_at)
		VALUES ('next-user', 'member', 1, '2026-01-03 00:00:00', '2026-01-03 00:00:00')`)
	if err != nil {
		t.Fatalf("insert after migration failed: %v", err)
	}
	nextID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	if nextID <= 7 {
		t.Fatalf("expected the next generated id to be > 7, got %d", nextID)
	}
}

// TestMigration00023DownRestoresSingleAdminSchema proves the rollback is
// a real inverse with data in place: the local account and its session
// come back as the admins/admin_sessions rows they once were, while
// non-local users (unrepresentable in the single-admin schema) are
// dropped rather than corrupting the singleton constraint.
func TestMigration00023DownRestoresSingleAdminSchema(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, role, status, is_local, created_at, updated_at)
		VALUES (3, 'boss', 'bcrypt-hash', 'admin', 1, 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00'),
		       (4, 'ext-member', '', 'member', 1, 0, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO user_sessions (id, user_id, expires_at, created_at)
		VALUES ('local-session', 3, '2027-01-01 00:00:00', '2026-01-01 00:00:00'),
		       ('member-session', 4, '2027-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}

	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 22); err != nil {
		t.Fatalf("RollbackTo(22) failed: %v", err)
	}

	var username, passwordHash string
	row := db.QueryRow(`SELECT username, password_hash FROM admins WHERE id = 3`)
	if err := row.Scan(&username, &passwordHash); err != nil {
		t.Fatalf("rolled-back admin row not found: %v", err)
	}
	if username != "boss" || passwordHash != "bcrypt-hash" {
		t.Fatalf("unexpected rolled-back admin: username=%q hash=%q", username, passwordHash)
	}

	var adminCount, sessionCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admins`).Scan(&adminCount); err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if adminCount != 1 {
		t.Fatalf("expected exactly the local account to survive rollback, got %d admins", adminCount)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_sessions`).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("expected only the local account's session to survive rollback, got %d", sessionCount)
	}
	var sessionAdminID int64
	if err := db.QueryRow(`SELECT admin_id FROM admin_sessions WHERE id = 'local-session'`).Scan(&sessionAdminID); err != nil {
		t.Fatalf("local session not found after rollback: %v", err)
	}
	if sessionAdminID != 3 {
		t.Fatalf("expected local session attached to admin 3, got %d", sessionAdminID)
	}
}

// TestMigration00024BackfillsOwnershipToLocalAdmin replays the ownership
// upgrade: keys and logs created under the single-admin model must come
// out owned by the local account, so the admin's per-user view of history
// equals the pre-upgrade global view. Unauthenticated audit rows (NULL
// api_key_id) also backfill to the local account — all history predates
// multi-account, so it is all the admin's.
func TestMigration00024BackfillsOwnershipToLocalAdmin(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 23); err != nil {
		t.Fatalf("RollbackTo(23) failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, role, status, is_local, created_at, updated_at)
		VALUES (5, 'boss', 'hash', 'admin', 1, 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("seed local admin: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id, key_hash, key_prefix, status, created_at, updated_at)
		VALUES (11, 'kh-1', 'sk-yr-a', 1, '2026-01-02 00:00:00', '2026-01-02 00:00:00')`); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO request_logs (request_id, api_key_id, model_name, status_code, created_at)
		VALUES ('req-1', 11, 'm', 200, '2026-01-03 00:00:00')`); err != nil {
		t.Fatalf("seed request log: %v", err)
	}

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-running migrations failed: %v", err)
	}

	var keyOwner int64
	if err := db.QueryRow(`SELECT user_id FROM api_keys WHERE id = 11`).Scan(&keyOwner); err != nil {
		t.Fatalf("read backfilled key owner: %v", err)
	}
	if keyOwner != 5 {
		t.Fatalf("expected key backfilled to local admin 5, got %d", keyOwner)
	}
	var logOwner int64
	if err := db.QueryRow(`SELECT user_id FROM request_logs WHERE request_id = 'req-1'`).Scan(&logOwner); err != nil {
		t.Fatalf("read backfilled log owner: %v", err)
	}
	if logOwner != 5 {
		t.Fatalf("expected log backfilled to local admin 5, got %d", logOwner)
	}
}

// TestMigration00023EnforcesSingleLocalUser proves the partial unique
// index survives the round trip through goose: two enabled local rows
// must be impossible, while any number of non-local rows is fine.
func TestMigration00023EnforcesSingleLocalUser(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO users (username, role, status, is_local, created_at, updated_at)
		VALUES ('local-one', 'admin', 1, 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("first local insert failed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (username, role, status, is_local, created_at, updated_at)
		VALUES ('local-two', 'admin', 1, 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`); err == nil {
		t.Fatalf("expected the second local user to violate the partial unique index")
	}
	for _, name := range []string{"member-a", "member-b"} {
		if _, err := db.Exec(`INSERT INTO users (username, role, status, is_local, created_at, updated_at)
			VALUES (?, 'member', 1, 0, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`, name); err != nil {
			t.Fatalf("non-local insert %q should be allowed: %v", name, err)
		}
	}
}

func TestRunMigrationsAddsProviderProtocolEndpointsColumn(t *testing.T) {
	db := newMemoryDB(t)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	var found bool
	rows, err := db.Query("PRAGMA table_info(providers)")
	if err != nil {
		t.Fatalf("PRAGMA table_info(providers): %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
			t.Fatalf("scan table_info row: %v", err)
		}
		if name == "protocol_endpoints" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info rows: %v", err)
	}
	if !found {
		t.Fatal("expected providers.protocol_endpoints column after migration, not found")
	}

	// Rolling back the 00012 migration must drop the column again.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 11); err != nil {
		t.Fatalf("RollbackTo(11) failed: %v", err)
	}
	rows2, err := db.Query("PRAGMA table_info(providers)")
	if err != nil {
		t.Fatalf("PRAGMA table_info(providers) after rollback: %v", err)
	}
	defer func() { _ = rows2.Close() }()
	for rows2.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows2.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
			t.Fatalf("scan table_info row after rollback: %v", err)
		}
		if name == "protocol_endpoints" {
			t.Fatal("expected providers.protocol_endpoints column to be dropped after rollback to version 11")
		}
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("iterate table_info rows after rollback: %v", err)
	}
}

func TestRollbackToVersionZero(t *testing.T) {
	db := newMemoryDB(t)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 0); err != nil {
		t.Fatalf("RollbackTo(0) failed: %v", err)
	}

	version, err := GetCurrentVersion(db, "sqlite")
	if err != nil {
		t.Fatalf("GetCurrentVersion after rollback failed: %v", err)
	}
	if version != 0 {
		t.Fatalf("expected version 0 after rollback, got %d", version)
	}
}

// TestMigration00015SystemSettingsAndCSPColumns verifies that migration 00015
// creates the system_settings table with both seed rows and adds the three
// custom-system-prompt columns to api_keys with the expected defaults.
func TestMigration00015SystemSettingsAndCSPColumns(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	// system_settings table exists with both seed rows.
	var enabledValue string
	err := db.QueryRow("SELECT value FROM system_settings WHERE key = 'custom_system_prompt_enabled'").Scan(&enabledValue)
	if err != nil {
		t.Fatalf("seed row custom_system_prompt_enabled missing: %v", err)
	}
	if enabledValue != "false" {
		t.Fatalf("enabled seed = %q, want false", enabledValue)
	}

	var textValue string
	err = db.QueryRow("SELECT value FROM system_settings WHERE key = 'custom_system_prompt'").Scan(&textValue)
	if err != nil {
		t.Fatalf("seed row custom_system_prompt missing: %v", err)
	}
	if textValue != "" {
		t.Fatalf("text seed = %q, want empty", textValue)
	}

	// api_keys gained the three columns with safe defaults — insert a row
	// using only NOT NULL pre-existing columns and read the new columns back.
	res, err := db.Exec(`INSERT INTO api_keys (key_hash, key_prefix, status, budget_spent_micros, created_at, updated_at) VALUES ('h', 'sk-x', 1, 0, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	rowID, _ := res.LastInsertId()

	var cspEnabled int
	var cspOverride int
	var cspText string
	err = db.QueryRow("SELECT custom_system_prompt_enabled, custom_system_prompt_enabled_override, custom_system_prompt FROM api_keys WHERE id = ?", rowID).Scan(&cspEnabled, &cspOverride, &cspText)
	if err != nil {
		t.Fatalf("read api key csp columns: %v", err)
	}
	if cspEnabled != 0 || cspOverride != 0 || cspText != "" {
		t.Fatalf("csp columns default not false/false/empty: enabled=%d override=%d text=%q", cspEnabled, cspOverride, cspText)
	}
}

// TestMigration00016InputCompression verifies that migration 00016 seeds the
// input_compression_enabled row in system_settings, adds the two per-key
// override columns to api_keys, the four savings columns to request_logs,
// and the debug body column to request_log_bodies — all with safe defaults.
// Rolling back to version 15 must drop every new column and delete the seed row.
func TestMigration00016InputCompression(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	// system_settings seed row exists with the off default.
	var enabledValue string
	err := db.QueryRow("SELECT value FROM system_settings WHERE key = 'input_compression_enabled'").Scan(&enabledValue)
	if err != nil {
		t.Fatalf("seed row input_compression_enabled missing: %v", err)
	}
	if enabledValue != "false" {
		t.Fatalf("input_compression_enabled seed = %q, want false", enabledValue)
	}

	// api_keys compress columns default to 0 (inherit global default).
	res, err := db.Exec(`INSERT INTO api_keys (key_hash, key_prefix, status, budget_spent_micros, created_at, updated_at) VALUES ('h2', 'sk-y', 1, 0, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	apiKeyID, _ := res.LastInsertId()

	var compressEnabled int
	var compressOverride int
	err = db.QueryRow("SELECT compress_enabled, compress_enabled_override FROM api_keys WHERE id = ?", apiKeyID).Scan(&compressEnabled, &compressOverride)
	if err != nil {
		t.Fatalf("read api key compress columns: %v", err)
	}
	if compressEnabled != 0 || compressOverride != 0 {
		t.Fatalf("api_keys compress defaults not 0/0: enabled=%d override=%d", compressEnabled, compressOverride)
	}

	// request_logs compress columns default to 0/0/empty/empty.
	rlRes, err := db.Exec(`INSERT INTO request_logs (request_id, model_name, status_code, created_at) VALUES ('req-1', 'm', 200, '2026-01-01 00:00:00')`)
	if err != nil {
		t.Fatalf("create request log: %v", err)
	}
	rlID, _ := rlRes.LastInsertId()

	var tokensSaved int
	var costSaved int64
	var skipReason string
	var compressors string
	err = db.QueryRow("SELECT compress_estimated_tokens_saved, compress_estimated_cost_saved_micros, compress_skip_reason, compressors_applied FROM request_logs WHERE id = ?", rlID).Scan(&tokensSaved, &costSaved, &skipReason, &compressors)
	if err != nil {
		t.Fatalf("read request_logs compress columns: %v", err)
	}
	if tokensSaved != 0 || costSaved != 0 || skipReason != "" || compressors != "" {
		t.Fatalf("request_logs compress defaults not 0/0/empty/empty: tokens=%d cost=%d skip=%q compressors=%q", tokensSaved, costSaved, skipReason, compressors)
	}

	// request_log_bodies compressed_request_body defaults to empty string.
	rlbRes, err := db.Exec(`INSERT INTO request_log_bodies (request_id, created_at) VALUES ('req-1', '2026-01-01 00:00:00')`)
	if err != nil {
		t.Fatalf("create request log body: %v", err)
	}
	rlbID, _ := rlbRes.LastInsertId()

	var compressedBody string
	err = db.QueryRow("SELECT compressed_request_body FROM request_log_bodies WHERE id = ?", rlbID).Scan(&compressedBody)
	if err != nil {
		t.Fatalf("read request_log_bodies compress column: %v", err)
	}
	if compressedBody != "" {
		t.Fatalf("compressed_request_body default = %q, want empty", compressedBody)
	}

	// Rolling back migration 00016 must drop every new column and remove the
	// seed row.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 15); err != nil {
		t.Fatalf("RollbackTo(15) failed: %v", err)
	}

	// Seed row must be gone.
	var remaining string
	err = db.QueryRow("SELECT value FROM system_settings WHERE key = 'input_compression_enabled'").Scan(&remaining)
	if err == nil {
		t.Fatalf("input_compression_enabled seed row still present after rollback: value=%q", remaining)
	}

	// Helper: scan one column name from a table and return whether it exists.
	hasColumn := func(table, col string) bool {
		t.Helper()
		rows, err := db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatalf("PRAGMA table_info(%s): %v", table, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				cid        int
				name       string
				colType    string
				notNull    int
				defaultVal sql.NullString
				pk         int
			)
			if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
				t.Fatalf("scan table_info(%s) row: %v", table, err)
			}
			if name == col {
				return true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate table_info(%s) rows: %v", table, err)
		}
		return false
	}

	for _, c := range []struct{ table, col string }{
		{"api_keys", "compress_enabled"},
		{"api_keys", "compress_enabled_override"},
		{"request_logs", "compress_estimated_tokens_saved"},
		{"request_logs", "compress_estimated_cost_saved_micros"},
		{"request_logs", "compress_skip_reason"},
		{"request_logs", "compressors_applied"},
		{"request_log_bodies", "compressed_request_body"},
	} {
		if hasColumn(c.table, c.col) {
			t.Fatalf("expected %s.%s to be dropped after rollback to 15", c.table, c.col)
		}
	}
}

// TestMigration00018ModelCandidateCapabilityTristate verifies that migration
// 00018 makes the two capability columns nullable so they can express "unknown"
// alongside supported/unsupported, and that rolling back restores the old
// two-state shape by collapsing unknown to false.
//
// Nullability is the whole point of the migration: the gateway drops candidates
// whose capability flag reads false from streaming and tool-calling rotation, so
// an inconclusive probe needs somewhere to go other than false.
func TestMigration00018ModelCandidateCapabilityTristate(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Roll back so the up-migration's value handling is exercised against a
	// database holding the two-state data the old schema actually stored.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 17); err != nil {
		t.Fatalf("RollbackTo(17) to stage pre-migration data failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO models (id, name, management_status, created_at, updated_at)
		VALUES (1, 'm', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	// Two providers, because model_candidates carries UNIQUE(model_id, provider_id).
	for id, name := range map[int]string{1: "p1", 2: "p2", 3: "p3"} {
		if _, err := db.Exec(`INSERT INTO providers (id, name, provider_type, base_url, management_status, destination_version, created_at, updated_at)
			VALUES (?, ?, 'openai', 'https://example.invalid', 1, 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`, id, name); err != nil {
			t.Fatalf("seed provider %d: %v", id, err)
		}
	}
	// Candidate 1 has a PROVEN streaming capability and a stored false for tools;
	// candidate 2 has false for both.
	seedCandidate := func(t *testing.T, id, providerID, sortOrder int, streaming, functionCalling int) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO model_candidates
			(id, model_id, provider_id, provider_model_name, input_price, output_price, max_output,
			 management_status, sort_order, verification_status, created_at, updated_at,
			 supports_streaming, supports_function_calling)
			VALUES (?, 1, ?, 'gpt-4o', 0, 0, 0, 1, ?, 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00', ?, ?)`,
			id, providerID, sortOrder, streaming, functionCalling); err != nil {
			t.Fatalf("seed candidate %d: %v", id, err)
		}
	}
	seedCandidate(t, 1, 1, 1, 1, 0)
	seedCandidate(t, 2, 2, 2, 0, 0)

	// The up-migration under test.
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}

	readCapabilities := func(t *testing.T, id int) (streaming, functionCalling sql.NullBool) {
		t.Helper()
		if err := db.QueryRow(`SELECT supports_streaming, supports_function_calling FROM model_candidates WHERE id = ?`, id).
			Scan(&streaming, &functionCalling); err != nil {
			t.Fatalf("read capability columns for candidate %d: %v", id, err)
		}
		return streaming, functionCalling
	}

	// A stored false was written by the old "anything not proven is false" rule
	// and cannot be told apart from a misclassified probe, so it must become
	// unknown. A stored true came from a probe that actually succeeded and must
	// survive — discarding it would make every verified candidate demand a retest.
	streaming, functionCalling := readCapabilities(t, 1)
	if !streaming.Valid || !streaming.Bool {
		t.Fatalf("expected a proven supports_streaming=true to survive the migration, got %+v", streaming)
	}
	if functionCalling.Valid {
		t.Fatalf("expected a stored supports_function_calling=false to become unknown, got %+v", functionCalling)
	}
	if streaming, functionCalling = readCapabilities(t, 2); streaming.Valid || functionCalling.Valid {
		t.Fatalf("expected both stored falses to become unknown, got streaming=%+v function_calling=%+v", streaming, functionCalling)
	}

	// Nullability is the point of the migration: before it, these columns were
	// NOT NULL and this insert would fail.
	if _, err := db.Exec(`INSERT INTO model_candidates
		(id, model_id, provider_id, provider_model_name, input_price, output_price, max_output,
		 management_status, sort_order, verification_status, created_at, updated_at,
		 supports_streaming, supports_function_calling)
		VALUES (3, 1, 3, 'gpt-4o-mini', 0, 0, 0, 2, 3, 0, '2026-01-01 00:00:00', '2026-01-01 00:00:00', NULL, NULL)`); err != nil {
		t.Fatalf("inserting NULL capabilities failed — columns are still NOT NULL: %v", err)
	}

	// Rolling back collapses unknown to false, restoring the two-state
	// convention, while still carrying proven trues over.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 17); err != nil {
		t.Fatalf("RollbackTo(17) failed: %v", err)
	}
	var downStreaming, downFunctionCalling bool
	if err := db.QueryRow(`SELECT supports_streaming, supports_function_calling FROM model_candidates WHERE id = 1`).
		Scan(&downStreaming, &downFunctionCalling); err != nil {
		t.Fatalf("read capability columns after rollback: %v", err)
	}
	if !downStreaming {
		t.Fatal("expected rollback to preserve a proven supports_streaming=true, got false")
	}
	if downFunctionCalling {
		t.Fatal("expected rollback to collapse an unknown supports_function_calling to false, got true")
	}

	// Re-applying must work, so an operator who rolls back is not stuck there.
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying migrations after rollback failed: %v", err)
	}
	version, err := GetCurrentVersion(db, "sqlite")
	if err != nil {
		t.Fatalf("GetCurrentVersion failed: %v", err)
	}
	if want := maxEmbeddedMigrationVersion(t, migrations.SQLiteFS, "sqlite"); version != want {
		t.Fatalf("expected version %d after re-apply, got %d", want, version)
	}
}

func TestMigration00022VisionFallback(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	// Both settings rows are seeded at the SAME version — the repository
	// reader treats a version mismatch between the pair as corruption, so the
	// seed itself must satisfy that invariant.
	rows, err := db.Query("SELECT key, value, version FROM system_settings WHERE key IN ('vision_fallback_model','vision_fallback_prompt') ORDER BY key")
	if err != nil {
		t.Fatalf("query settings pair: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	versions := map[string]int64{}
	for rows.Next() {
		var k, v string
		var ver int64
		if err := rows.Scan(&k, &v, &ver); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v != "" {
			t.Fatalf("seed %s = %q, want empty (feature off, built-in prompt)", k, v)
		}
		keys = append(keys, k)
		versions[k] = ver
	}
	if len(keys) != 2 {
		t.Fatalf("seeded %d vision_fallback rows, want 2 (%v)", len(keys), keys)
	}
	if versions["vision_fallback_model"] != versions["vision_fallback_prompt"] {
		t.Fatalf("seed versions differ: %v", versions)
	}

	// models.supports_image_input round-trips all three states: absent (NULL),
	// declared true, declared false.
	if _, err := db.Exec(`INSERT INTO models (name, management_status, created_at, updated_at) VALUES ('tri', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	var got *bool
	if err := db.QueryRow("SELECT supports_image_input FROM models WHERE name = 'tri'").Scan(&got); err != nil {
		t.Fatalf("read declaration: %v", err)
	}
	if got != nil {
		t.Fatalf("fresh row declaration = %v, want NULL (undeclared)", *got)
	}
	for _, want := range []bool{true, false} {
		if _, err := db.Exec("UPDATE models SET supports_image_input = ? WHERE name = 'tri'", want); err != nil {
			t.Fatalf("set declaration %v: %v", want, err)
		}
		if err := db.QueryRow("SELECT supports_image_input FROM models WHERE name = 'tri'").Scan(&got); err != nil {
			t.Fatalf("reread: %v", err)
		}
		if got == nil || *got != want {
			t.Fatalf("declaration round-trip = %v, want %v", got, want)
		}
	}

	// request_logs sub-call columns default to the normal-request markers.
	if _, err := db.Exec(`INSERT INTO request_logs (request_id, model_name, status_code, created_at) VALUES ('req-vf', 'm', 200, '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("create request log: %v", err)
	}
	var source, parent string
	if err := db.QueryRow("SELECT source, parent_request_id FROM request_logs WHERE request_id = 'req-vf'").Scan(&source, &parent); err != nil {
		t.Fatalf("read sub-call columns: %v", err)
	}
	if source != "" || parent != "" {
		t.Fatalf("defaults = %q/%q, want empty/empty", source, parent)
	}
}
