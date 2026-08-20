package database

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// testMigrationsUpTo builds an in-memory goose migration set with versions
// 1..n, each creating its own table (t1, t2, ...), rooted under the "sqlite"
// subdirectory to mirror how the real embedded FS is laid out.
func testMigrationsUpTo(n int) fstest.MapFS {
	m := fstest.MapFS{}
	for v := 1; v <= n; v++ {
		sql := fmt.Sprintf("-- +goose Up\nCREATE TABLE t%d (id INTEGER PRIMARY KEY);\n-- +goose Down\nDROP TABLE t%d;\n", v, v)
		m[fmt.Sprintf("sqlite/%05d_table_v%d.sql", v, v)] = &fstest.MapFile{Data: []byte(sql)}
	}
	return m
}

// breakVersion replaces one migration's SQL with a statement that cannot
// execute, so goose fails exactly at that version.
func breakVersion(m fstest.MapFS, version int) {
	m[fmt.Sprintf("sqlite/%05d_table_v%d.sql", version, version)] = &fstest.MapFile{
		Data: []byte("-- +goose Up\nTHIS IS NOT SQL;\n-- +goose Down\nSELECT 1;\n"),
	}
}

// stageDB creates a fresh data directory holding a file-backed SQLite
// database migrated up to version v (0 = untouched fresh database), the
// starting state every test here needs.
func stageDB(t *testing.T, v int) (db *sql.DB, sqlitePath string) {
	t.Helper()
	sqlitePath = filepath.Join(t.TempDir(), "yolorouter.db")
	db = newTestDB(t, sqlitePath)
	if v > 0 {
		if err := RunMigrations(db, "sqlite", testMigrationsUpTo(v), "sqlite"); err != nil {
			t.Fatalf("stage db at v%d: %v", v, err)
		}
	}
	return db, sqlitePath
}

func mustVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	v, err := GetCurrentVersion(db, "sqlite")
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	return v
}

