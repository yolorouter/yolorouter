package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadGeneratesDefaultConfigWhenMissing(t *testing.T) {
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

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with missing default config should succeed: %v", err)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Fatalf("expected default driver sqlite, got %s", cfg.Database.Driver)
	}
	if cfg.Security.ProviderMasterKey == "" {
		t.Fatalf("expected generated provider_master_key, got empty string")
	}

	generatedPath := filepath.Join(dir, "configs", "config.yaml")
	if _, err := os.Stat(generatedPath); err != nil {
		t.Fatalf("expected configs/config.yaml to be written: %v", err)
	}

	// The second load must reuse the same key
	cfg2, err := Load("")
	if err != nil {
		t.Fatalf("second Load failed: %v", err)
	}
	if cfg2.Security.ProviderMasterKey != cfg.Security.ProviderMasterKey {
		t.Fatalf("provider_master_key changed between loads: %q vs %q", cfg.Security.ProviderMasterKey, cfg2.Security.ProviderMasterKey)
	}
}

// TestLoadRejectsMultiDocumentYAML guards against yaml.Decoder.Decode's
// single-call behavior: it only consumes the first "---"-delimited
// document in a stream, so a config.yaml with two documents would have its
// second document silently ignored — potentially hiding a value the
// file's author expected to take effect — unless loadStrict explicitly
// decodes again and requires io.EOF.
func TestLoadRejectsMultiDocumentYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "server:\n  port: 8080\n" +
		"database:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n" +
		"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n" +
		"---\n" +
		"server:\n  port: 9090\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error for a config file containing more than one YAML document")
	}
}

func TestLoadFailsForExplicitMissingPath(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "nonexistent.yaml"))
	if err == nil {
		t.Fatalf("expected error when explicit --config path does not exist")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 8080\nnot_a_real_field: true\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected strict decoding to reject unknown field")
	}
}

func TestLoadRejectsEmptyRequiredFieldInExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// provider_master_key is empty in the explicitly provided config file — must error out, not silently fill it in
	if err := os.WriteFile(path, []byte("server:\n  port: 8080\ndatabase:\n  driver: sqlite\n  sqlite_path: ./data/x.db\nsecurity:\n  provider_master_key: \"\"\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error for empty provider_master_key in an explicitly provided config file")
	}
}

func TestLoadRejectsInvalidDriver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("database:\n  driver: mysql\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error for unsupported driver value")
	}
}

// TestLoadRejectsInvalidLogLevel guards the log.level whitelist: pkg/logger
// (copied verbatim from the reference project) silently falls back to info
// on an unparseable level string instead of erroring, so config validation
// is the only place a typo like "debu" gets caught.
func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"log:\n  level: debu\n"+
			"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error for unrecognized log.level value")
	}
}

// TestLoadAcceptsEveryKnownLogLevel drives all four recognized log.level
// values through validate() individually, the same way
// TestLoadAcceptsEveryKnownSSLMode does for sslmode — a single
// "one bad value is rejected" test wouldn't catch a typo in validLogLevels
// that silently rejects one of the legitimate values too.
func TestLoadAcceptsEveryKnownLogLevel(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(
				"log:\n  level: "+level+"\n"+
					"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"), 0o600); err != nil {
				t.Fatalf("write test config: %v", err)
			}

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("expected log.level %q to be accepted, got error: %v", level, err)
			}
			if cfg.Log.Level != level {
				t.Fatalf("expected log.level %q to round-trip, got %q", level, cfg.Log.Level)
			}
		})
	}
}

func TestLoadRejectsInvalidSSLModeForPostgres(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"database:\n  driver: postgres\n  host: localhost\n  port: 5432\n  user: u\n  dbname: d\n  sslmode: not-a-real-mode\n"+
			"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error for unrecognized database.sslmode value")
	}
}

// TestLoadAcceptsEveryKnownSSLMode drives all six libpq sslmode values
// through validate() individually — a single "one bad value is rejected"
// test wouldn't catch e.g. an off-by-one typo in validSSLModes that
// silently rejects one of the legitimate values too.
func TestLoadAcceptsEveryKnownSSLMode(t *testing.T) {
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(
				"database:\n  driver: postgres\n  host: localhost\n  port: 5432\n  user: u\n  dbname: d\n  sslmode: "+mode+"\n"+
					"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"), 0o600); err != nil {
				t.Fatalf("write test config: %v", err)
			}

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("expected sslmode %q to be accepted, got error: %v", mode, err)
			}
			if cfg.Database.SSLMode != mode {
				t.Fatalf("expected sslmode %q to round-trip, got %q", mode, cfg.Database.SSLMode)
			}
		})
	}
}

