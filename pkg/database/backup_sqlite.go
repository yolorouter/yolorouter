package database

import (
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	// database.go (same package) imports github.com/glebarez/sqlite, which
	// transitively registers the "sqlite" database/sql driver via its init().
	// Blank-importing modernc.org/sqlite here as well (as the plan's
	// illustrative code does) panics with "sql: Register called twice for
	// driver sqlite" because both packages register the exact same driver
	// name — so we rely on the registration already pulled in transitively
	// instead of duplicating it (see migration_test.go for the same note).
)

// sqliteFileURIEscaper percent-encodes the three characters SQLite's file:
// URI parser (https://www.sqlite.org/uri.html) treats as structurally
// significant: an unescaped '?' would start the query-parameter section
// early (truncating the path and letting whatever follows be
// misinterpreted as parameters — including silently dropping our own
// mode=rw), '#' would start a fragment, and '%' would be misread as
// introducing a percent-encoded escape sequence. Built with NewReplacer
// (a single simultaneous left-to-right pass, not sequential replacements)
// so escaping '%' can't double-escape the '%3f'/'%23' this same call just
// produced for '?'/'#'.
var sqliteFileURIEscaper = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// sqliteFileURI builds a file: URI for path with the given query string
// (e.g. "mode=rw"). filepath.Clean collapses a leading "//" — which
// SQLite's URI parser would otherwise read as a "file://authority/..."
// form and misinterpret the first path segment as a hostname — down to a
// single "/", matching what a plain (non-URI) os.Stat/os.Open call on the
// same path would already treat as equivalent. This project targets
// macOS/Linux; Windows drive-letter/backslash paths are out of scope.
func sqliteFileURI(path, query string) string {
	return "file:" + sqliteFileURIEscaper.Replace(filepath.Clean(path)) + "?" + query
}

// BackupSQLite produces a consistent point-in-time snapshot of the SQLite
// database at sourcePath, gzip-compressed to outputPath. It uses SQLite's
// VACUUM INTO statement (3.27+) rather than copying the file directly,
// because a plain file copy can miss committed data still sitting in the
// WAL file. Any failure cleans up all temporary artifacts.
func BackupSQLite(sourcePath, outputPath string) error {
	if _, err := os.Stat(sourcePath); err != nil {
		return fmt.Errorf("source database not found: %w", err)
	}

	// Owning the output directory's creation here — rather than leaving it to
	// the caller, as an earlier version did with a different permission mode
	// than BackupPostgres used — keeps both backup paths consistent (0700,
	// since backups may contain sensitive data).
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return fmt.Errorf("create backup output directory: %w", err)
	}

	// mode=rw (via the file: URI form) tells SQLite to open the file
	// read-write but never create it — closing the TOCTOU race between the
	// os.Stat check above and this Open: sql.Open itself is lazy and
	// doesn't touch the file, but a concurrent db:reset could delete
	// sourcePath in between, and a plain (mode=rwc, the default) open would
	// then silently recreate it as an empty database on the first query
	// below, producing a "successful" backup of nothing. With mode=rw, that
	// first query instead fails with a clear "unable to open database
	// file" error.
	db, err := sql.Open("sqlite", sqliteFileURI(sourcePath, "mode=rw"))
	if err != nil {
		return fmt.Errorf("open source database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("open source database: %w", err)
	}

	snapshotPath, err := vacuumSnapshotIntoTempFile(db, filepath.Dir(outputPath), filepath.Base(outputPath)+".*.snapshot.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(snapshotPath) }()

	gzTmpFile, err := os.CreateTemp(filepath.Dir(outputPath), filepath.Base(outputPath)+".*.gz.tmp")
	if err != nil {
		return fmt.Errorf("create temp gzip file: %w", err)
	}
	gzTmpPath := gzTmpFile.Name()
	_ = gzTmpFile.Close()
	if err := gzipFile(snapshotPath, gzTmpPath); err != nil {
		_ = os.Remove(gzTmpPath)
		return fmt.Errorf("gzip snapshot: %w", err)
	}

	// Publish via a hard link rather than Rename, which would silently
	// overwrite outputPath on a name collision — Link fails instead, turning
	// a collision into a visible error rather than clobbering a previous
	// backup.
	if err := os.Link(gzTmpPath, outputPath); err != nil {
		_ = os.Remove(gzTmpPath)
		return fmt.Errorf("finalize backup output: %w", err)
	}
	_ = os.Remove(gzTmpPath)

	// Best-effort: fsync the parent directory so the new directory entry
	// itself is durable, not just the gzip file's data (already fsynced in
	// gzipFile below). Not fatal if unsupported.
	if dir, err := os.Open(filepath.Dir(outputPath)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// vacuumSnapshotIntoTempFile pre-creates a uniquely-named 0600 temp file in
// dir (matching pattern) and VACUUM INTOs db's data into it, deliberately
// without deleting the placeholder first — SQLite's VACUUM INTO accepts an
// existing *empty* file as its target and fills it in place, preserving
// whatever permissions it already had. Recreating the file fresh (as an
// earlier version did, by removing the placeholder before VACUUM INTO)
// would get SQLite's own default create mode instead (0644 & umask,
// typically world/group-readable), exposing the plaintext database
// snapshot to other local users for the window between VACUUM INTO and the
// gzip step reading it. Split out from BackupSQLite so this exact
// mechanism is directly unit-testable, not just re-implemented by hand in
// a test.
func vacuumSnapshotIntoTempFile(db *sql.DB, dir, pattern string) (string, error) {
	snapshotFile, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("create temp snapshot file: %w", err)
	}
	snapshotPath := snapshotFile.Name()
	_ = snapshotFile.Close()

	// Bind the path as a parameter rather than interpolating it into the SQL
	// text — VACUUM INTO's target accepts any expression, including a bound
	// value, so this avoids ever having to hand-quote a filename that could
	// contain a single quote.
	if _, err := db.Exec("VACUUM INTO ?", snapshotPath); err != nil {
		_ = os.Remove(snapshotPath)
		return "", fmt.Errorf("VACUUM INTO failed: %w", err)
	}
	return snapshotPath, nil
}

func gzipFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(dst)
	if _, err := io.Copy(gz, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("close gzip writer: %w", err)
	}
	// fsync before Close so the compressed backup's bytes are actually
	// durable on disk before this function reports success — without this,
	// a crash right after BackupSQLite returns (and the caller reports
	// "backup written") could lose data the kernel never flushed, even
	// though os.Link had already "published" the file.
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("sync gzip output file: %w", err)
	}
	// Explicit Close (not deferred) so a failure here — e.g. a network
	// filesystem or disk error surfacing only at close time — is not
	// silently discarded; an earlier version deferred this and ignored
	// the error entirely.
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close gzip output file: %w", err)
	}
	return nil
}
