package database

import (
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"testing"

	_ "github.com/lib/pq"
)

// parseTestPostgresDSN parses a postgres://user:pass@host:port/dbname URL
// into PostgresDSN, so the integration test actually exercises the
// connection TEST_POSTGRES_DSN points at — an earlier version checked this
// env var only to decide whether to skip, then called BackupPostgres with
// unrelated hardcoded parameters regardless of what it said.
func parseTestPostgresDSN(t *testing.T, dsn string) PostgresDSN {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("TEST_POSTGRES_DSN is not a valid URL: %v", err)
	}
	port := 5432
	if p := u.Port(); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	password, _ := u.User.Password()
	dbName := u.Path
	if len(dbName) > 0 && dbName[0] == '/' {
		dbName = dbName[1:]
	}
	return PostgresDSN{
		Host:     u.Hostname(),
		Port:     port,
		User:     u.User.Username(),
		Password: password,
		DBName:   dbName,
	}
}

func TestBackupPostgresRequiresTestDSN(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set, skipping Postgres integration test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open test postgres: %v", err)
	}
	defer func() { _ = db.Close() }()

	outputDir := t.TempDir()
	path, err := BackupPostgres(parseTestPostgresDSN(t, dsn), outputDir)
	if err != nil {
		t.Fatalf("BackupPostgres failed (requires pg_dump installed): %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("backup output file missing: %v", err)
	}
}

func TestPgpassEscapeHandlesColonsAndBackslashes(t *testing.T) {
	cases := map[string]string{
		"plain":       "plain",
		"has:colon":   `has\:colon`,
		`has\slash`:   `has\\slash`,
		`both:\chars`: `both\:\\chars`,
	}
	for in, want := range cases {
		if got := pgpassEscape(in); got != want {
			t.Fatalf("pgpassEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