// TestAtomicWriteConfigConcurrentRaceHasExactlyOneWinner drives many
// goroutines racing to publish distinct configs to the same path at once —
// this is the scenario a Stat-then-Rename implementation gets wrong (two
// goroutines can both observe "doesn't exist" and both proceed, with the
// last Rename silently overwriting an earlier winner's file, including its
// already-generated master key). With os.Link-based publishing, every
// non-winner must observe an "already exists" condition and defer to
// whichever goroutine's content actually landed on disk.
func TestAtomicWriteConfigConcurrentRaceHasExactlyOneWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const n = 20
	keys := make([]string, n)
	errs := make([]error, n)
	done := make(chan int, n)

	for i := range n {
		key, err := randomMasterKey()
		if err != nil {
			t.Fatalf("generate test key %d: %v", i, err)
		}
		keys[i] = key
		go func() {
			cfg := defaults()
			cfg.Security.ProviderMasterKey = keys[i]
			errs[i] = atomicWriteConfig(path, cfg)
			done <- i
		}()
	}
	for range n {
		<-done
	}

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: atomicWriteConfig returned a real error (should only ever succeed or silently lose the race): %v", i, err)
		}
	}

	final, err := loadStrict(path)
	if err != nil {
		t.Fatalf("loadStrict after race: %v", err)
	}
	if !slices.Contains(keys, final.Security.ProviderMasterKey) {
		t.Fatalf("final config's key %q does not match any of the %d racing goroutines' keys — file was corrupted, not just raced", final.Security.ProviderMasterKey, n)
	}

	leftover, globErr := filepath.Glob(filepath.Join(dir, "config.yaml.*.tmp"))
	if globErr != nil {
		t.Fatalf("glob for leftover temp files: %v", globErr)
	}
	if len(leftover) != 0 {
		t.Fatalf("expected no leftover temp files, found: %v", leftover)
	}
}

// TestDefaultsSetsUpdateEnabled guards the update-feature default: defaults()
// must set Enabled=true so an auto-generated or legacy config that omits the
// whole `update` section keeps updates ON. A zero-value UpdateConfig (Enabled
// false) would silently disable the feature — exactly the regression this
// test guards against.
func TestDefaultsSetsUpdateEnabled(t *testing.T) {
	cfg := defaults()
	if !cfg.Update.Enabled {
		t.Fatalf("defaults().Update.Enabled = false, want true (omitted update section must not disable updates)")
	}
}

// TestLoadOmittedUpdateSectionDefaultsEnabled drives a config with NO
// `update:` section through Load: the strict decoder starts from defaults()
// (Enabled=true) and an absent section leaves it untouched. Without this, a
// typo in defaults() that drops the Enabled field would silently flip every
// legacy config to updates-disabled.
func TestLoadOmittedUpdateSectionDefaultsEnabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"server:\n  port: 8080\ndatabase:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n"+
			"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}
	if !cfg.Update.Enabled {
		t.Fatalf("omitted update section must default Enabled=true, got false")
	}
}

func TestLoadAcceptsUpdateSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"server:\n  port: 8080\ndatabase:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n"+
			"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"+
			"update:\n  enabled: false\n  github_repo: \"fork/ce\"\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}
	if cfg.Update.Enabled {
		t.Fatalf("expected Enabled=false, got true")
	}
	if cfg.Update.GitHubRepo != "fork/ce" {
		t.Fatalf("expected GitHubRepo fork/ce, got %q", cfg.Update.GitHubRepo)
	}
}

// TestLoadRejectsInvalidGitHubRepo drives every malformed shape through
// validate() so a typo'd owner/repo fails at config load, not as a mysterious
// GitHub 404 at runtime.
func TestLoadRejectsInvalidGitHubRepo(t *testing.T) {
	for _, repo := range []string{
		"ownerrepo",        // missing slash
		"owner/repo/extra", // too many segments
		"/repo",            // empty owner
		"owner/",           // empty repo
		"own er/repo",      // whitespace
	} {
		t.Run(repo, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(
				"server:\n  port: 8080\ndatabase:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n"+
					"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"+
					"update:\n  github_repo: \""+repo+"\"\n"), 0o600); err != nil {
				t.Fatalf("write test config: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("expected error for malformed github_repo %q", repo)
			}
		})
	}
}

// TestLoadAcceptsEmptyGitHubRepo: an empty repo is valid (it falls back to
// the compiled-in default, or disables updates if that is also empty) — only
// a non-empty malformed value is rejected.
func TestLoadAcceptsEmptyGitHubRepo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"server:\n  port: 8080\ndatabase:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n"+
			"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"+
			"update:\n  github_repo: \"\"\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("expected empty github_repo to be accepted, got error: %v", err)
	}
}
