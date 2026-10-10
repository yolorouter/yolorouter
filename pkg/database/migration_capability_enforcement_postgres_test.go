package database

import (
	"database/sql"
	"testing"

	"github.com/yolorouter/yolorouter/migrations"
)

// The capability-enforcement seed's Postgres arm. The seed is plain SQL,
// but its Down must remove exactly the seven rows it added here too, and
// the shared-version contract the CAS writer depends on is only real on
// the backend half of production. Gated on TEST_POSTGRES_DSN exactly like
// the rest of this family: unset means skip, so the local suite stays
// green without a server.

// capabilitySeedRows pins the seven seeded rows and their shipped values.
var capabilitySeedRows = map[string]string{
	"capability_context_limit_block":    "false",
	"capability_temperature_default":    "false",
	"capability_temperature_force":      "false",
	"capability_tools_strict":           "false",
	"capability_reasoning_strict":       "false",
	"capability_reasoning_egress_clear": "false",
	"capability_context_margin_percent": "5",
}

// assertCapabilitySeed pins the enforcement rows: all seven present with
// the shipped all-off/5% default and sharing one version after the chain,
// all absent after the batch's Down, and restored by a re-apply.
func assertCapabilitySeed(t *testing.T, db *sql.DB, tree string, wantPresent bool) {
	t.Helper()
	rows, err := db.Query(`SELECT key, value, version FROM system_settings WHERE key LIKE 'capability_%'`)
	if err != nil {
		t.Fatalf("%s tree: read capability rows: %v", tree, err)
	}
	defer rows.Close()
	got := map[string]struct {
		value   string
		version int64
	}{}
	for rows.Next() {
		var k, v string
		var ver int64
		if err := rows.Scan(&k, &v, &ver); err != nil {
			t.Fatalf("%s tree: scan capability row: %v", tree, err)
		}
		got[k] = struct {
			value   string
			version int64
		}{v, ver}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s tree: iterate capability rows: %v", tree, err)
	}
	if !wantPresent {
		if len(got) != 0 {
			t.Fatalf("%s tree: %d capability rows present, want the Down to have removed them all", tree, len(got))
		}
		return
	}
	if len(got) != len(capabilitySeedRows) {
		t.Fatalf("%s tree: %d capability rows, want %d", tree, len(got), len(capabilitySeedRows))
	}
	sharedVersion := int64(-1)
	for key, want := range capabilitySeedRows {
		row, ok := got[key]
		if !ok {
			t.Fatalf("%s tree: capability row %q missing", tree, key)
		}
		if row.value != want {
			t.Fatalf("%s tree: %s=%q, want %q", tree, key, row.value, want)
		}
		if sharedVersion == -1 {
			sharedVersion = row.version
		} else if row.version != sharedVersion {
			t.Fatalf("%s tree: capability rows share no version (%s at v%d vs v%d) — the CAS writer's witness depends on it", tree, key, row.version, sharedVersion)
		}
	}
}

// TestPostgresMigration00057CapabilityEnforcementSeed is the Postgres side
// of the enforcement seed: all seven rows present after the chain, removed
// by the batch's Down, and restored by a re-apply. The audit-retention
// setting must survive the whole round trip untouched.
func TestPostgresMigration00057CapabilityEnforcementSeed(t *testing.T) {
	db := newTestPostgresDB(t)
	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	assertCapabilitySeed(t, db, "postgres", true)

	if err := RollbackTo(db, "postgres", migrations.PostgresFS, "postgres", 56); err != nil {
		t.Fatalf("RollbackTo(56) failed: %v", err)
	}
	assertCapabilitySeed(t, db, "postgres", false)

	if err := RunMigrations(db, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}
	assertCapabilitySeed(t, db, "postgres", true)

	var auditRetention sql.NullString
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE key = 'admin_audit_retention_days'`).Scan(&auditRetention); err != nil {
		t.Fatalf("postgres tree: read admin_audit_retention_days: %v", err)
	}
	if !auditRetention.Valid {
		t.Fatal("postgres tree: admin_audit_retention_days must survive the capability batch untouched")
	}
}
