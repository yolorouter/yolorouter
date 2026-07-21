package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootstrapCommandRejectsExtraArgsBeforeInit guards the ordering fix:
// unexpected positional args (e.g. a misplaced --config caught as a
// positional arg by Go's flag package, which stops parsing flags at the
// first non-flag argument) must be rejected before bootstrap.Init ever
// runs — otherwise the command would already have connected to (and for
// SQLite, auto-created) the wrong default database by the time the error
// surfaces. We assert this by checking no config.yaml was generated as a
// side effect of the rejected call.
func TestBootstrapCommandRejectsExtraArgsBeforeInit(t *testing.T) {
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	}()

	err = runDBMigrate(context.Background(), []string{"unexpected-extra-arg"})
	if err == nil {
		t.Fatalf("expected error for unexpected positional argument")
	}
	if !strings.Contains(err.Error(), "unexpected extra arguments") {
		t.Fatalf("expected 'unexpected extra arguments' in error, got: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "configs", "config.yaml")); statErr == nil {
		t.Fatalf("bootstrap.Init must not have run — configs/config.yaml should not have been generated")
	}
}

// TestRunDBRollbackRejectsFlagAfterPositionalVersion is the exact scenario
// from a real invocation: "db:rollback 1 --config prod.yaml" — Go's flag
// package stops parsing at "1" and leaves "--config"/"prod.yaml" as
// unconsumed extra args. Silently ignoring them would roll back the
// default database instead of prod.yaml; this must error instead.
func TestRunDBRollbackRejectsFlagAfterPositionalVersion(t *testing.T) {
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	}()

	err = runDBRollback(context.Background(), []string{"1", "--config", "some-other-config.yaml"})
	if err == nil {
		t.Fatalf("expected error for flag appearing after the positional version argument")
	}
	if !strings.Contains(err.Error(), "unexpected extra arguments") {
		t.Fatalf("expected 'unexpected extra arguments' in error, got: %v", err)
	}
}

// TestRunDBRollbackRejectsInvalidVersionBeforeInit guards the ordering fix
// for version-format validation: "db:rollback abc" must fail immediately
// on the malformed version string, before bootstrap.Init ever runs — not
// after already generating a default config and connecting to (and for
// SQLite, creating) a database file. We assert this the same way as
// TestBootstrapCommandRejectsExtraArgsBeforeInit: no configs/config.yaml
// should exist afterward.
func TestRunDBRollbackRejectsInvalidVersionBeforeInit(t *testing.T) {
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	}()

	err = runDBRollback(context.Background(), []string{"abc"})
	if err == nil {
		t.Fatalf("expected error for a non-numeric version argument")
	}
	if !strings.Contains(err.Error(), "invalid version argument") {
		t.Fatalf("expected 'invalid version argument' in error, got: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "configs", "config.yaml")); statErr == nil {
		t.Fatalf("bootstrap.Init must not have run — configs/config.yaml should not have been generated")
	}
}

// TestRunDBRollbackRejectsNegativeVersionBeforeInit guards against a
// negative version reaching bootstrap.Init: strconv.ParseInt happily
// parses "-1" as a valid int64, so without an explicit >= 0 check, a
// negative version would sail past version-format validation and connect
// to (and for SQLite, create) a database before database.RollbackTo ever
// gets a chance to reject a version number that can't mean anything.
//
// The args use Go flag's "--" terminator ("end of flags") to get "-1"
// through as a positional argument at all — flag.FlagSet.Parse treats a
// bare "-1" as an attempt to use an unrecognized flag named "1", not as a
// positional value, so plain []string{"-1"} would be rejected by Parse
// itself before ever reaching this check (also a safe outcome, just via a
// different error message than the one this test is targeting).
func TestRunDBRollbackRejectsNegativeVersionBeforeInit(t *testing.T) {
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	}()

	err = runDBRollback(context.Background(), []string{"--", "-1"})
	if err == nil {
		t.Fatalf("expected error for a negative version argument")
	}
	if !strings.Contains(err.Error(), "cannot be negative") {
		t.Fatalf("expected 'cannot be negative' in error, got: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "configs", "config.yaml")); statErr == nil {
		t.Fatalf("bootstrap.Init must not have run — configs/config.yaml should not have been generated")
	}
}

// TestRunDBBackupFailsForMissingSQLiteSourceWithoutCreatingIt guards against
// db:backup silently fabricating an empty database: an earlier version
// routed through bootstrap.Init/database.Init first, which opens (and for
// SQLite, creates) the database file as a side effect — so backing up a
// database that doesn't exist yet would "succeed" with an empty backup
// instead of erroring. db:backup must load config only, check the source
// file, and fail without ever creating it.
func TestRunDBBackupFailsForMissingSQLiteSourceWithoutCreatingIt(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "does-not-exist.db")
	outDir := filepath.Join(dir, "backups")

	configYAML := "database:\n" +
		"  driver: sqlite\n" +
		"  sqlite_path: " + dbPath + "\n" +
		"security:\n" +
		"  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	err := runDBBackup(context.Background(), []string{"--config", configPath, "--output-dir", outDir})
	if err == nil {
		t.Fatalf("expected error for missing source database")
	}
	if !strings.Contains(err.Error(), "source database not found") {
		t.Fatalf("expected 'source database not found' in error, got: %v", err)
	}

	if _, statErr := os.Stat(dbPath); statErr == nil {
		t.Fatalf("db:backup must not have created the missing source database file")
	}
}
