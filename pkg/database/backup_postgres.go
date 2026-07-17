package database

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PostgresDSN carries the connection parameters BackupPostgres needs to
// invoke pg_dump. This mirrors the commercial yolorouter-server's
// internal/cli/db_backup.go implementation, adapted to take explicit
// parameters instead of reading a package-level config singleton.
type PostgresDSN struct {
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	// SSLMode is passed to pg_dump via PGSSLMODE. Must match the app's own
	// connection policy (internal/config's database.sslmode) — otherwise a
	// deployment configured for "require"/"verify-full" would have its
	// backups silently fall back to libpq's default negotiation instead of
	// enforcing the same TLS policy the app itself uses.
	SSLMode string
}

// BackupPostgres shells out to pg_dump + gzip, matching the commercial
// implementation's pgpass-file approach (avoids leaking the password via
// /proc/PID/cmdline or the process environment shown in `ps`).
func BackupPostgres(dsn PostgresDSN, outputDir string) (string, error) {
	if dsn.Host == "" || dsn.User == "" || dsn.DBName == "" {
		return "", fmt.Errorf("incomplete postgres connection parameters")
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}
	sslMode := dsn.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}

	// Nanosecond-precision timestamp (not just second-precision) so two
	// backups started within the same second still get distinct filenames;
	// the final publish step below also refuses to silently overwrite an
	// existing file if a collision somehow still occurs.
	timestamp := time.Now().UTC().Format("20060102_150405.000000000")
	filename := fmt.Sprintf("pg_%s.sql.gz", timestamp)
	finalPath := filepath.Join(outputDir, filename)

	tmpFile, err := os.CreateTemp(outputDir, filename+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp output file: %w", err)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close() // reopened below via os.OpenFile; this call only reserves a unique name
	// Deferred immediately (rather than only on the failure paths below, as
	// an earlier version did) so a failure between here and the first
	// explicit cleanup call — e.g. pgpass file creation/chmod, or os.Pipe —
	// can't leak tmpPath. A no-op after the successful Link path at the
	// bottom: Link creates a second name for the same inode, so removing
	// this one still leaves finalPath intact.
	defer func() { _ = os.Remove(tmpPath) }()

	// .pgpass fields are colon-separated; both ':' and '\' must be escaped
	// in each field or a host/user/password containing either character
	// would corrupt the line and shift subsequent fields.
	pgpassLine := fmt.Sprintf("%s:%d:*:%s:%s\n",
		pgpassEscape(dsn.Host), dsn.Port, pgpassEscape(dsn.User), pgpassEscape(dsn.Password))
	pgpassFile, err := os.CreateTemp("", "pgpass-*.conf")
	if err != nil {
		return "", fmt.Errorf("create temp pgpass file: %w", err)
	}
	pgpassPath := pgpassFile.Name()
	defer func() { _ = os.Remove(pgpassPath) }()
	if _, err := pgpassFile.WriteString(pgpassLine); err != nil {
		_ = pgpassFile.Close()
		return "", fmt.Errorf("write pgpass file: %w", err)
	}
	if err := pgpassFile.Close(); err != nil {
		return "", fmt.Errorf("close pgpass file: %w", err)
	}
	if err := os.Chmod(pgpassPath, 0o600); err != nil {
		return "", fmt.Errorf("chmod pgpass file: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pgDump := exec.CommandContext(ctx, "pg_dump",
		"-h", dsn.Host, "-p", fmt.Sprintf("%d", dsn.Port),
		"-U", dsn.User, "-d", dsn.DBName, "--no-password")
	pgDump.Env = append(os.Environ(), "PGPASSFILE="+pgpassPath, "PGSSLMODE="+sslMode)
	gzipCmd := exec.CommandContext(ctx, "gzip", "--stdout")

	pr, pw, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("create pipe: %w", err)
	}
	pgDump.Stdout = pw
	gzipCmd.Stdin = pr

	outFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return "", fmt.Errorf("create temp output file: %w", err)
	}
	gzipCmd.Stdout = outFile

	if err := pgDump.Start(); err != nil {
		_ = outFile.Close()
		_ = pr.Close()
		_ = pw.Close()
		return "", fmt.Errorf("start pg_dump (is it installed?): %w", err)
	}
	_ = pw.Close()

	if err := gzipCmd.Start(); err != nil {
		_ = outFile.Close()
		_ = pr.Close()
		_ = pgDump.Process.Kill()
		_ = pgDump.Wait()
		return "", fmt.Errorf("start gzip: %w", err)
	}
	_ = pr.Close()

	// Wait on both processes concurrently. Waiting on pg_dump first (as an
	// earlier version did) risks a hang: if gzip exits early (e.g. it
	// isn't installed, or fails fast), nothing is left reading the pipe,
	// and pg_dump blocks the moment the OS pipe buffer fills — invisible
	// until the 30-minute context timeout finally kills it. Waiting
	// concurrently surfaces either failure immediately.
	var dumpErr, gzipErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		dumpErr = pgDump.Wait()
	}()
	go func() {
		defer wg.Done()
		gzipErr = gzipCmd.Wait()
	}()
	wg.Wait()

	var syncErr error
	if dumpErr == nil && gzipErr == nil {
		// fsync before Close so the dump's bytes are actually durable on
		// disk before this function reports success back to the caller —
		// otherwise a crash right after returning could lose data the
		// kernel never flushed, even though os.Link had already
		// "published" the file below.
		syncErr = outFile.Sync()
	}
	closeErr := outFile.Close()

	if dumpErr != nil {
		return "", fmt.Errorf("pg_dump failed: %w", dumpErr)
	}
	if gzipErr != nil {
		return "", fmt.Errorf("gzip failed: %w", gzipErr)
	}
	if syncErr != nil {
		return "", fmt.Errorf("sync backup output file: %w", syncErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close backup output file: %w", closeErr)
	}

	// Publish via a hard link rather than Rename, which would silently
	// overwrite finalPath if the nanosecond timestamp above ever collided —
	// Link fails instead, so a collision surfaces as an error rather than
	// clobbering a previous backup.
	if err := os.Link(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("finalize backup file: %w", err)
	}

	// Best-effort: fsync the parent directory so the new directory entry
	// itself is durable too. Not fatal if unsupported.
	if dir, err := os.Open(filepath.Dir(finalPath)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return finalPath, nil
}

// pgpassEscape escapes ':' and '\' in a single .pgpass field, per the
// format documented for libpq password files.
func pgpassEscape(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `:`, `\:`)
	return v
}
