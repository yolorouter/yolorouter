package database

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	// database.go (same package) imports github.com/glebarez/sqlite, which
	// transitively registers the "sqlite" database/sql driver via its init().
	// Blank-importing modernc.org/sqlite here as well (as the plan's
	// illustrative test does) panics with "sql: Register called twice for
	// driver sqlite" because both packages register the exact same driver
	// name — so we rely on the registration already pulled in transitively
	// instead of duplicating it (see migration_test.go for the same note).

	"github.com/yolorouter/yolorouter/migrations"
)

func TestResetSQLiteRecreatesDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	lockPath := filepath.Join(dir, "yolorouter.lock")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE junk (id INTEGER)"); err != nil {
		t.Fatalf("create junk table: %v", err)
	}
	_ = db.Close()

	// 模拟 WAL sidecar 文件存在
	if err := os.WriteFile(dbPath+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatalf("write -wal sidecar fixture: %v", err)
	}
	if err := os.WriteFile(dbPath+"-shm", []byte("shm"), 0o600); err != nil {
		t.Fatalf("write -shm sidecar fixture: %v", err)
	}

	if err := ResetSQLite(dbPath, lockPath, migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("ResetSQLite failed: %v", err)
	}

	if _, err := os.Stat(dbPath + "-wal"); err == nil {
		t.Fatalf("-wal sidecar should have been removed")
	}
	if _, err := os.Stat(dbPath + "-shm"); err == nil {
		t.Fatalf("-shm sidecar should have been removed")
	}

	newDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen after reset: %v", err)
	}
	defer func() { _ = newDB.Close() }()

	var name string
	row := newDB.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='junk'")
	if err := row.Scan(&name); err == nil {
		t.Fatalf("junk table should not exist after reset")
	}
}

// TestResetSQLiteRecreatesWithRestrictivePermissions guards against the
// freshly reset database being exposed with SQLite's own default create
// mode (0644 & umask) — ResetSQLite must pre-create the file at 0600
// before handing it back to SQLite, the same protection database.Init
// applies on a normal first boot. Pinned to umask 022 (see withUmask) so
// this can't pass by coincidence under a more restrictive ambient umask.
func TestResetSQLiteRecreatesWithRestrictivePermissions(t *testing.T) {
	defer withUmask(0o022)()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	lockPath := filepath.Join(dir, "yolorouter.lock")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = db.Close()

	if err := ResetSQLite(dbPath, lockPath, migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("ResetSQLite failed: %v", err)
	}

	info, statErr := os.Stat(dbPath)
	if statErr != nil {
		t.Fatalf("stat reset database file: %v", statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected reset database file to have 0600 permissions, got %04o", info.Mode().Perm())
	}
}

func TestResetSQLiteFailsWhenLockHeld(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	lockPath := filepath.Join(dir, "yolorouter.lock")

	unlock, err := AcquireInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer func() { _ = unlock() }()

	err = ResetSQLite(dbPath, lockPath, migrations.SQLiteFS, "sqlite")
	if err == nil {
		t.Fatalf("expected ResetSQLite to fail while another instance holds the lock")
	}
}
