package database

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/pkg/logger"
)

// pgAdvisoryLockKey is an arbitrary fixed key used to serialize migrations
// across concurrently-starting PostgreSQL instances (design doc §6). SQLite
// deployments are single-instance by design and don't need this.
const pgAdvisoryLockKey = 8821_0714 // yolorouter, no particular significance beyond being a fixed constant

// RunMigrations executes pending migrations for the given driver against db,
// reading .sql files from the embedded migrationsFS (design doc §9 — the
// single binary must be self-contained, so migrations are compiled in
// rather than read from a path relative to the process's working directory;
// see migrations.SQLiteFS / migrations.PostgresFS). dir is the
// subdirectory within migrationsFS to read (goose.SetBaseFS roots all
// migration discovery at migrationsFS). On failure it returns a wrapped
// error with driver context; the caller (cmd/api) treats this as fatal and
// refuses to start (design doc §6"迁移执行时机").
func RunMigrations(db *sql.DB, driver string, migrationsFS fs.FS, dir string) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}

	cleanup, err := prepareGoose(driver, migrationsFS)
	if err != nil {
		return err
	}
	defer cleanup()

	logger.Info("running database migrations", zap.String("driver", driver))

	if driver == "postgres" {
		unlock, lockErr := acquirePostgresAdvisoryLock(context.Background(), db)
		if lockErr != nil {
			return fmt.Errorf("acquire postgres migration advisory lock: %w", lockErr)
		}
		defer unlock()
	}

	if err := goose.Up(db, dir); err != nil {
		return fmt.Errorf("run migrations (driver=%s): %w", driver, err)
	}

	logger.Info("database migrations completed successfully")
	return nil
}

// GetCurrentVersion returns the current goose migration version. It sets the
// goose dialect based on driver before querying — goose defaults to
// Postgres dialect, so SQLite callers MUST go through this function rather
// than calling goose.GetDBVersion directly.
func GetCurrentVersion(db *sql.DB, driver string) (int64, error) {
	dialect, err := gooseDialect(driver)
	if err != nil {
		return 0, err
	}
	if err := goose.SetDialect(dialect); err != nil {
		return 0, fmt.Errorf("set goose dialect %q: %w", dialect, err)
	}
	return goose.GetDBVersion(db)
}

// RollbackTo rolls back to the given version (0 means roll back everything).
func RollbackTo(db *sql.DB, driver string, migrationsFS fs.FS, dir string, version int64) error {
	cleanup, err := prepareGoose(driver, migrationsFS)
	if err != nil {
		return err
	}
	defer cleanup()
	return goose.DownTo(db, dir, version)
}

// Rollback rolls back exactly one migration.
func Rollback(db *sql.DB, driver string, migrationsFS fs.FS, dir string) error {
	cleanup, err := prepareGoose(driver, migrationsFS)
	if err != nil {
		return err
	}
	defer cleanup()
	return goose.Down(db, dir)
}

// remigrate re-runs all pending migrations after a db:reset has dropped
// every table. Shared by ResetSQLite and ResetPostgres.
func remigrate(db *sql.DB, driver string, migrationsFS fs.FS, dir string) error {
	if err := RunMigrations(db, driver, migrationsFS, dir); err != nil {
		return fmt.Errorf("re-migrate after reset: %w", err)
	}
	return nil
}

func gooseDialect(driver string) (string, error) {
	switch driver {
	case "postgres":
		return "postgres", nil
	case "sqlite":
		return "sqlite3", nil
	default:
		return "", fmt.Errorf("unsupported database driver %q (must be sqlite or postgres)", driver)
	}
}

// prepareGoose sets the goose dialect for driver and points goose's base FS
// at migrationsFS, returning a cleanup func that resets the base FS
// afterward. RunMigrations, RollbackTo, and Rollback all need this same
// preamble before calling their respective goose function.
func prepareGoose(driver string, migrationsFS fs.FS) (cleanup func(), err error) {
	dialect, err := gooseDialect(driver)
	if err != nil {
		return nil, err
	}
	if err := goose.SetDialect(dialect); err != nil {
		return nil, fmt.Errorf("set goose dialect %q: %w", dialect, err)
	}
	goose.SetBaseFS(migrationsFS)
	return func() { goose.SetBaseFS(nil) }, nil
}

// acquirePostgresAdvisoryLock blocks until this process holds the
// session-level advisory lock, so that if several instances start at once
// against the same PostgreSQL database, only one runs migrations at a time
// and the rest wait for the lock instead of racing (design doc §6).
//
// pg_advisory_lock/pg_advisory_unlock are session-scoped: the lock and its
// release must happen on the exact same physical connection, or PostgreSQL
// just returns false for the unlock (not an error) while the connection
// that actually holds the lock goes back into the pool still holding it
// forever. An earlier version tried to guarantee this by temporarily
// capping the whole pool to one open connection (db.SetMaxOpenConns(1)) —
// but that's the wrong tool: per the stdlib's own documented behavior,
// shrinking MaxOpenConns below the current MaxIdleConns silently shrinks
// MaxIdleConns too, and restoring only MaxOpenConns afterward left the pool
// permanently degraded to one idle connection for the rest of the
// process's life (codex adversarial review, round 5). db.Conn acquires one
// specific *sql.Conn from the pool and guarantees every call through it
// hits that same session until Close — exactly what's needed here — without
// touching the shared pool's settings at all. goose's own migration
// queries run through db (the pool) as before; they don't need to share
// this connection, only other sessions attempting the same advisory lock
// key do, and this dedicated Conn is the only thing they contend with.
func acquirePostgresAdvisoryLock(ctx context.Context, db *sql.DB) (unlock func(), err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", pgAdvisoryLockKey); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return func() {
		var released bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", pgAdvisoryLockKey).Scan(&released); err != nil {
			logger.Error("failed to release postgres migration advisory lock", zap.Error(err))
		} else if !released {
			// Shouldn't happen — conn is the same session that acquired the
			// lock — but if it ever does, the lock is stuck until this
			// connection closes; surface it loudly either way.
			logger.Error("postgres migration advisory lock unlock returned false: lock was not held by this session, it may now be stuck")
		}
		if err := conn.Close(); err != nil {
			logger.Error("failed to close postgres advisory lock connection", zap.Error(err))
		}
	}, nil
}
