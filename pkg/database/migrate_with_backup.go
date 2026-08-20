package database

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/pkg/logger"
)

// preMigrationBackupKeep is how many pre-migration snapshots survive the
// post-success cleanup. Snapshots are small (gzipped VACUUM output) and only
// one is produced per schema upgrade, so five covers several releases of
// rollback history without unbounded growth.
const preMigrationBackupKeep = 5

// preMigrationBackupDir is a dedicated subdirectory so cleanup can never
// touch anything else — in particular the operator's own db:backup output,
// which lives wherever they pointed it (default: ./backups, timestamped
// names that don't match this scheme anyway).
func preMigrationBackupDir(sqlitePath string) string {
	return filepath.Join(filepath.Dir(sqlitePath), "backups", "pre-migration")
}

func preMigrationBackupFilename(version int64) string {
	return fmt.Sprintf("sqlite_v%d.db.gz", version)
}

// parsePreMigrationBackupVersion extracts the source schema version from a
// pre-migration backup filename, reporting ok=false for anything that
// doesn't match the exact naming scheme (such files are never cleaned up).
func parsePreMigrationBackupVersion(name string) (version int64, ok bool) {
	rest, ok := strings.CutPrefix(name, "sqlite_v")
	if !ok {
		return 0, false
	}
	numStr, ok := strings.CutSuffix(rest, ".db.gz")
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// maxMigrationVersion derives the highest migration version from the .sql
// filenames under dir in migrationsFS. Deriving it from the FS rather than
// hardcoding a number keeps "is there anything pending?" honest by
// construction whenever a migration is added.
func maxMigrationVersion(migrationsFS fs.FS, dir string) (int64, error) {
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return 0, fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	var maxVersion int64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			continue
		}
		if v > maxVersion {
			maxVersion = v
		}
	}
	if maxVersion == 0 {
		return 0, fmt.Errorf("no numbered .sql migrations found in %q", dir)
	}
	return maxVersion, nil
}

// MigrateWithBackup runs pending migrations, snapshotting the SQLite
// database first so a schema upgrade always leaves a rollback point behind
// — the in-app updater and Docker image pulls swap the binary without any
// installer-side backup, so the serve startup path is the only place this
// can happen. The caller must already hold the instance lock, which makes
// version check, backup, and migration a single critical section.
//
// The returned path is the snapshot protecting this upgrade ("" when no
// backup was needed). Behavior by situation:
//
//   - driver != sqlite: warn (only when an upgrade is actually pending —
//     otherwise there is nothing a backup would have protected) and migrate
//     directly; the official container image has no backup tooling for
//     postgres, so operators are pointed at db:backup instead.
//   - fresh database (version 0) or nothing pending: migrate directly,
//     there is no pre-upgrade state worth snapshotting.
//   - backup fails: return the error WITHOUT migrating (fail-closed); the
//     database is untouched and the process should refuse to start.
//
// Snapshots are named after the source schema version, deterministically,
// so a crash-restart loop overwrites one file instead of accumulating
// copies, and a partially-applied upgrade (version moved from N to M, then
// failed) writes sqlite_vM without disturbing the sqlite_vN recovery point.
// An existing file at the path is never trusted — it may be stale (the
// database can have been restored from it and written to since) — so every
// attempt snapshots the CURRENT database and atomically replaces the file.
// Old snapshots are pruned only after a fully successful migration, never
// including the one just taken.
func MigrateWithBackup(db *sql.DB, driver, sqlitePath string, migrationsFS fs.FS, dir string) (string, error) {
	target, err := maxMigrationVersion(migrationsFS, dir)
	if err != nil {
		return "", fmt.Errorf("determine target migration version: %w", err)
	}

	if driver != "sqlite" {
		// The pending-upgrade probe must not go through goose here: goose
		// creates goose_db_version (without IF NOT EXISTS) when it is
		// missing, and this runs before RunMigrations takes the postgres
		// advisory lock — several replicas starting at once against a fresh
		// database would race on that CREATE. A plain read-only query
		// leaves creation to the locked migration path; on any error
		// (typically: table not there yet) just skip the warning.
		if current, ok := probeCurrentVersion(db); ok && current > 0 && current < target {
			logger.Warn("automatic pre-migration backup is only available for sqlite; run db:backup manually before upgrades",
				zap.String("driver", driver))
		}
		return "", RunMigrations(db, driver, migrationsFS, dir)
	}

	// SQLite from here on: single-instance by design and the caller holds
	// the instance lock, so goose-backed reads are race-free.
	current, err := GetCurrentVersion(db, driver)
	if err != nil {
		return "", fmt.Errorf("read current migration version: %w", err)
	}
	if current == 0 || current >= target {
		return "", RunMigrations(db, driver, migrationsFS, dir)
	}

	backupPath := filepath.Join(preMigrationBackupDir(sqlitePath), preMigrationBackupFilename(current))
	if err := snapshotCurrentDatabase(sqlitePath, backupPath); err != nil {
		return "", fmt.Errorf("pre-migration backup failed, refusing to migrate: %w", err)
	}

	if err := RunMigrations(db, driver, migrationsFS, dir); err != nil {
		// Every snapshot is kept, and the one for THIS attempt is named in
		// the error: a failed startup migration is exactly the moment the
		// operator needs the recovery point, and the fatal log line built
		// from this error is the only place they will see it.
		return "", fmt.Errorf("migration failed, pre-migration snapshot preserved at %s: %w", backupPath, err)
	}
	cleanupPreMigrationBackups(preMigrationBackupDir(sqlitePath), filepath.Base(backupPath))
	return backupPath, nil
}

