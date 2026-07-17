package database

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	// database.go (same package) imports github.com/glebarez/sqlite, which
	// transitively registers the "sqlite" database/sql driver via its init().
	// Blank-importing modernc.org/sqlite here as well (as the plan's
	// illustrative code does) panics with "sql: Register called twice for
	// driver sqlite" because both packages register the exact same driver
	// name — so we rely on the registration already pulled in transitively
	// instead of duplicating it (see migration_test.go for the same note).
)

// ResetSQLite deletes the SQLite database file (and its -wal/-shm sidecars)
// and re-runs migrations from scratch. It requires the exclusive instance
// lock — if another process (typically `serve`) holds it, ResetSQLite fails
// immediately rather than risking the "delete a file another connection has
// open" split-brain scenario.
func ResetSQLite(dbPath, lockPath string, migrationsFS fs.FS, dir string) error {
	unlock, err := AcquireInstanceLock(lockPath)
	if err != nil {
		return fmt.Errorf("cannot reset while another instance is running: %w", err)
	}
	defer func() { _ = unlock() }()

	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := dbPath + suffix
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	// Recreate the file ourselves with restrictive permissions before
	// handing it to SQLite — otherwise SQLite's own default file creation
	// (0644 & umask, typically world-readable) would apply the moment the
	// migrations below first write to it, permanently exposing the
	// freshly reset database instead of just matching database.Init's
	// same protection on a normal first boot.
	if err := ensureSQLiteFileRestrictivePermissions(dbPath); err != nil {
		return fmt.Errorf("recreate database file: %w", err)
	}

	// sqliteConnectionQuery closes the same residual gap database.Init
	// guards against (mode=rw: this Open can only touch a file that
	// already exists, the one just recreated above, rather than silently
	// recreating it itself with SQLite's default world/group-readable
	// permissions if dbPath were a dangling symlink or got deleted again
	// in between) and keeps foreign key enforcement on for whatever
	// migrations/seed data run below — the same query database.Init's own
	// connection uses, so `serve`/`db:migrate` and `db:reset` never
	// disagree about it.
	db, err := sql.Open("sqlite", sqliteFileURI(dbPath, sqliteConnectionQuery))
	if err != nil {
		return fmt.Errorf("reopen database after delete: %w", err)
	}
	defer func() { _ = db.Close() }()

	return remigrate(db, "sqlite", migrationsFS, dir)
}
