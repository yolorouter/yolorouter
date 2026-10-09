package database

import (
	"database/sql"
	"testing"

	"github.com/yolorouter/yolorouter/migrations"
)

// The admin-audit batch's Postgres arms. Every other test in the suite runs
// on SQLite, so these are the only place the batch's three migrations ever
// execute against the backend half of production: the audit table's DDL has
// Postgres-specific shapes (BIGSERIAL, TIMESTAMPTZ), the key-suffix column's
// ALTER TABLE and its interaction with pre-existing rows is backend
// behavior, and the retention seed is plain SQL but its Down must remove
// exactly the seeded row here too. Gated on TEST_POSTGRES_DSN exactly like
// the rest of this file's siblings: unset means skip, so the local suite
// stays green without a server.

// adminAuditPostgresColumnShape is the admin_audit_logs DDL contract in
// Postgres vocabulary — (column, data_type, is_nullable) straight from
// information_schema. The sqlite tree's twin list lives in
// migration_admin_audit_sqlite_test.go; the two are mirrors and must move
// together.
var adminAuditPostgresColumnShape = []struct {
	name       string
	dataType   string
	nullable   bool
	hasDefault bool
}{
	{name: "id", dataType: "bigint", hasDefault: true}, // BIGSERIAL
	{name: "created_at", dataType: "timestamp with time zone"},
	{name: "admin_user_id", dataType: "bigint", nullable: true},
	{name: "admin_username", dataType: "text", hasDefault: true},
	{name: "action", dataType: "text"},
	{name: "resource_type", dataType: "text", hasDefault: true},
	{name: "resource_id", dataType: "text", hasDefault: true},
	{name: "detail", dataType: "text", hasDefault: true},
	{name: "status", dataType: "text", hasDefault: true},
	{name: "error_brief", dataType: "text", hasDefault: true},
	{name: "source_ip", dataType: "text", hasDefault: true},
	{name: "user_agent", dataType: "text", hasDefault: true},
	{name: "request_id", dataType: "text", hasDefault: true},
}

