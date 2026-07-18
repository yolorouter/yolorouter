package database

import (
	"database/sql"
	"testing"

	// database.go (same package) imports github.com/glebarez/sqlite, which
	// transitively imports github.com/glebarez/go-sqlite and registers the
	// "sqlite" database/sql driver via its init(). Blank-importing
	// modernc.org/sqlite here as well (as the plan's illustrative test does)
	// panics with "sql: Register called twice for driver sqlite" because
	// both packages register the exact same driver name — so we rely on the
	// registration already pulled in transitively instead of duplicating it.

	"github.com/yolorouter/yolorouter-ce/migrations"
)

// newMemoryDB opens an in-memory SQLite database for a single test and
// registers its Close via t.Cleanup, so callers don't each repeat the
// open-or-fail-and-defer-close boilerplate.
func newMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRunMigrationsAppliesBaselineOnSQLite(t *testing.T) {
	db := newMemoryDB(t)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	// goose 元数据表必须存在，且版本至少为 1（baseline 迁移已应用）
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
	// baseline migration — it must be bumped whenever a new migration file
	// is added to migrations/sqlite (currently 00001_baseline.sql +
	// 00002_create_admin_auth.sql + 00003_add_admin_sessions_expires_at_index.sql).
	if version != 3 {
		t.Fatalf("expected version 3 after all migrations, got %d", version)
	}
}

func TestRunMigrationsAppliesAdminAuthTablesOnSQLite(t *testing.T) {
	db := newMemoryDB(t)

	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	version, err := GetCurrentVersion(db, "sqlite")
	if err != nil {
		t.Fatalf("GetCurrentVersion failed: %v", err)
	}
	if version < 2 {
		t.Fatalf("expected version >= 2 after admin_auth migration, got %d", version)
	}

	for _, table := range []string{"admins", "admin_sessions"} {
		var name string
		row := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table)
		if err := row.Scan(&name); err != nil {
			t.Fatalf("table %q not found after migration: %v", table, err)
		}
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
