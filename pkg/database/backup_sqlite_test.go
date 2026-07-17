package database

import (
	"compress/gzip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	// database.go (same package) imports github.com/glebarez/sqlite, which
	// transitively registers the "sqlite" database/sql driver via its init().
	// Blank-importing modernc.org/sqlite here as well (as the plan's
	// illustrative test does) panics with "sql: Register called twice for
	// driver sqlite" because both packages register the exact same driver
	// name — so we rely on the registration already pulled in transitively
	// instead of duplicating it (see migration_test.go for the same note).
)

// withUmask temporarily sets the process umask (Unix-only, matching this
// project's macOS/Linux-only scope) and returns a restore func — some
// permission regression tests would otherwise pass or fail depending on
// whatever umask happens to be set in the environment running them (e.g. a
// CI container with umask 077 would mask SQLite's own default 0644 create
// mode down to 0600 by coincidence, hiding a real regression).
//
// syscall.Umask mutates process-wide state, not anything per-goroutine or
// per-test — callers of this helper must NOT call t.Parallel(), or a
// concurrently-running test doing its own file creation could observe (or
// race to set) the wrong umask.
func withUmask(mask int) func() {
	old := syscall.Umask(mask)
	return func() { syscall.Umask(old) }
}

// decompressToSQLite gunzips gzPath to a temp file and opens it, so tests
// can actually query the backed-up data rather than just checking the gzip
// output file exists — a truncated, corrupt, or empty snapshot would still
// produce a nonzero-size .gz file, so existence alone doesn't prove the
// backup is real.
func decompressToSQLite(t *testing.T, gzPath string) *sql.DB {
	t.Helper()
	src, err := os.Open(gzPath)
	if err != nil {
		t.Fatalf("open backup gz: %v", err)
	}
	defer func() { _ = src.Close() }()
	gz, err := gzip.NewReader(src)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = gz.Close() }()

	outPath := filepath.Join(t.TempDir(), "restored.db")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create restored db: %v", err)
	}
	if _, err := io.Copy(out, gz); err != nil {
		_ = out.Close()
		t.Fatalf("decompress: %v", err)
	}
	_ = out.Close()

	db, err := sql.Open("sqlite", outPath)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	return db
}

func TestBackupSQLiteProducesReadableSnapshot(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	outputPath := filepath.Join(dir, "backup.db.gz")

	// 建一个有实际数据的源库
	db, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO t (name) VALUES ('hello')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = db.Close()

	if err := BackupSQLite(sourcePath, outputPath); err != nil {
		t.Fatalf("BackupSQLite failed: %v", err)
	}

	// 确认没有残留临时文件
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("leftover temp file found: %s", e.Name())
		}
	}

	// Decompress and actually query the backed-up data — a bare "the .gz
	// file exists" check would still pass against a truncated or empty
	// snapshot.
	restored := decompressToSQLite(t, outputPath)
	defer func() { _ = restored.Close() }()
	var name string
	if err := restored.QueryRow("SELECT name FROM t WHERE id = 1").Scan(&name); err != nil {
		t.Fatalf("query restored backup: %v", err)
	}
	if name != "hello" {
		t.Fatalf("expected restored row name %q, got %q", "hello", name)
	}
}

func TestBackupSQLiteFailsWhenSourceMissing(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "nonexistent.db")
	err := BackupSQLite(sourcePath, filepath.Join(dir, "out.db.gz"))
	if err == nil {
		t.Fatalf("expected error when source database does not exist")
	}
	if _, statErr := os.Stat(sourcePath); statErr == nil {
		t.Fatalf("BackupSQLite must not create the missing source database as a side effect")
	}
}

// TestBackupSQLiteHandlesURISpecialCharactersInPath guards sqliteFileURI: a
// source path containing '?' or '#' would, if concatenated into the file:
// URI unescaped, truncate the path at that character and either corrupt
// the "mode=rw" query parameter or make SQLite open a completely different
// (and likely nonexistent) path — either way silently backing up the wrong
// thing instead of erroring or backing up the right file.
//
// The source file is created at a plain path and then os.Rename'd to the
// special-character one — going through sql.Open directly against a path
// containing '?' doesn't work here either (the driver's own DSN parsing
// has the identical ambiguity, independent of this fix), but os.Rename is
// a plain filesystem syscall with no DSN parsing involved, so it reliably
// produces a real file at the special-character path for BackupSQLite
// (the thing actually under test) to open.
func TestBackupSQLiteHandlesURISpecialCharactersInPath(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "source.db")
	// Contains all three characters sqliteFileURIEscaper handles: '?', '#',
	// and a literal '%' immediately followed by "3f" — without the
	// "%" -> "%25" rule, that "%3f" substring looks like a valid
	// percent-encoded escape for '?', which SQLite's URI parser could
	// decode back into an actual '?' — silently producing a different path
	// than the literal filename on disk instead of erroring.
	sourcePath := filepath.Join(dir, "so?urce#name%3ftest.db")
	outputPath := filepath.Join(dir, "backup.db.gz")

	db, err := sql.Open("sqlite", plainPath)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO t (name) VALUES ('special-chars')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = db.Close()

	if err := os.Rename(plainPath, sourcePath); err != nil {
		t.Fatalf("rename to special-character path: %v", err)
	}

	if err := BackupSQLite(sourcePath, outputPath); err != nil {
		t.Fatalf("BackupSQLite failed for a path containing '?'/'#': %v", err)
	}

	restored := decompressToSQLite(t, outputPath)
	defer func() { _ = restored.Close() }()
	var name string
	if err := restored.QueryRow("SELECT name FROM t WHERE id = 1").Scan(&name); err != nil {
		t.Fatalf("query restored backup: %v", err)
	}
	if name != "special-chars" {
		t.Fatalf("expected restored row name %q, got %q — likely backed up the wrong file", "special-chars", name)
	}
}

