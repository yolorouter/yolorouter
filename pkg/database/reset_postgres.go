package database

import (
	"database/sql"
	"fmt"
	"io/fs"
	"strings"
)

// ResetPostgres drops every table in the public schema, then re-runs
// migrations. Mirrors the commercial yolorouter-server's
// internal/cli/cli_reset.go logic. Caller is responsible for the
// release-build guard and interactive confirmation (see cmd/yolorouter).
//
// Like ResetSQLite, it acquires the exclusive instance lock itself rather
// than trusting the caller to do so — the mutual-exclusion precondition
// against a running `serve` instance applies to both drivers, and making
// each Reset* function self-contained means a future destructive operation
// can't be added without it.
//
// Known limitation (accepted, not an oversight): this lock
// is a local file lock (flock), so it only protects against another
// instance on the *same host*. If multiple `serve` instances are deployed
// on different hosts against the same remote Postgres — already outside
// v0.1's single-instance deployment assumption — this lock provides no
// cross-host protection, and ResetPostgres could drop tables while a
// serve instance on another host is actively serving requests against
// them. Supporting that deployment shape would require a real Postgres-side
// advisory lock protocol (serve holds a shared lock, reset acquires an
// exclusive one), not a patch on top of the local file lock.
func ResetPostgres(db *sql.DB, driver string, migrationsFS fs.FS, dir string, lockPath string) error {
	unlock, err := AcquireInstanceLock(lockPath)
	if err != nil {
		return fmt.Errorf("cannot reset while another instance is running: %w", err)
	}
	defer func() { _ = unlock() }()

	rows, err := db.Query("SELECT tablename FROM pg_tables WHERE schemaname = 'public'")
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan table name: %w", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate table list: %w", err)
	}
	_ = rows.Close()

	// All drops run in one transaction: a failure partway through (e.g. a
	// permission error on one table) rolls back rather than leaving the
	// database in a half-dropped state.
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin reset transaction: %w", err)
	}
	for _, t := range tables {
		if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, quotePostgresIdentifier(t))); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("drop table %s: %w", t, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reset transaction: %w", err)
	}

	return remigrate(db, driver, migrationsFS, dir)
}

// quotePostgresIdentifier double-quotes a Postgres identifier and doubles
// any embedded double-quote character, per the standard SQL quoted-
// identifier escaping rule. Table names here come from pg_tables (the
// system catalog), not directly from user input, but quoting defensively
// costs nothing and avoids relying on that being true forever.
func quotePostgresIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
