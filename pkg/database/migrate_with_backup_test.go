package database

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
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

// unlimitedSpaceProbe is a FreeSpaceProbe that always reports a practically
// infinite filesystem, letting the backup/migration behavior tests run
// undisturbed by (and independent of) the disk-space gate. The gate itself
// has its own boundary tests with fake values in disk_precheck_test.go.
func unlimitedSpaceProbe(string) (int64, error) {
	return math.MaxInt64, nil
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

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", unlimitedSpaceProbe)
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

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", unlimitedSpaceProbe)
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

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", unlimitedSpaceProbe)
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

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", unlimitedSpaceProbe)
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

	if _, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", unlimitedSpaceProbe); err == nil {
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

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(3), "sqlite", unlimitedSpaceProbe)
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

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", unlimitedSpaceProbe)
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

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", unlimitedSpaceProbe)
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

// TestMigrateWithBackupCapsHistoryAtThreeAfterSuccess: a fully successful
// upgrade leaves the backup directory at the keep cap — the ordinary
// forward-upgrade shape here (the snapshot's source version is the newest)
// ends with exactly preMigrationBackupKeep versioned files, not one more
// with each release. A file that doesn't match the naming scheme must never
// be touched by any of the cleanup passes.
func TestMigrateWithBackupCapsHistoryAtThreeAfterSuccess(t *testing.T) {
	db, sqlitePath := stageDB(t, 4)
	dataDir := filepath.Dir(sqlitePath)
	backupDir := filepath.Join(dataDir, "backups", "pre-migration")
	// Genuine history from earlier upgrades: v1..v4, the database itself at
	// v4, migrating to v5.
	for v := 1; v <= 4; v++ {
		mustWriteGzip(t, filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)), fmt.Appendf(nil, "snapshot v%d", v))
	}
	// A file that doesn't match the naming scheme must never be touched.
	unrelated := filepath.Join(backupDir, "manual-copy.db.gz")
	mustWriteFile(t, unrelated, []byte("operator's own file"))

	if _, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(5), "sqlite", unlimitedSpaceProbe); err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	// Four versioned files rotate down to the newest three: v1 goes, and the
	// fresh v4 snapshot (which replaced the stale v4 file) joins v2 and v3.
	if _, err := os.Stat(filepath.Join(backupDir, "sqlite_v1.db.gz")); !os.IsNotExist(err) {
		t.Fatalf("expected sqlite_v1.db.gz to be cleaned up, stat err = %v", err)
	}
	for v := 2; v <= 4; v++ {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); err != nil {
			t.Fatalf("expected sqlite_v%d.db.gz to survive: %v", v, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated file must never be cleaned up: %v", err)
	}
	if got := mustVersion(t, db); got != 5 {
		t.Fatalf("db version after migrate = %d, want 5", got)
	}
}

// TestMigrateWithBackupCleanupNeverDeletesJustUsedSnapshot: after a database
// is restored to an OLDER schema while several higher-version snapshots
// remain, keep-newest-by-version alone would rank the snapshot just taken
// below the survivors and delete it — returning a path that no longer
// exists. The just-used snapshot must be pinned through BOTH cleanup passes
// (the pre-backup rotation and the post-success one) that now each rank it
// below the cap.
func TestMigrateWithBackupCleanupNeverDeletesJustUsedSnapshot(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	backupDir := filepath.Join(filepath.Dir(sqlitePath), "backups", "pre-migration")
	for v := 3; v <= 8; v++ {
		mustWriteGzip(t, filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)), fmt.Appendf(nil, "snapshot v%d", v))
	}

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(3), "sqlite", unlimitedSpaceProbe)
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	if want := filepath.Join(backupDir, "sqlite_v2.db.gz"); backupPath != want {
		t.Fatalf("backup path = %q, want %q", backupPath, want)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("the just-used snapshot was deleted by cleanup: %v", err)
	}
	// Six planted snapshots (v3..v8) rotate down to the newest three before
	// the backup: v3, v4 and v5 go, v6..v8 stay; the pinned v2 snapshot then
	// survives the post-success pass as the fourth file (cap + pin).
	for _, v := range []int{3, 4, 5} {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); !os.IsNotExist(err) {
			t.Fatalf("expected sqlite_v%d.db.gz to be cleaned up, stat err = %v", v, err)
		}
	}
	for v := 6; v <= 8; v++ {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); err != nil {
			t.Fatalf("expected sqlite_v%d.db.gz to survive cleanup: %v", v, err)
		}
	}
}