// TestSQLiteFileURICollapsesLeadingDoubleSlash guards filepath.Clean's role
// in sqliteFileURI: a path starting with "//" would otherwise produce
// "file://host/..." — SQLite's URI parser reads the segment between the
// second and third slash as an authority/hostname, not part of the path —
// silently opening (or, worse, creating) a completely different location
// than os.Stat/a plain path open would resolve to for the same string.
func TestSQLiteFileURICollapsesLeadingDoubleSlash(t *testing.T) {
	got := sqliteFileURI("//tmp/double-slash.db", "mode=rw")
	want := "file:/tmp/double-slash.db?mode=rw"
	if got != want {
		t.Fatalf("expected leading '//' to collapse to a single '/', got %q, want %q", got, want)
	}
}

// TestSQLiteModeRWDoesNotCreateMissingFile locks in the mechanism
// BackupSQLite relies on to close a TOCTOU race: the os.Stat existence
// check up front can't protect against a concurrent db:reset deleting the
// file between that check and the actual connection open (sql.Open is
// lazy). Opening via the file:...?mode=rw URI form, rather than a plain
// path (which defaults to mode=rwc — read/write/create), makes the open
// itself refuse to fabricate a missing database instead of silently
// succeeding against an empty one. This calls sqliteFileURI directly (the
// same production helper BackupSQLite uses), not a hand-rolled DSN string,
// so a regression in the helper itself would fail this test too.
func TestSQLiteModeRWDoesNotCreateMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.db")

	db, err := sql.Open("sqlite", sqliteFileURI(path, "mode=rw"))
	if err != nil {
		t.Fatalf("sql.Open should not fail eagerly (connections are lazy): %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err == nil {
		t.Fatalf("expected Ping to fail for a nonexistent file opened with mode=rw")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatalf("mode=rw must not have created the file")
	}
}

// TestVacuumSnapshotIntoTempFilePreservesPermissions locks in the fix for
// the snapshot-file permission leak by calling the actual production
// helper BackupSQLite uses (vacuumSnapshotIntoTempFile) rather than
// hand-rolling an equivalent VACUUM INTO call — a hand-rolled copy would
// keep passing even if BackupSQLite itself regressed back to deleting the
// placeholder before VACUUM INTO, since it wouldn't be exercising the real
// code path at all. os.CreateTemp creates files at mode 0600, and this
// asserts that mode survives VACUUM INTO filling the file in place
// (SQLite accepts an existing *empty* file as VACUUM INTO's target rather
// than requiring it be absent) — recreating the file fresh instead would
// get SQLite's own default create mode (0644 & umask), typically
// world/group-readable, exposing the plaintext database snapshot to other
// local users for the window between VACUUM INTO and the gzip step
// reading it.
func TestVacuumSnapshotIntoTempFilePreservesPermissions(t *testing.T) {
	// Pinned so this test's pass/fail doesn't depend on whatever umask the
	// environment running it happens to have — without this, a CI
	// container with umask 077 would mask SQLite's own default 0644 create
	// mode down to 0600 by coincidence, hiding a real regression (deleting
	// the placeholder before VACUUM INTO) that a more typical umask 022
	// would have caught.
	defer withUmask(0o022)()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	if _, err := src.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	_ = src.Close()

	db, err := sql.Open("sqlite", sqliteFileURI(srcPath, "mode=rw"))
	if err != nil {
		t.Fatalf("open source via mode=rw: %v", err)
	}
	defer func() { _ = db.Close() }()

	snapshotPath, err := vacuumSnapshotIntoTempFile(db, dir, "snapshot.*.tmp")
	if err != nil {
		t.Fatalf("vacuumSnapshotIntoTempFile: %v", err)
	}
	defer func() { _ = os.Remove(snapshotPath) }()

	info, err := os.Stat(snapshotPath)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected the snapshot file to keep its pre-created 0600 permissions, got %04o", info.Mode().Perm())
	}
}