// assertAdminAuditPostgresShape pins the table's DDL through
// information_schema/pg_indexes after the full tree has run.
func assertAdminAuditPostgresShape(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT column_name, data_type, is_nullable,
		COALESCE(column_default, '') <> '' AS has_default
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'admin_audit_logs'
		ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("query information_schema.columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type pgColumn struct {
		name       string
		dataType   string
		nullable   string
		hasDefault bool
	}
	var got []pgColumn
	for rows.Next() {
		var col pgColumn
		if err := rows.Scan(&col.name, &col.dataType, &col.nullable, &col.hasDefault); err != nil {
			t.Fatalf("scan information_schema row: %v", err)
		}
		got = append(got, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate information_schema rows: %v", err)
	}
	if len(got) != len(adminAuditPostgresColumnShape) {
		t.Fatalf("admin_audit_logs has %d columns, want %d", len(got), len(adminAuditPostgresColumnShape))
	}
	for i, want := range adminAuditPostgresColumnShape {
		g := got[i]
		wantNullable := "NO"
		if want.nullable {
			wantNullable = "YES"
		}
		if g.name != want.name {
			t.Fatalf("column %d is %q, want %q", i, g.name, want.name)
		}
		if g.dataType != want.dataType {
			t.Fatalf("column %s data_type is %q, want %q", want.name, g.dataType, want.dataType)
		}
		if g.nullable != wantNullable {
			t.Fatalf("column %s is_nullable is %q, want %q", want.name, g.nullable, wantNullable)
		}
		if g.hasDefault != want.hasDefault {
			t.Fatalf("column %s has_default is %v, want %v", want.name, g.hasDefault, want.hasDefault)
		}
	}

	for _, name := range adminAuditIndexNames {
		var indexed bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes
			WHERE schemaname = 'public' AND tablename = 'admin_audit_logs' AND indexname = $1)`, name).Scan(&indexed); err != nil {
			t.Fatalf("read pg_indexes for %s: %v", name, err)
		}
		if !indexed {
			t.Fatalf("index %s missing on admin_audit_logs (postgres tree)", name)
		}
	}
}

// TestPostgresMigration00052AdminAuditLogs pins the audit table on the
// Postgres tree: the exact column shapes (including the serial default and
// the TIMESTAMPTZ audit timestamps), the four named indexes, a row written
// the way the capture worker writes one (identity NULL) round-tripping with
// NULL intact, the defaults holding when the defaulted columns are omitted,
// and the Down dropping table and indexes so a downgrade lands clean.
func TestPostgresMigration00052AdminAuditLogs(t *testing.T) {
	db := newTestPostgresDB(t)
	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	assertAdminAuditPostgresShape(t, db)

	if _, err := db.Exec(`INSERT INTO admin_audit_logs
		(created_at, admin_user_id, admin_username, action, resource_type, resource_id, detail, status, error_brief, source_ip, user_agent, request_id)
		VALUES (now(), NULL, 'boss', 'auth.login', 'auth', '', '{"username":"bob"}', 'success', '', '127.0.0.1', 'probe-agent', '550e8400-e29b-41d4-a716-446655440000')`); err != nil {
		t.Fatalf("insert full audit row: %v", err)
	}
	var (
		adminUserID sql.NullInt64
		username    string
		detail      string
		requestID   string
	)
	if err := db.QueryRow(`SELECT admin_user_id, admin_username, detail, request_id FROM admin_audit_logs WHERE id = 1`).
		Scan(&adminUserID, &username, &detail, &requestID); err != nil {
		t.Fatalf("read audit row back: %v", err)
	}
	if adminUserID.Valid {
		t.Fatalf("admin_user_id round-tripped as %d, want NULL", adminUserID.Int64)
	}
	if username != "boss" || detail != `{"username":"bob"}` || requestID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("audit row round-trip wrong: username=%q detail=%q request_id=%q", username, detail, requestID)
	}

	if _, err := db.Exec(`INSERT INTO admin_audit_logs (created_at, action) VALUES (now(), 'auth.login')`); err != nil {
		t.Fatalf("insert with defaulted columns omitted: %v", err)
	}
	var (
		snapshotName string
		status       string
	)
	if err := db.QueryRow(`SELECT admin_username, status FROM admin_audit_logs WHERE id = 2`).Scan(&snapshotName, &status); err != nil {
		t.Fatalf("read defaulted audit row: %v", err)
	}
	if snapshotName != "" || status != "success" {
		t.Fatalf("defaults wrong: admin_username=%q status=%q, want '' and 'success'", snapshotName, status)
	}

	// The BIGSERIAL sequence must be past the copied explicit ids — without
	// it the next runtime insert would collide. (The ids here were
	// generated, not copied, but the probe pins the sequence-liveness the
	// capture worker's INSERT depends on.)
	var nextID int64
	if err := db.QueryRow(`INSERT INTO admin_audit_logs (created_at, action)
		VALUES (now(), 'auth.logout') RETURNING id`).Scan(&nextID); err != nil {
		t.Fatalf("insert after shape assertions failed: %v", err)
	}
	if nextID <= 2 {
		t.Fatalf("expected a fresh generated id > 2, got %d", nextID)
	}

	if err := RollbackTo(db, "postgres", migrations.PostgresFS, "postgres", 51); err != nil {
		t.Fatalf("RollbackTo(51) failed: %v", err)
	}
	var objects int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'admin_audit_logs'`).Scan(&objects); err != nil {
		t.Fatalf("count admin_audit_logs after rollback: %v", err)
	}
	if objects != 0 {
		t.Fatal("admin_audit_logs still present after rolling back to version 51")
	}
	for _, name := range adminAuditIndexNames {
		var indexed bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes
			WHERE schemaname = 'public' AND indexname = $1)`, name).Scan(&indexed); err != nil {
			t.Fatalf("read pg_indexes after rollback for %s: %v", name, err)
		}
		if indexed {
			t.Fatalf("index %s survived the rollback to version 51", name)
		}
	}
	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("re-applying migrations after rollback failed: %v", err)
	}
	assertAdminAuditPostgresShape(t, db)
}

// TestPostgresMigration00053ProviderKeySuffix is the Postgres side of the
// key-suffix column: present with the right shape after the full tree, and —
// the part only an upgrade replay can prove — a provider_keys row stored
// before the column existed comes out of the ALTER with the empty-string
// not-yet-derived marker, never NULL, on this backend too. The downgrade
// drops the column with rows present and the second replay restores it.
func TestPostgresMigration00053ProviderKeySuffix(t *testing.T) {
	db := newTestPostgresDB(t)
	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	var (
		dataType string
		nullable string
	)
	if err := db.QueryRow(`SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'provider_keys' AND column_name = 'key_suffix'`).
		Scan(&dataType, &nullable); err != nil {
		t.Fatalf("read key_suffix column shape: %v", err)
	}
	if dataType != "text" || nullable != "NO" {
		t.Fatalf("key_suffix shape wrong: data_type=%q is_nullable=%q, want text and NO", dataType, nullable)
	}

	if err := RollbackTo(db, "postgres", migrations.PostgresFS, "postgres", 52); err != nil {
		t.Fatalf("RollbackTo(52) failed: %v", err)
	}
	mustExec(t, db, `INSERT INTO providers (id, name, base_url, created_at, updated_at)
		VALUES (1, 'suffix-provider', 'https://api.example.com', now(), now())`)
	mustExec(t, db, `INSERT INTO provider_keys (provider_id, label, encrypted_key, key_prefix, sort_order,
			test_model, authorized_destination_version, created_at, updated_at)
		VALUES (1, 'legacy-key', 'ciphertext-blob', 'sk-a', 1, 'test-model', 1, now(), now())`)

	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}
	var suffix sql.NullString
	if err := db.QueryRow(`SELECT key_suffix FROM provider_keys WHERE label = 'legacy-key'`).Scan(&suffix); err != nil {
		t.Fatalf("read legacy key suffix after upgrade: %v", err)
	}
	if !suffix.Valid || suffix.String != "" {
		t.Fatalf("pre-existing key read key_suffix=%+v after upgrade, want the empty string (not-yet-derived marker)", suffix)
	}

	if err := RollbackTo(db, "postgres", migrations.PostgresFS, "postgres", 52); err != nil {
		t.Fatalf("RollbackTo(52) with rows present failed: %v", err)
	}
	var stillThere bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'provider_keys' AND column_name = 'key_suffix')`).Scan(&stillThere); err != nil {
		t.Fatalf("read information_schema after rollback: %v", err)
	}
	if stillThere {
		t.Fatal("key_suffix still present after rolling back to version 52")
	}
	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("re-applying after rollback failed: %v", err)
	}
	if err := db.QueryRow(`SELECT key_suffix FROM provider_keys WHERE label = 'legacy-key'`).Scan(&suffix); err != nil {
		t.Fatalf("read legacy key suffix after second replay: %v", err)
	}
	if !suffix.Valid || suffix.String != "" {
		t.Fatalf("pre-existing key read key_suffix=%+v after second replay, want empty string", suffix)
	}
}