// TestMigrateWithBackupRotatesBeforeBackup: an upgrade with five old
// snapshots of distinct source versions in place starts by rotating the
// directory down to the newest three — ranked by SOURCE VERSION descending,
// not by write order (the files are planted in scrambled order and all share
// the same timestamp granularity). The fresh snapshot then lands as the
// fourth file, so the end state is exactly "three old + this one": bounded
// by construction, whatever the upgrade history before it.
func TestMigrateWithBackupRotatesBeforeBackup(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	backupDir := filepath.Join(filepath.Dir(sqlitePath), "backups", "pre-migration")
	// Planted in scrambled order: v9, v3, v7, v1, v5. Version ranking must
	// keep v9/v7/v5 and drop v3/v1 regardless of either write order or name
	// sort. The database sits at v2, below every planted version (a restored
	// schema), so this attempt's own file cannot be confused with the
	// survivors.
	for _, v := range []int{9, 3, 7, 1, 5} {
		mustWriteGzip(t, filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)), fmt.Appendf(nil, "snapshot v%d", v))
	}

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(3), "sqlite", unlimitedSpaceProbe)
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	if got := mustVersion(t, db); got != 3 {
		t.Fatalf("db version after migrate = %d, want 3", got)
	}
	// The two oldest by version are gone before the new backup lands.
	for _, v := range []int{1, 3} {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); !os.IsNotExist(err) {
			t.Fatalf("expected sqlite_v%d.db.gz to be rotated away, stat err = %v", v, err)
		}
	}
	// The newest three by version survive, and this attempt's snapshot is a
	// real, fresh pre-migration snapshot (v2 state) joining them.
	for _, v := range []int{5, 7, 9} {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); err != nil {
			t.Fatalf("expected sqlite_v%d.db.gz to survive rotation: %v", v, err)
		}
	}
	if want := filepath.Join(backupDir, "sqlite_v2.db.gz"); backupPath != want {
		t.Fatalf("backup path = %q, want %q", backupPath, want)
	}
	snapPath := filepath.Join(t.TempDir(), "snapshot.db")
	gunzipToFile(t, backupPath, snapPath)
	if got := mustVersion(t, newTestDB(t, snapPath)); got != 2 {
		t.Fatalf("fresh snapshot goose version = %d, want 2 (pre-migration state)", got)
	}
}

// TestMigrateWithBackupRotationPinsRestoredSnapshot: the pre-backup rotation
// must pin the file this attempt is ABOUT to write. The observation point is
// a rejecting space probe: the rotation runs before the precheck, the
// precheck then refuses without writing anything, so the directory state is
// exactly what the rotation left behind. Five files are planted with the
// database restored to v2: the rotation applies the cap (v5, the fourth by
// version, goes) while sparing the pinned v2 file that version ranking alone
// would rank fifth and delete. The trimmed v5 also proves the rotation
// happened BEFORE the rejection — a rotation moved after the precheck would
// leave it in place.
func TestMigrateWithBackupRotationPinsRestoredSnapshot(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	backupDir := filepath.Join(filepath.Dir(sqlitePath), "backups", "pre-migration")
	// Five files, the restored-to v2 file ranking below all others: without
	// a pin the rotation deletes v5 AND v2.
	for _, v := range []int{5, 6, 7, 8, 2} {
		mustWriteGzip(t, filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)), fmt.Appendf(nil, "snapshot v%d", v))
	}
	p := &recordingSpaceProbe{free: 0} // rejects instantly

	_, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(3), "sqlite", p.probe)
	var rejected *PrecheckRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected the precheck rejection to stop the run, got %v", err)
	}
	// The rotation already ran (before the precheck): the cap was applied...
	if _, err := os.Stat(filepath.Join(backupDir, "sqlite_v5.db.gz")); !os.IsNotExist(err) {
		t.Fatalf("expected sqlite_v5.db.gz to be rotated away before the rejection, stat err = %v", err)
	}
	// ...the pinned v2 file survived the ranking that would have dropped it...
	for _, v := range []int{2, 6, 7, 8} {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); err != nil {
			t.Fatalf("sqlite_v%d.db.gz must survive the rotation: %v", v, err)
		}
	}
	// ...and nothing ran past the rejection.
	if got := mustVersion(t, db); got != 2 {
		t.Fatalf("db version = %d, want 2 (nothing may run past the rejection)", got)
	}
}