// probeCurrentVersion reads the current goose version with a plain query and
// no side effects, reporting ok=false when it cannot (most commonly because
// the goose table does not exist yet). goose deletes a migration's row when
// it is rolled back, so the newest applied row is the current version.
func probeCurrentVersion(db *sql.DB) (int64, bool) {
	var v int64
	if err := db.QueryRow("SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id DESC LIMIT 1").Scan(&v); err != nil {
		return 0, false
	}
	return v, true
}

// snapshotCurrentDatabase snapshots the database as it is RIGHT NOW into
// backupPath, atomically replacing whatever file may already be there. A
// pre-existing file is never reused: it can be stale (the database may have
// been restored from it and written to since), and reusing it would migrate
// without any snapshot of the current data. BackupSQLite writes to a scratch
// name (it refuses to overwrite) and the rename publishes atomically.
func snapshotCurrentDatabase(sqlitePath, backupPath string) error {
	scratch := backupPath + ".partial"
	// A crashed earlier attempt can leave scratch behind; it is ours alone,
	// so clearing it is safe.
	if err := os.Remove(scratch); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear stale snapshot scratch %s: %w", scratch, err)
	}
	if err := BackupSQLite(sqlitePath, scratch); err != nil {
		return err
	}
	if err := os.Rename(scratch, backupPath); err != nil {
		_ = os.Remove(scratch)
		return fmt.Errorf("publish snapshot to %s: %w", backupPath, err)
	}
	// BackupSQLite synced the directory while the file still had the scratch
	// name; the rename is a second directory change that needs its own sync,
	// or a power loss after the migration commits could leave the advertised
	// rollback point absent (the file data itself is already fsynced). Kept
	// best-effort like BackupSQLite's own directory sync: some filesystems
	// cannot sync directories, and refusing to start there would trade a
	// crash-window durability gap for a certain outage.
	if dir, err := os.Open(filepath.Dir(backupPath)); err == nil {
		if syncErr := dir.Sync(); syncErr != nil {
			logger.Warn("failed to sync backup directory after publishing snapshot", zap.String("path", backupPath), zap.Error(syncErr))
		}
		_ = dir.Close()
	} else {
		logger.Warn("failed to open backup directory for post-publish sync", zap.String("path", backupPath), zap.Error(err))
	}
	return nil
}

// cleanupPreMigrationBackups deletes all but the newest (by source version)
// preMigrationBackupKeep snapshots, always sparing keepName — the snapshot
// the just-finished migration ran against. Ranking by version alone would
// delete exactly that file whenever the database had been restored to an
// older schema while enough higher-version snapshots remained. Best-effort:
// a failure here must never take down a service that just migrated
// successfully, so problems are only logged. Files not matching the
// snapshot naming scheme are left alone.
func cleanupPreMigrationBackups(dirPath, keepName string) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		logger.Warn("failed to list pre-migration backups for cleanup", zap.String("dir", dirPath), zap.Error(err))
		return
	}
	type snapshot struct {
		version int64
		name    string
	}
	var snapshots []snapshot
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if v, ok := parsePreMigrationBackupVersion(e.Name()); ok {
			snapshots = append(snapshots, snapshot{version: v, name: e.Name()})
		}
	}
	if len(snapshots) <= preMigrationBackupKeep {
		return
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].version > snapshots[j].version })
	for _, s := range snapshots[preMigrationBackupKeep:] {
		if s.name == keepName {
			continue
		}
		path := filepath.Join(dirPath, s.name)
		if err := os.Remove(path); err != nil {
			logger.Warn("failed to remove old pre-migration backup", zap.String("path", path), zap.Error(err))
			continue
		}
		logger.Info("removed old pre-migration backup", zap.String("path", path))
	}
}
