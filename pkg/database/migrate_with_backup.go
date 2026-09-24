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

// preMigrationBackupKeep is how many pre-migration snapshots survive
// rotation. Rotation runs before every backup (and again after a fully
// successful migration), so the backup directory's disk usage is bounded by
// construction: at most the newest three source versions plus the pinned
// snapshot of the upgrade in flight. Three still covers rolling back across
// a couple of releases while keeping the footprint predictable on small
// system disks, where an upgrade must not compete with years of accumulated
// history for the space it needs.
const preMigrationBackupKeep = 3

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

// MigrationFailedError marks a startup upgrade-chain failure that needs an
// operator: either the pre-migration backup could not be written (nothing
// was migrated — fail-closed) or the migrations themselves failed after a
// snapshot was preserved (the snapshot is named in the message). Dispatch
// layers can detect this class with errors.As and map it to a dedicated
// exit code, distinct from generic failures, so service managers and
// operators can tell "the upgrade chain is stuck, intervene" from any other
// startup error. A disk-space precheck rejection is reported as
// *PrecheckRejectedError (its own type, same "needs an operator" class) and
// is deliberately NOT wrapped in this one.
type MigrationFailedError struct {
	// SnapshotPath is the preserved rollback point, empty when the failure
	// happened before any snapshot was taken (backup-stage failures).
	SnapshotPath string
	Err          error
}

func (e *MigrationFailedError) Error() string {
	if e.SnapshotPath == "" {
		return fmt.Sprintf("pre-migration backup failed, refusing to migrate: %s", e.Err)
	}
	return fmt.Sprintf("migration failed, pre-migration snapshot preserved at %s: %s", e.SnapshotPath, e.Err)
}

func (e *MigrationFailedError) Unwrap() error { return e.Err }