// TestMigrateWithBackupRotationSurvivesBackupFailure: when the backup itself
// fails mid-attempt, the rotation has already done its work — the directory
// holds the newest three, not the five that were there before. The failure
// is injected by parking a directory at the snapshot path: the backup writes
// its scratch file fine and then cannot publish over a directory, failing
// late in the backup stage with everything cleaned up behind it. This is the
// revised failure judge (the rotation runs BEFORE the hands-on backup,
// deletions stand); the old "a failed backup leaves everything untouched"
// assertion is retired.
func TestMigrateWithBackupRotationSurvivesBackupFailure(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	backupDir := filepath.Join(filepath.Dir(sqlitePath), "backups", "pre-migration")
	for _, v := range []int{1, 3, 4, 5, 6} {
		mustWriteGzip(t, filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)), fmt.Appendf(nil, "snapshot v%d", v))
	}
	// The blocking directory at the snapshot path this attempt would use.
	if err := os.MkdirAll(filepath.Join(backupDir, "sqlite_v2.db.gz"), 0o700); err != nil {
		t.Fatalf("plant blocking directory: %v", err)
	}

	_, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(3), "sqlite", unlimitedSpaceProbe)
	var mf *MigrationFailedError
	if !errors.As(err, &mf) {
		t.Fatalf("expected a backup-stage *MigrationFailedError, got %T: %v", err, err)
	}
	if mf.SnapshotPath != "" {
		t.Fatalf("SnapshotPath = %q, want empty (no snapshot was published)", mf.SnapshotPath)
	}
	if got := mustVersion(t, db); got != 2 {
		t.Fatalf("db version = %d, want 2 (fail-closed: the migration must not run)", got)
	}
	// Exactly the newest three snapshots remain, plus the blocking directory
	// (never a cleanup candidate — directories don't match the naming
	// scheme) and no scratch leftovers from the failed attempt.
	entries, readErr := os.ReadDir(backupDir)
	if readErr != nil {
		t.Fatalf("read backup dir: %v", readErr)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := []string{"sqlite_v2.db.gz", "sqlite_v4.db.gz", "sqlite_v5.db.gz", "sqlite_v6.db.gz"}
	if !slices.Equal(got, want) {
		t.Fatalf("backup dir after failed backup = %v, want %v (newest three rotated in, oldest two gone, no scratch)", got, want)
	}
}

// TestMigrateWithBackupCleanupRunsBeforeBackupAndAfterSuccess pins the two
// startup-layer cleanup call sites through the var seam: once BEFORE the
// backup (pin = the snapshot this attempt is about to write) and once after
// a fully successful migration (pin = the snapshot it just used). Both calls
// go through the same package-level seam the postgres exemption guard
// observes, so a call site growing around it fails here first.
func TestMigrateWithBackupCleanupRunsBeforeBackupAndAfterSuccess(t *testing.T) {
	db, sqlitePath := stageDB(t, 1)
	calls := recordingCleanup(t)

	if _, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", unlimitedSpaceProbe); err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	wantDir := preMigrationBackupDir(sqlitePath)
	if len(*calls) != 2 {
		t.Fatalf("cleanup called %d time(s) on a successful upgrade, want 2 (before the backup and after success): %v", len(*calls), *calls)
	}
	for i, want := range []string{"before the backup", "after success"} {
		if (*calls)[i].dir != wantDir || (*calls)[i].keep != "sqlite_v1.db.gz" {
			t.Fatalf("cleanup call %d (%s) = %+v, want {dir:%s keep:sqlite_v1.db.gz}", i, want, (*calls)[i], wantDir)
		}
	}
}

// TestMigrateWithBackupMigrationFailureLeavesCappedHistoryPlusPin: when the
// migration itself fails (after a successful snapshot), the directory holds
// the keep cap of old snapshots plus the pinned fresh one — the rotation
// already ran before the backup and its deletions stand. This is the
// deliberate overturning of the old "a failed migration never deletes a
// recovery point" judge: what survives a failure is exactly what the cap
// semantics promise (three newest + the snapshot of the attempt in flight),
// nothing more.
func TestMigrateWithBackupMigrationFailureLeavesCappedHistoryPlusPin(t *testing.T) {
	db, sqlitePath := stageDB(t, 2)
	dataDir := filepath.Dir(sqlitePath)
	backupDir := filepath.Join(dataDir, "backups", "pre-migration")
	for v := 1; v <= 6; v++ {
		mustWriteGzip(t, filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)), fmt.Appendf(nil, "snapshot v%d", v))
	}

	broken := testMigrationsUpTo(3)
	breakVersion(broken, 3)
	_, err := MigrateWithBackup(db, "sqlite", sqlitePath, broken, "sqlite", unlimitedSpaceProbe)
	if err == nil {
		t.Fatal("expected the broken migration to fail")
	}
	// The startup outage this causes is exactly when the operator needs the
	// snapshot: its location must be in the error they will see in the logs.
	if !strings.Contains(err.Error(), filepath.Join(backupDir, "sqlite_v2.db.gz")) {
		t.Fatalf("migration failure does not surface the snapshot path: %v", err)
	}
	// The pre-backup rotation trimmed v1..v6 to the newest three (v4..v6;
	// the v2 pin wasn't among the deletions), the fresh v2 snapshot then
	// replaced the stale v2 file — that file is the pinned survivor.
	for _, v := range []int{1, 3} {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); !os.IsNotExist(err) {
			t.Fatalf("expected sqlite_v%d.db.gz to be rotated away before the failed migration, stat err = %v", v, err)
		}
	}
	for _, v := range []int{2, 4, 5, 6} {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); err != nil {
			t.Fatalf("expected sqlite_v%d.db.gz to survive the failed migration: %v", v, err)
		}
	}
	if got := mustVersion(t, db); got != 2 {
		t.Fatalf("db version after failed migration = %d, want 2", got)
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
	backupPath, err := MigrateWithBackup(db, "postgres", sqlitePath, testMigrationsUpTo(1), "sqlite", unlimitedSpaceProbe)
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