func fileDigest(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256.Sum256(data)
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mustWriteGzip plants a complete, valid gzip file at path — the shape a
// pre-existing snapshot must have to pass the reuse verification.
func mustWriteGzip(t *testing.T, path string, payload []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(payload); err != nil {
		t.Fatalf("gzip payload: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	mustWriteFile(t, path, buf.Bytes())
}

// gunzipToFile decompresses a .gz backup so it can be opened as a plain
// SQLite database file.
func gunzipToFile(t *testing.T, gzPath, dstPath string) {
	t.Helper()
	src, err := os.Open(gzPath)
	if err != nil {
		t.Fatalf("open %s: %v", gzPath, err)
	}
	defer func() { _ = src.Close() }()
	gz, err := gzip.NewReader(src)
	if err != nil {
		t.Fatalf("gzip reader for %s: %v", gzPath, err)
	}
	dst, err := os.Create(dstPath)
	if err != nil {
		t.Fatalf("create %s: %v", dstPath, err)
	}
	if _, err := io.Copy(dst, gz); err != nil { //nolint:gosec // trusted test artifact, no decompression-bomb concern
		t.Fatalf("decompress %s: %v", gzPath, err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close %s: %v", dstPath, err)
	}
}

func TestMigrateWithBackupProducesPreMigrationSnapshot(t *testing.T) {
	db, sqlitePath := stageDB(t, 1)
	dataDir := filepath.Dir(sqlitePath)

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite")
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	wantPath := filepath.Join(dataDir, "backups", "pre-migration", "sqlite_v1.db.gz")
	if backupPath != wantPath {
		t.Fatalf("backup path = %q, want %q", backupPath, wantPath)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	if got := mustVersion(t, db); got != 2 {
		t.Fatalf("db version after migrate = %d, want 2", got)
	}

	// The snapshot must capture the PRE-migration state: goose version still 1
	// and the v2 table absent. A snapshot taken after migrating would be
	// useless as a rollback point.
	snapPath := filepath.Join(t.TempDir(), "snapshot.db")
	gunzipToFile(t, backupPath, snapPath)
	snapDB := newTestDB(t, snapPath)
	if got := mustVersion(t, snapDB); got != 1 {
		t.Fatalf("snapshot goose version = %d, want 1 (pre-migration state)", got)
	}
	var name string
	if err := snapDB.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='t2'").Scan(&name); err == nil {
		t.Fatal("snapshot contains table t2 — backup was taken after migrating, not before")
	}
}

func TestMigrateWithBackupFailClosedWhenBackupFails(t *testing.T) {
	db, sqlitePath := stageDB(t, 1)
	dataDir := filepath.Dir(sqlitePath)
	// Occupy the backup directory's path with a regular file so creating the
	// backup is impossible.
	mustWriteFile(t, filepath.Join(dataDir, "backups", "pre-migration"), []byte("in the way"))

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite")
	if err == nil {
		t.Fatal("expected an error when the backup cannot be written")
	}
	if backupPath != "" {
		t.Fatalf("backup path = %q, want empty on failure", backupPath)
	}
	// Fail-closed: the migration must NOT have run.
	if got := mustVersion(t, db); got != 1 {
		t.Fatalf("db version after failed backup = %d, want 1 (migration must not run)", got)
	}
}

// TestMigrateWithBackupRefreshesExistingSnapshot: a file already at the
// snapshot path may be stale — the classic case is a database restored from
// that very snapshot and then written to before the next upgrade attempt.
// Trusting it by name would migrate without any snapshot of the CURRENT
// data, so an existing file must be replaced with a fresh snapshot, never
// reused.
func TestMigrateWithBackupRefreshesExistingSnapshot(t *testing.T) {
	db, sqlitePath := stageDB(t, 1)
	dataDir := filepath.Dir(sqlitePath)
	existing := filepath.Join(dataDir, "backups", "pre-migration", "sqlite_v1.db.gz")
	mustWriteGzip(t, existing, []byte("stale snapshot from an earlier upgrade"))
	before := fileDigest(t, existing)

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite")
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	if backupPath != existing {
		t.Fatalf("backup path = %q, want %q", backupPath, existing)
	}
	if after := fileDigest(t, existing); after == before {
		t.Fatal("existing snapshot was reused as-is — it must be replaced with a snapshot of the current database")
	}
	// The replacement must be a real snapshot of the current pre-migration
	// state, not just any new bytes.
	snapPath := filepath.Join(t.TempDir(), "snapshot.db")
	gunzipToFile(t, existing, snapPath)
	if got := mustVersion(t, newTestDB(t, snapPath)); got != 1 {
		t.Fatalf("refreshed snapshot goose version = %d, want 1", got)
	}
	if got := mustVersion(t, db); got != 2 {
		t.Fatalf("db version after migrate = %d, want 2", got)
	}
}

// A corrupt or foreign file at the snapshot path is simply replaced — the
// fresh snapshot supersedes whatever was there.
func TestMigrateWithBackupReplacesCorruptExistingSnapshot(t *testing.T) {
	db, sqlitePath := stageDB(t, 1)
	dataDir := filepath.Dir(sqlitePath)
	corrupt := filepath.Join(dataDir, "backups", "pre-migration", "sqlite_v1.db.gz")
	mustWriteFile(t, corrupt, []byte("not a gzip stream"))

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite")
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	snapPath := filepath.Join(t.TempDir(), "snapshot.db")
	gunzipToFile(t, backupPath, snapPath)
	if got := mustVersion(t, newTestDB(t, snapPath)); got != 1 {
		t.Fatalf("replacement snapshot goose version = %d, want 1", got)
	}
	if got := mustVersion(t, db); got != 2 {
		t.Fatalf("db version after migrate = %d, want 2", got)
	}
}

func TestMigrateWithBackupRefusesDirectoryAtSnapshotPath(t *testing.T) {
	db, sqlitePath := stageDB(t, 1)
	dataDir := filepath.Dir(sqlitePath)
	if err := os.MkdirAll(filepath.Join(dataDir, "backups", "pre-migration", "sqlite_v1.db.gz"), 0o700); err != nil {
		t.Fatalf("plant directory: %v", err)
	}

	if _, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite"); err == nil {
		t.Fatal("expected an error when a directory occupies the snapshot path")
	}
	if got := mustVersion(t, db); got != 1 {
		t.Fatalf("db version = %d, want 1 (migration must not run)", got)
	}
}

func TestMigrateWithBackupPreservesOlderRecoveryPoints(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	dataDir := filepath.Dir(sqlitePath)
	older := filepath.Join(dataDir, "backups", "pre-migration", "sqlite_v1.db.gz")
	mustWriteFile(t, older, []byte("original pre-upgrade snapshot"))
	before := fileDigest(t, older)

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(3), "sqlite")
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	if want := filepath.Join(dataDir, "backups", "pre-migration", "sqlite_v2.db.gz"); backupPath != want {
		t.Fatalf("backup path = %q, want %q", backupPath, want)
	}
	if after := fileDigest(t, older); after != before {
		t.Fatal("older recovery point was modified — each source version must get its own file")
	}
}

func TestMigrateWithBackupSkipsWhenNothingPending(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	dataDir := filepath.Dir(sqlitePath)

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite")
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	if backupPath != "" {
		t.Fatalf("backup path = %q, want empty when nothing is pending", backupPath)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "backups")); !os.IsNotExist(err) {
		t.Fatalf("expected no backups directory when nothing is pending, stat err = %v", err)
	}
}

func TestMigrateWithBackupSkipsFreshDatabase(t *testing.T) {
	db, sqlitePath := stageDB(t, 0)
	dataDir := filepath.Dir(sqlitePath)

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite")
	if err != nil {
		t.Fatalf("MigrateWithBackup on fresh db: %v", err)
	}
	if backupPath != "" {
		t.Fatalf("backup path = %q, want empty for a fresh database", backupPath)
	}
	if got := mustVersion(t, db); got != 2 {
		t.Fatalf("db version after migrate = %d, want 2", got)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "backups")); !os.IsNotExist(err) {
		t.Fatalf("expected no backups directory for a fresh database, stat err = %v", err)
	}
}

