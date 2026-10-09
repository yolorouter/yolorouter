package database

import (
	"database/sql"
	"testing"

	"github.com/yolorouter/yolorouter/migrations"
)

// adminAuditColumnShape is the exact DDL contract of admin_audit_logs as the
// sqlite tree creates it, in declaration order: (name, sqlite type,
// NOT NULL). The postgres twin test holds the same contract in that tree's
// vocabulary — the two are mirrors and a change to one without the other is
// exactly the drift these tests exist to catch. admin_user_id is the only
// nullable column: rows captured before a session exists (public auth
// endpoints, unauthenticated probes) carry no identity.
var adminAuditColumnShape = []struct {
	name     string
	colType  string
	notNull  bool
	isPK     bool
	defaults string // expected PRAGMA dflt_value, "" column means NULL default
}{
	{name: "id", colType: "INTEGER", isPK: true},
	{name: "created_at", colType: "DATETIME", notNull: true},
	{name: "admin_user_id", colType: "INTEGER"},
	{name: "admin_username", colType: "TEXT", notNull: true, defaults: "''"},
	{name: "action", colType: "TEXT", notNull: true},
	{name: "resource_type", colType: "TEXT", notNull: true, defaults: "''"},
	{name: "resource_id", colType: "TEXT", notNull: true, defaults: "''"},
	{name: "detail", colType: "TEXT", notNull: true, defaults: "''"},
	{name: "status", colType: "TEXT", notNull: true, defaults: "'success'"},
	{name: "error_brief", colType: "TEXT", notNull: true, defaults: "''"},
	{name: "source_ip", colType: "TEXT", notNull: true, defaults: "''"},
	{name: "user_agent", colType: "TEXT", notNull: true, defaults: "''"},
	{name: "request_id", colType: "TEXT", notNull: true, defaults: "''"},
}

// adminAuditIndexNames are the four read-path indexes the console audit
// surface depends on: the retention sweep and the default time-ordered
// listing both ride created_at, the admin and action filters are the two
// hottest console dimensions, and request_id is the reconciliation key an
// operator resolves one specific console request by.
var adminAuditIndexNames = []string{
	"idx_admin_audit_logs_created_at",
	"idx_admin_audit_logs_admin_user_id",
	"idx_admin_audit_logs_action",
	"idx_admin_audit_logs_request_id",
}

// assertAdminAuditSQLiteShape pins the fresh-database DDL on the sqlite
// tree: every column with its type and nullability, the defaults the insert
// path leans on, and the four named indexes. Shared by the fresh-database
// test and the replay test so both directions of the migration hold the
// same contract.
func assertAdminAuditSQLiteShape(t *testing.T, db *sql.DB) {
	t.Helper()
	type sqliteColumn struct {
		name    string
		ctype   string
		notNull int
		dflt    sql.NullString
		pk      int
	}
	rows, err := db.Query(`PRAGMA table_info(admin_audit_logs)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(admin_audit_logs): %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []sqliteColumn
	for rows.Next() {
		var col sqliteColumn
		var cid int
		if err := rows.Scan(&cid, &col.name, &col.ctype, &col.notNull, &col.dflt, &col.pk); err != nil {
			t.Fatalf("scan table_info row: %v", err)
		}
		got = append(got, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info rows: %v", err)
	}
	if len(got) != len(adminAuditColumnShape) {
		t.Fatalf("admin_audit_logs has %d columns, want %d", len(got), len(adminAuditColumnShape))
	}
	for i, want := range adminAuditColumnShape {
		g := got[i]
		if g.name != want.name {
			t.Fatalf("column %d is %q, want %q", i, g.name, want.name)
		}
		if g.ctype != want.colType {
			t.Fatalf("column %s type is %q, want %q", want.name, g.ctype, want.colType)
		}
		if (g.notNull != 0) != want.notNull {
			t.Fatalf("column %s NOT NULL is %v, want %v", want.name, g.notNull != 0, want.notNull)
		}
		if (g.pk != 0) != want.isPK {
			t.Fatalf("column %s PK flag is %v, want %v", want.name, g.pk != 0, want.isPK)
		}
		if want.defaults != "" || g.dflt.Valid {
			gotDflt := ""
			if g.dflt.Valid {
				gotDflt = g.dflt.String
			}
			if gotDflt != want.defaults {
				t.Fatalf("column %s default is %q, want %q", want.name, gotDflt, want.defaults)
			}
		}
	}

	indexes := map[string]bool{}
	idxRows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'admin_audit_logs'`)
	if err != nil {
		t.Fatalf("read sqlite_master indexes: %v", err)
	}
	defer func() { _ = idxRows.Close() }()
	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			t.Fatalf("scan sqlite_master index row: %v", err)
		}
		indexes[name] = true
	}
	if err := idxRows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master index rows: %v", err)
	}
	for _, name := range adminAuditIndexNames {
		if !indexes[name] {
			t.Fatalf("index %s missing from admin_audit_logs (have %v)", name, indexes)
		}
	}
}