// MigrateWithBackup runs pending migrations, snapshotting the SQLite
// database first so a schema upgrade always leaves a rollback point behind
// — the in-app updater and Docker image pulls swap the binary without any
// installer-side backup, so the serve startup path is the only place this
// can happen. The caller must already hold the instance lock, which makes
// version check, backup, and migration a single critical section.
//
// freeSpace is the disk-space probe used by the pre-migration precheck;
// nil selects the OS probe. It is a parameter so tests can drive the
// boundary ("just enough", "one byte short", "probe unavailable") with
// fake values instead of a manufactured full disk.
//
// The returned path is the snapshot protecting this upgrade ("" when no
// backup was needed). Behavior by situation:
//
//   - driver != sqlite: warn (only when an upgrade is actually pending —
//     otherwise there is nothing a backup would have protected) and migrate
//     directly; the official container image has no backup tooling for
//     postgres, so operators are pointed at db:backup instead. No rotation,
//     no precheck, no backup — postgres deployments are exempt from all of
//     it.
//   - fresh database (version 0) or nothing pending: migrate directly,
//     there is no pre-upgrade state worth snapshotting or space to check.
//   - an upgrade is pending on an existing sqlite database: old snapshots
//     are rotated down to the newest preMigrationBackupKeep BEFORE anything
//     is written, pinning the file this attempt is about to produce — the
//     directory's usage stays capped and the space the rotation frees is
//     space the backup can use.
//   - disk space precheck rejects: return the *PrecheckRejectedError
//     WITHOUT writing anything; the database is untouched and the error
//     names the required and available sizes.
//   - backup fails: return a *MigrationFailedError WITHOUT migrating
//     (fail-closed); the database is untouched and the process should
//     refuse to start. The rotation has already happened at this point —
//     its deletions stand (only snapshots beyond the keep cap went, and
//     those were doomed at the next successful migration anyway).
//
// Snapshots are named after the source schema version, deterministically,
// so a crash-restart loop overwrites one file instead of accumulating
// copies, and a partially-applied upgrade (version moved from N to M, then
// failed) writes sqlite_vM without disturbing the sqlite_vN recovery point.
// An existing file at the path is never trusted — it may be stale (the
// database can have been restored from it and written to since) — so every
// attempt snapshots the CURRENT database and atomically replaces the file.
// Old snapshots are pruned by the pre-backup rotation and again after a
// fully successful migration, and the snapshot of the upgrade in flight is
// pinned through both.
func MigrateWithBackup(db *sql.DB, driver, sqlitePath string, migrationsFS fs.FS, dir string, freeSpace FreeSpaceProbe) (string, error) {
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

	backupDir := preMigrationBackupDir(sqlitePath)
	backupName := preMigrationBackupFilename(current)

	// Rotate BEFORE sizing the filesystem and writing the snapshot. This is
	// what gives the backup directory its deterministic usage cap, and the
	// space the rotation frees is space the precheck below may then count
	// on: an upgrade that fits after rotation is a real fit, not a refusal
	// the program could have solved by tidying up after itself. The pin
	// spares the file this attempt is about to (over)write — see
	// cleanupPreMigrationBackups for why ranking by version alone would
	// drop exactly that file on a database restored to an older schema.
	cleanupPreMigrationBackups(backupDir, backupName)

	// Last gate before writing anything: a filesystem that cannot fit one
	// more backup would fail halfway through writing it — a slow, noisy way
	// to learn the same thing, and on an auto-restarting service a way to
	// loop on it. Refused here, nothing has been written yet, and the error
	// carries exact numbers plus what to do about it. Unavailable probes
	// pass through (PrecheckMigrationDiskSpace never fails on its own).
	if err := PrecheckMigrationDiskSpace(sqlitePath, freeSpace); err != nil {
		return "", err
	}

	backupPath := filepath.Join(backupDir, backupName)
	if err := snapshotCurrentDatabase(sqlitePath, backupPath); err != nil {
		return "", &MigrationFailedError{Err: err}
	}

	if err := RunMigrations(db, driver, migrationsFS, dir); err != nil {
		// The one for THIS attempt survived the rotation (it was the pin)
		// and is named in the error: a failed startup migration is exactly
		// the moment the operator needs the recovery point, and the fatal
		// log line built from this error is the only place they will see it.
		return "", &MigrationFailedError{SnapshotPath: backupPath, Err: err}
	}
	// The same rotation that ran before the backup, once more after success:
	// by now the just-taken snapshot is the pin (the one the migration ran
	// against), and anything that escaped the pre-backup pass (say, files
	// created meanwhile) gets the same cap applied. Structurally it usually
	// finds nothing left to do — the pre-backup pass already trimmed to the
	// cap — but keeping it makes the cap a property of the cleanup itself
	// rather than of where it was last invoked.
	cleanupPreMigrationBackups(backupDir, backupName)
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
// of the upgrade in flight: before the backup it is the file this attempt is
// about to (over)write, after success the one the migration just ran
// against. Ranking by version alone would delete exactly that file whenever
// the database had been restored to an older schema while enough
// higher-version snapshots remained. Best-effort: a failure here must never
// take down a service — the rotation runs before backups on the startup path
// and the cleanup after a migration that just succeeded — so problems are
// only logged. Files not matching the snapshot naming scheme are left
// alone, and a missing directory (the first upgrade of a deployment, before
// any backup exists) is silence, not a warning.
//
// It is a package-level variable rather than a plain function so tests can
// substitute a call-recording fake: the real cleanup only reads and deletes,
// so against an empty or missing directory it is a silent no-op, and no
// filesystem-shape assertion can detect a call that must not happen. The
// postgres exemption guards observe the call itself through this seam, and
// every cleanup call site (pre-backup rotation, post-success cleanup, and
// the update-button preflight) must call through it too rather than
// growing a private side path around it.
var cleanupPreMigrationBackups = func(dirPath, keepName string) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			// The pre-backup rotation reaches a not-yet-created directory
			// exactly once per deployment, on its first upgrade — there is
			// nothing to rotate and nothing wrong.
			return
		}
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