func TestMigrateWithBackupCleansUpOnlyAfterSuccess(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	dataDir := filepath.Dir(sqlitePath)
	backupDir := filepath.Join(dataDir, "backups", "pre-migration")
	for v := 1; v <= 6; v++ {
		mustWriteGzip(t, filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)), fmt.Appendf(nil, "snapshot v%d", v))
	}
	// A file that doesn't match the naming scheme must never be touched.
	unrelated := filepath.Join(backupDir, "manual-copy.db.gz")
	mustWriteFile(t, unrelated, []byte("operator's own file"))

	if _, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(3), "sqlite"); err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	// 6 versioned files, keep the newest 5: v1 goes, v2..v6 stay.
	if _, err := os.Stat(filepath.Join(backupDir, "sqlite_v1.db.gz")); !os.IsNotExist(err) {
		t.Fatalf("expected sqlite_v1.db.gz to be cleaned up, stat err = %v", err)
	}
	for v := 2; v <= 6; v++ {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); err != nil {
			t.Fatalf("expected sqlite_v%d.db.gz to survive cleanup: %v", v, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated file must never be cleaned up: %v", err)
	}
}

// TestMigrateWithBackupCleanupNeverDeletesJustUsedSnapshot: after a database
// is restored to an OLDER schema while several higher-version snapshots
// remain, keep-newest-by-version alone would rank the snapshot just taken
// below the survivors and delete it — returning a path that no longer
// exists. The just-used snapshot must be pinned through cleanup.
func TestMigrateWithBackupCleanupNeverDeletesJustUsedSnapshot(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	backupDir := filepath.Join(filepath.Dir(sqlitePath), "backups", "pre-migration")
	for v := 3; v <= 8; v++ {
		mustWriteGzip(t, filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)), fmt.Appendf(nil, "snapshot v%d", v))
	}

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(3), "sqlite")
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	if want := filepath.Join(backupDir, "sqlite_v2.db.gz"); backupPath != want {
		t.Fatalf("backup path = %q, want %q", backupPath, want)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("the just-used snapshot was deleted by cleanup: %v", err)
	}
	// Among the non-pinned snapshots the newest five (v4..v8) survive and v3
	// is pruned as usual.
	if _, err := os.Stat(filepath.Join(backupDir, "sqlite_v3.db.gz")); !os.IsNotExist(err) {
		t.Fatalf("expected sqlite_v3.db.gz to be cleaned up, stat err = %v", err)
	}
	for v := 4; v <= 8; v++ {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); err != nil {
			t.Fatalf("expected sqlite_v%d.db.gz to survive cleanup: %v", v, err)
		}
	}
}