// TestMigration00052FreshSQLiteCarriesAdminAuditLogs proves the fresh-start
// path a new deployment takes: the whole sqlite chain on an empty database
// produces exactly the audit table the runtime maps onto, and a row written
// the way the capture worker writes one (every column named, identity NULL)
// round-trips — NULL must survive as NULL, not collapse to 0 or ”, because
// "no resolved identity" and "identity 0" mean different things to every
// reader of the table. The insert that omits the defaulted columns pins the
// NOT NULL DEFAULT contract those columns carry.
func TestMigration00052FreshSQLiteCarriesAdminAuditLogs(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	assertAdminAuditSQLiteShape(t, db)

	if _, err := db.Exec(`INSERT INTO admin_audit_logs
		(created_at, admin_user_id, admin_username, action, resource_type, resource_id, detail, status, error_brief, source_ip, user_agent, request_id)
		VALUES ('2026-10-09 01:02:03', NULL, 'boss', 'auth.login', 'auth', '', '{"username":"bob"}', 'success', '', '127.0.0.1', 'probe-agent', '550e8400-e29b-41d4-a716-446655440000')`); err != nil {
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

	// The defaulted columns: an insert naming only the required ones must
	// land the documented defaults, not fail the NOT NULL constraints.
	if _, err := db.Exec(`INSERT INTO admin_audit_logs (created_at, action) VALUES ('2026-10-09 01:02:04', 'auth.login')`); err != nil {
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
}

// TestMigration00052SQLiteDownDropsTableAndIndexes walks the Down of the
// audit migration on the sqlite tree: an operator who downgrades past this
// version must land on a database with neither the table nor its indexes,
// and migrating forward again must rebuild the identical shape.
func TestMigration00052SQLiteDownDropsTableAndIndexes(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 51); err != nil {
		t.Fatalf("RollbackTo(51) failed: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE tbl_name = 'admin_audit_logs'`).Scan(&n); err != nil {
		t.Fatalf("count admin_audit_logs objects after rollback: %v", err)
	}
	if n != 0 {
		t.Fatalf("admin_audit_logs left %d objects in sqlite_master after rolling back to 51, want 0", n)
	}
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying migrations after rollback failed: %v", err)
	}
	assertAdminAuditSQLiteShape(t, db)
}

// seedSQLiteProviderKeyAt52 writes one provider_keys row the way a binary
// running schema version 52 would — no key_suffix column exists there, so
// the INSERT cannot name it. The upgrade replay then proves the migration
// leaves that row with the empty-string not-yet-derived marker rather than
// NULL or a bogus value.
func seedSQLiteProviderKeyAt52(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO providers (id, name, base_url, created_at, updated_at)
		VALUES (1, 'suffix-provider', 'https://api.example.com', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)
	mustExec(t, db, `INSERT INTO provider_keys (provider_id, label, encrypted_key, key_prefix, sort_order, test_model, authorized_destination_version, created_at, updated_at)
		VALUES (1, 'legacy-key', 'ciphertext-blob', 'sk-a', 1, 'test-model', 1, '2026-01-02 00:00:00', '2026-01-02 00:00:00')`)
}

// TestMigration00053SQLiteKeySuffixUpgradeReplay pins the key-suffix column
// on the sqlite tree: present, TEXT, NOT NULL, defaulting to ” — and the
// upgrade replay proves a row stored before the column existed reads ”
// afterwards (never NULL), which is precisely the marker the startup
// backfill selects on. The downgrade drops the column with rows present and
// the second replay restores the same shape.
func TestMigration00053SQLiteKeySuffixUpgradeReplay(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	colType, notNull, found := sqliteColumnInfo(t, db, "provider_keys", "key_suffix")
	if !found {
		t.Fatal("provider_keys.key_suffix missing after the full migration chain")
	}
	if colType != "TEXT" || !notNull {
		t.Fatalf("key_suffix shape wrong: type=%q notNull=%v, want TEXT and NOT NULL", colType, notNull)
	}

	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 52); err != nil {
		t.Fatalf("RollbackTo(52) failed: %v", err)
	}
	if _, _, found := sqliteColumnInfo(t, db, "provider_keys", "key_suffix"); found {
		t.Fatal("key_suffix still present after rolling back to version 52")
	}
	seedSQLiteProviderKeyAt52(t, db)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}
	var suffix sql.NullString
	if err := db.QueryRow(`SELECT key_suffix FROM provider_keys WHERE label = 'legacy-key'`).Scan(&suffix); err != nil {
		t.Fatalf("read legacy key suffix after upgrade: %v", err)
	}
	if !suffix.Valid || suffix.String != "" {
		t.Fatalf("pre-existing key read key_suffix=%+v after upgrade, want the empty string (not-yet-derived marker)", suffix)
	}

	// Downgrade with the row present, then forward again: the column comes
	// back with the same default, and the row reads '' once more.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 52); err != nil {
		t.Fatalf("RollbackTo(52) with rows present failed: %v", err)
	}
	if _, _, found := sqliteColumnInfo(t, db, "provider_keys", "key_suffix"); found {
		t.Fatal("key_suffix still present after the second rollback to version 52")
	}
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying after the second rollback failed: %v", err)
	}
	if err := db.QueryRow(`SELECT key_suffix FROM provider_keys WHERE label = 'legacy-key'`).Scan(&suffix); err != nil {
		t.Fatalf("read legacy key suffix after second replay: %v", err)
	}
	if !suffix.Valid || suffix.String != "" {
		t.Fatalf("pre-existing key read key_suffix=%+v after second replay, want empty string", suffix)
	}
}

// TestMigration00054SQLiteSeedsAuditRetentionSetting pins the audit
// retention seed on the sqlite tree: a fresh deployment starts at 180 days
// (a bounded default — an audit log nobody prunes slowly becomes a
// liability nobody reads), the Down removes exactly that one row, and a
// re-apply restores it. The neighboring request-log retention setting must
// be untouched by all of it.
func TestMigration00054SQLiteSeedsAuditRetentionSetting(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	assertAuditRetentionSeed(t, db, "sqlite", true)

	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 53); err != nil {
		t.Fatalf("RollbackTo(53) failed: %v", err)
	}
	assertAuditRetentionSeed(t, db, "sqlite", false)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}
	assertAuditRetentionSeed(t, db, "sqlite", true)
}