// assertAuditRetentionSeed pins the audit retention setting row shared by
// both trees: present with the 180-day default after the full chain, absent
// after the batch's Down, and restored by a re-apply. The request-log
// retention setting must survive the whole round trip untouched.
func assertAuditRetentionSeed(t *testing.T, db *sql.DB, tree string, wantPresent bool) {
	t.Helper()
	var value sql.NullString
	err := db.QueryRow(`SELECT value FROM system_settings WHERE key = 'admin_audit_retention_days'`).Scan(&value)
	switch {
	case err == sql.ErrNoRows:
		if wantPresent {
			t.Fatalf("%s tree: admin_audit_retention_days row missing, want the seeded 180-day default", tree)
		}
	case err != nil:
		t.Fatalf("%s tree: read admin_audit_retention_days: %v", tree, err)
	default:
		if !wantPresent {
			t.Fatalf("%s tree: admin_audit_retention_days=%q present, want the Down to have removed it", tree, value.String)
		}
		if value.String != "180" {
			t.Fatalf("%s tree: admin_audit_retention_days=%q, want \"180\"", tree, value.String)
		}
	}

	var requestLogRetention sql.NullString
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE key = 'request_log_retention_days'`).Scan(&requestLogRetention); err != nil {
		t.Fatalf("%s tree: read request_log_retention_days: %v", tree, err)
	}
	if !requestLogRetention.Valid {
		t.Fatalf("%s tree: request_log_retention_days must survive the audit batch untouched", tree)
	}
}

// TestPostgresMigration00054AdminAuditRetentionSeed is the Postgres side of
// the retention seed: seed row present at 180 after the chain, removed by
// the Down, restored by the re-apply — with the neighboring request-log
// retention row untouched throughout.
func TestPostgresMigration00054AdminAuditRetentionSeed(t *testing.T) {
	db := newTestPostgresDB(t)
	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	assertAuditRetentionSeed(t, db, "postgres", true)

	if err := RollbackTo(db, "postgres", migrations.PostgresFS, "postgres", 53); err != nil {
		t.Fatalf("RollbackTo(53) failed: %v", err)
	}
	assertAuditRetentionSeed(t, db, "postgres", false)

	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}
	assertAuditRetentionSeed(t, db, "postgres", true)
}