func TestMigrateWithBackupKeepsEverythingWhenMigrationFails(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	dataDir := filepath.Dir(sqlitePath)
	backupDir := filepath.Join(dataDir, "backups", "pre-migration")
	for v := 1; v <= 6; v++ {
		mustWriteGzip(t, filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)), fmt.Appendf(nil, "snapshot v%d", v))
	}

	broken := testMigrationsUpTo(3)
	breakVersion(broken, 3)
	_, err := MigrateWithBackup(db, "sqlite", sqlitePath, broken, "sqlite")
	if err == nil {
		t.Fatal("expected the broken migration to fail")
	}
	// The startup outage this causes is exactly when the operator needs the
	// snapshot: its location must be in the error they will see in the logs.
	if !strings.Contains(err.Error(), filepath.Join(backupDir, "sqlite_v2.db.gz")) {
		t.Fatalf("migration failure does not surface the snapshot path: %v", err)
	}
	// Failure path must never delete a recovery point, however many there are.
	for v := 1; v <= 6; v++ {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); err != nil {
			t.Fatalf("sqlite_v%d.db.gz must survive a failed migration: %v", v, err)
		}
	}
}

func TestMigrateWithBackupPostgresSkipsBackup(t *testing.T) {
	dataDir := t.TempDir()
	// The sqlite path deliberately points at a file that doesn't exist: the
	// postgres branch must not touch the filesystem at all.
	sqlitePath := filepath.Join(dataDir, "unused.db")
	db := newMemoryDB(t)

	// The in-memory SQLite connection stands in for postgres just to prove
	// delegation: pg_advisory_lock doesn't exist in SQLite, so reaching the
	// migration path fails with a driver-level error — which is exactly the
	// evidence that MigrateWithBackup skipped backup and went straight to
	// migrating.
	backupPath, err := MigrateWithBackup(db, "postgres", sqlitePath, testMigrationsUpTo(1), "sqlite")
	if err == nil {
		t.Fatal("expected the fake postgres connection to fail inside migration")
	}
	// The stand-in connection cannot satisfy the version probe (no goose
	// table), and that must not block migrating: the error must come from
	// inside the migration path (the advisory-lock statement is the first
	// thing it runs), not from a failed pre-read.
	if !strings.Contains(err.Error(), "pg_advisory_lock") {
		t.Fatalf("migration path was never reached — a failed version probe must not block migration: %v", err)
	}
	if backupPath != "" {
		t.Fatalf("backup path = %q, want empty for postgres", backupPath)
	}
	entries, readErr := os.ReadDir(dataDir)
	if readErr != nil {
		t.Fatalf("read data dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("postgres branch wrote to the filesystem: %v", entries)
	}
	// The version pre-read must not touch the database outside RunMigrations'
	// advisory-lock critical section: goose creates goose_db_version without
	// IF NOT EXISTS, so on a fresh database with several replicas starting at
	// once, creating it before the lock races. The stand-in connection proves
	// the property: no goose table may exist after the failed call.
	var name string
	if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='goose_db_version'").Scan(&name); err == nil {
		t.Fatal("goose_db_version was created before the migration critical section — concurrent fresh-database postgres starts would race here")
	}
}

func TestProbeCurrentVersion(t *testing.T) {
	db := newMemoryDB(t)
	if v, ok := probeCurrentVersion(db); ok {
		t.Fatalf("probe reported version %d on a database with no goose table", v)
	}
	if err := RunMigrations(db, "sqlite", testMigrationsUpTo(2), "sqlite"); err != nil {
		t.Fatalf("stage db at v2: %v", err)
	}
	v, ok := probeCurrentVersion(db)
	if !ok {
		t.Fatal("probe failed on a migrated database")
	}
	if v != 2 {
		t.Fatalf("probe version = %d, want 2", v)
	}
}

func TestMaxMigrationVersion(t *testing.T) {
	m := fstest.MapFS{
		"sqlite/00001_a.sql":  &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
		"sqlite/00012_b.sql":  &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
		"sqlite/00003_c.sql":  &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
		"sqlite/README.md":    &fstest.MapFile{Data: []byte("not a migration")},
		"sqlite/notnum_x.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
	}
	got, err := maxMigrationVersion(m, "sqlite")
	if err != nil {
		t.Fatalf("maxMigrationVersion: %v", err)
	}
	if got != 12 {
		t.Fatalf("max version = %d, want 12", got)
	}

	empty := fstest.MapFS{"sqlite/README.md": &fstest.MapFile{Data: []byte("x")}}
	if _, err := maxMigrationVersion(empty, "sqlite"); err == nil {
		t.Fatal("expected an error when no numbered .sql migrations exist")
	}
}
