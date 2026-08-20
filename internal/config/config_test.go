package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

// TestLoadSeedsGitHubProxyFromEnv covers a mirror install: install.sh exports
// YOLO_UPDATE_GITHUB_PROXY, and the first generated config must record it under
// update.github_proxy so self-update uses the mirror without any manual edit.
// The strict re-parse of the written file also proves github_proxy is a known
// field, so a documented manual edit does not trip KnownFields(true).
func TestLoadSeedsGitHubProxyFromEnv(t *testing.T) {
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
	t.Setenv("YOLO_UPDATE_GITHUB_PROXY", "https://gh.example.com/")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load should generate config: %v", err)
	}
	if cfg.Update.GitHubProxy != "https://gh.example.com/" {
		t.Fatalf("github_proxy = %q, want it seeded from the env var", cfg.Update.GitHubProxy)
	}

	// A strict re-parse of the just-written file must accept github_proxy.
	cfg2, err := Load("")
	if err != nil {
		t.Fatalf("strict reload of generated config failed: %v", err)
	}
	if cfg2.Update.GitHubProxy != "https://gh.example.com/" {
		t.Fatalf("github_proxy not persisted across reload: %q", cfg2.Update.GitHubProxy)
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
// silently falls back to info on an unparseable level string instead of
// erroring, so config validation is the only place a typo like "debu" gets
// caught.
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

// TestDefaultsPriceCatalogLiveEndpoint verifies the default endpoint points at
// the live distribution Worker, so every instance refreshes daily with zero
// config. An operator opts OUT by setting endpoint to "".
func TestDefaultsPriceCatalogLiveEndpoint(t *testing.T) {
	cfg := defaults()
	const want = "https://prices.yolorouter.com/catalog.json"
	if cfg.PriceCatalog.Endpoint != want {
		t.Fatalf("defaults().PriceCatalog.Endpoint = %q, want %q (live distribution Worker)", cfg.PriceCatalog.Endpoint, want)
	}
	if cfg.PriceCatalog.RefreshInterval != 24*time.Hour {
		t.Fatalf("defaults().PriceCatalog.RefreshInterval = %v, want 24h", cfg.PriceCatalog.RefreshInterval)
	}
}

// TestLoadOmittedPriceCatalogSectionKeepsDefaults drives a config with NO
// price_catalog section through Load and asserts the default live endpoint +
// 24h interval survive. Without this, a typo in defaults() that drops the
// PriceCatalog field would silently disable refresh for every legacy config.
func TestLoadOmittedPriceCatalogSectionKeepsDefaults(t *testing.T) {
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
	if cfg.PriceCatalog.Endpoint != "https://prices.yolorouter.com/catalog.json" {
		t.Fatalf("omitted price_catalog section must default to the live endpoint, got %q", cfg.PriceCatalog.Endpoint)
	}
	if cfg.PriceCatalog.RefreshInterval != 24*time.Hour {
		t.Fatalf("omitted price_catalog section must default RefreshInterval 24h, got %v", cfg.PriceCatalog.RefreshInterval)
	}
}

// TestLoadAcceptsPriceCatalogSection asserts an explicit endpoint + interval
// round-trip through the strict decoder (KnownFields(true)).
func TestLoadAcceptsPriceCatalogSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"server:\n  port: 8080\ndatabase:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n"+
			"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"+
			"price_catalog:\n  endpoint: \"https://prices.example.test/catalog.json\"\n  refresh_interval: 6h\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}
	if cfg.PriceCatalog.Endpoint != "https://prices.example.test/catalog.json" {
		t.Fatalf("expected endpoint, got %q", cfg.PriceCatalog.Endpoint)
	}
	if cfg.PriceCatalog.RefreshInterval != 6*time.Hour {
		t.Fatalf("expected 6h, got %v", cfg.PriceCatalog.RefreshInterval)
	}
}

// TestLoadPriceCatalogEmptyEndpointOverridesDefault pins the documented disable
// contract: the default endpoint is the live Worker, but an operator who writes
// `price_catalog: { endpoint: "" }` opts out, and the empty string must reach
// serve/StartRefresh so no refresh goroutine spawns. The strict decoder overwrites
// the seeded default with the explicit empty value — this test catches a regression
// where that override silently keeps the default (and the instance refreshes
// against the operator's explicit wish to stay on the embedded seed).
func TestLoadPriceCatalogEmptyEndpointOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"server:\n  port: 8080\ndatabase:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n"+
			"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"+
			"price_catalog:\n  endpoint: \"\"\n"), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}
	if cfg.PriceCatalog.Endpoint != "" {
		t.Fatalf("explicit empty endpoint must override the live default to disable refresh, got %q", cfg.PriceCatalog.Endpoint)
	}
}

// TestValidatePriceCatalogRejectsZeroIntervalWithEndpoint is the guard against
// the one genuinely broken combination: an endpoint set (operator wants live
// refresh) with a non-positive interval (ticker fires constantly or never).
// An empty endpoint with any interval stays valid — it's the documented no-op.
func TestValidatePriceCatalogRejectsZeroIntervalWithEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		interval time.Duration
		wantErr  bool
	}{
		{"endpoint set, zero interval", "https://x.test/c.json", 0, true},
		{"endpoint set, negative interval", "https://x.test/c.json", -time.Hour, true},
		{"endpoint empty, zero interval (no-op)", "", 0, false},
		{"endpoint empty, positive interval (no-op)", "", 24 * time.Hour, false},
		{"endpoint set, positive interval (active)", "https://x.test/c.json", 12 * time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePriceCatalog(&PriceCatalogConfig{Endpoint: tc.endpoint, RefreshInterval: tc.interval})
			if tc.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

// TestLoadFillsGitHubProxyFromEnvOnExistingConfig covers a mirror installer
// upgrading a prior direct install: config.yaml already exists (so it is never
// regenerated), yet the proxy env the installer injects into the service unit
// must still take effect — but only when the file leaves github_proxy empty.
func TestLoadFillsGitHubProxyFromEnvOnExistingConfig(t *testing.T) {
	base := "server:\n  port: 8080\ndatabase:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n" +
		"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"

	t.Run("empty in file is filled from env", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(base+"update:\n  enabled: true\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		t.Setenv("YOLO_UPDATE_GITHUB_PROXY", "https://gh.example.com/")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.Update.GitHubProxy != "https://gh.example.com/" {
			t.Fatalf("github_proxy = %q, want filled from env", cfg.Update.GitHubProxy)
		}
	})

	t.Run("explicit value in file wins over env", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(base+"update:\n  enabled: true\n  github_proxy: \"https://in-file.example/\"\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		t.Setenv("YOLO_UPDATE_GITHUB_PROXY", "https://gh.example.com/")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.Update.GitHubProxy != "https://in-file.example/" {
			t.Fatalf("github_proxy = %q, want the explicit config value to win", cfg.Update.GitHubProxy)
		}
	})
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

// TestGatewayTimeoutsDefaults drives a config with NO `gateway:` block through
// Load: the strict decoder leaves every gateway field at its zero value, and
// applyGatewayDefaults must then fill the idle-keepalive defaults so an upgrade
// without config changes picks up the new timeout model automatically. Reuses
// the chdir-into-empty-tmpdir pattern from TestLoadGeneratesDefaultConfigWhenMissing.
func TestGatewayTimeoutsDefaults(t *testing.T) {
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
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gateway.ConnectTimeout != 5*time.Second {
		t.Errorf("ConnectTimeout default = %v, want 5s", cfg.Gateway.ConnectTimeout)
	}
	if cfg.Gateway.HeaderTimeout != 600*time.Second {
		t.Errorf("HeaderTimeout default = %v, want 600s", cfg.Gateway.HeaderTimeout)
	}
	if cfg.Gateway.FirstByteTimeout != 600*time.Second {
		t.Errorf("FirstByteTimeout default = %v, want 600s", cfg.Gateway.FirstByteTimeout)
	}
	if cfg.Gateway.BodyIdleTimeout != 60*time.Second {
		t.Errorf("BodyIdleTimeout default = %v, want 60s", cfg.Gateway.BodyIdleTimeout)
	}
	if cfg.Gateway.AttemptTimeout != 20*time.Minute {
		t.Errorf("AttemptTimeout default = %v, want 20m", cfg.Gateway.AttemptTimeout)
	}
	if cfg.Gateway.RequestTimeout != 30*time.Minute {
		t.Errorf("RequestTimeout default = %v, want 30m", cfg.Gateway.RequestTimeout)
	}
	if cfg.Gateway.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout default = %v, want 10s", cfg.Gateway.TLSHandshakeTimeout)
	}
	if cfg.Gateway.KeyRateLimitCooldown != 60*time.Second {
		t.Errorf("KeyRateLimitCooldown default = %v, want 60s", cfg.Gateway.KeyRateLimitCooldown)
	}
}

// TestGenerateDefaultConfigWritesRealGatewayTimeouts pins the requirement that
// generateDefaultConfig must write the real idle-keepalive gateway defaults
// (5s/600s/60s/20m/30m + 10s TLS) to disk, not five 0s. Previously defaults()
// omitted the Gateway block entirely, so the first-run file landed on disk
// with zero values that diverged from both the actual runtime behaviour and
// configs/config.example.yaml.
func TestGenerateDefaultConfigWritesRealGatewayTimeouts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Drive the exact write path: defaults() → marshal → atomicWriteConfig.
	// generateDefaultConfig also creates directories and re-reads, which is
	// covered by TestLoadGeneratesDefaultConfigWhenMissing; here the focus is
	// the ON-DISK content.
	cfg := defaults()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal defaults: %v", err)
	}

	var roundTrip Config
	if err := yaml.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("unmarshal marshalled defaults: %v", err)
	}

	want := DefaultGatewayConfig()
	if roundTrip.Gateway != want {
		t.Errorf("generated gateway block = %+v, want %+v (real defaults, not 0s)", roundTrip.Gateway, want)
	}

	// Also assert the literal values so a future change to DefaultGatewayConfig
	// that accidentally zeroes a field is caught here too.
	if roundTrip.Gateway.ConnectTimeout != 5*time.Second {
		t.Errorf("ConnectTimeout in generated file = %v, want 5s", roundTrip.Gateway.ConnectTimeout)
	}
	if roundTrip.Gateway.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout in generated file = %v, want 10s", roundTrip.Gateway.TLSHandshakeTimeout)
	}

	// Belt-and-suspenders: write the file and re-load it through the real
	// Load path, confirming the gateway values survive the full round trip.
	// Fill a valid provider_master_key so validate() accepts the file.
	cfg.Security.ProviderMasterKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	data, err = yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("re-marshal defaults with key: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load generated file: %v", err)
	}
	if loaded.Gateway != want {
		t.Errorf("loaded gateway = %+v, want %+v", loaded.Gateway, want)
	}
}

// TestGatewayTimeoutsValidation drives every layering-invariant violation
// through validateGatewayTimeouts individually. Every field must be strictly
// positive (a zero/negative would make a timer fire immediately or disable a
// dial timeout). The only ordering constraints enforced are the ones that
// reflect a real same-attempt nesting relationship: header_timeout <=
// attempt_timeout, first_byte_timeout <= attempt_timeout, and attempt_timeout
// < request_timeout. connect_timeout and body_idle_timeout bound independent
// phases (dial, and inter-chunk body gaps) and are deliberately NOT ordered
// against each other or against header_timeout — a connect_timeout larger
// than body_idle_timeout (e.g. a slow network with a tight steady-state gap)
// is a valid deployment choice, not a misconfiguration. The "header ==
// attempt (equal allowed)" case pins that the `<=` rule accepts equality — a
// future refactor flipping it to strict `<` would flip that case to wantErr.
func TestGatewayTimeoutsValidation(t *testing.T) {
	valid := GatewayConfig{
		ConnectTimeout:          5 * time.Second,
		HeaderTimeout:           600 * time.Second,
		FirstByteTimeout:        600 * time.Second,
		BodyIdleTimeout:         60 * time.Second,
		AttemptTimeout:          20 * time.Minute,
		RequestTimeout:          30 * time.Minute,
		TLSHandshakeTimeout:     10 * time.Second,
		MaxUpstreamAttempts:     DefaultMaxUpstreamAttempts,
		MaxCandidateProbes:      DefaultMaxCandidateProbes,
		CircuitFailureThreshold: DefaultCircuitFailureThreshold,
		CircuitSuccessThreshold: DefaultCircuitSuccessThreshold,
		CircuitOpenTimeout:      DefaultCircuitOpenTimeout,
		KeyRateLimitCooldown:    DefaultKeyRateLimitCooldown,
	}
	cases := []struct {
		name    string
		mutate  func(*GatewayConfig)
		wantErr bool
	}{
		{"valid defaults", nil, false},
		{"zero connect", func(g *GatewayConfig) { g.ConnectTimeout = 0 }, true},
		{"zero tls_handshake", func(g *GatewayConfig) { g.TLSHandshakeTimeout = 0 }, true},
		{"zero first_byte", func(g *GatewayConfig) { g.FirstByteTimeout = 0 }, true},
		// connect_timeout and body_idle_timeout bound independent phases —
		// a large dial budget with a tight inter-chunk idle budget (or vice
		// versa) must be accepted, not rejected.
		{"connect > body_idle (independent phases, must be accepted)", func(g *GatewayConfig) { g.ConnectTimeout = 60 * time.Second }, false},
		{"body_idle > header (independent phases, must be accepted)", func(g *GatewayConfig) { g.HeaderTimeout = 60 * time.Second }, false},
		{"header > attempt", func(g *GatewayConfig) { g.HeaderTimeout = 25 * time.Minute }, true},
		{"header == attempt (equal allowed)", func(g *GatewayConfig) { g.HeaderTimeout = 20 * time.Minute }, false},
		{"first_byte > attempt", func(g *GatewayConfig) { g.FirstByteTimeout = 25 * time.Minute }, true},
		{"first_byte == attempt (equal allowed)", func(g *GatewayConfig) { g.FirstByteTimeout = 20 * time.Minute }, false},
		{"attempt >= request", func(g *GatewayConfig) { g.AttemptTimeout = 30 * time.Minute }, true},
		{"zero max_upstream_attempts", func(g *GatewayConfig) { g.MaxUpstreamAttempts = 0 }, true},
		{"negative max_candidate_probes", func(g *GatewayConfig) { g.MaxCandidateProbes = -1 }, true},
		{"zero circuit_failure_threshold", func(g *GatewayConfig) { g.CircuitFailureThreshold = 0 }, true},
		{"negative circuit_success_threshold", func(g *GatewayConfig) { g.CircuitSuccessThreshold = -1 }, true},
		{"sub-second circuit_open_timeout", func(g *GatewayConfig) { g.CircuitOpenTimeout = 500 * time.Millisecond }, true},
		{"sub-second key_rate_limit_cooldown", func(g *GatewayConfig) { g.KeyRateLimitCooldown = 500 * time.Millisecond }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := valid
			if tc.mutate != nil {
				tc.mutate(&g)
			}
			err := validateGatewayTimeouts(&g)
			if tc.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected nil, got %v", err)
			}
		})
	}
}

// TestLoadGatewayRejectsExplicitZeroTimeout pins the behavior that explicit
// zero timeouts are rejected by validateGatewayTimeouts: with
// applyGatewayDefaults no longer running inside loadStrict, an explicit
// `0s` in the file is no longer silently papered over with the default — it
// reaches validateGatewayTimeouts as 0 and fails the `> 0` check. The user
// gets an error pointing at the bad field instead of a config that quietly
// runs with the default while looking like it honors the override.
func TestLoadGatewayRejectsExplicitZeroTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "server:\n  port: 8080\n" +
		"database:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n" +
		"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n" +
		"gateway:\n  request_timeout: 0s\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatalf("expected error for gateway.request_timeout: 0s; applyGatewayDefaults must NOT silently default-load it")
	}
}

// TestLoadGatewayPartialBlockKeepsDefaultsForOmittedFields pins the behaviour
// loadStrict relies on: defaults() seeds every gateway field, the yaml
// decoder only overwrites fields the user set, so a partial `gateway:` block
// ends up with explicit values where the user wrote them and default values
// where they didn't — no applyGatewayDefaults pass needed.
func TestLoadGatewayPartialBlockKeepsDefaultsForOmittedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Set only request_timeout; every other gateway field is omitted and
	// must come back as the built-in default.
	content := "server:\n  port: 8080\n" +
		"database:\n  driver: sqlite\n  sqlite_path: ./data/x.db\n" +
		"security:\n  provider_master_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n" +
		"gateway:\n  request_timeout: 45m\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The one explicit field round-trips.
	if cfg.Gateway.RequestTimeout != 45*time.Minute {
		t.Errorf("RequestTimeout = %v, want 45m (explicit value)", cfg.Gateway.RequestTimeout)
	}
	// Omitted fields keep their defaults().
	want := DefaultGatewayConfig()
	if cfg.Gateway.ConnectTimeout != want.ConnectTimeout {
		t.Errorf("ConnectTimeout = %v, want default %v", cfg.Gateway.ConnectTimeout, want.ConnectTimeout)
	}
	if cfg.Gateway.HeaderTimeout != want.HeaderTimeout {
		t.Errorf("HeaderTimeout = %v, want default %v", cfg.Gateway.HeaderTimeout, want.HeaderTimeout)
	}
	if cfg.Gateway.FirstByteTimeout != want.FirstByteTimeout {
		t.Errorf("FirstByteTimeout = %v, want default %v", cfg.Gateway.FirstByteTimeout, want.FirstByteTimeout)
	}
	if cfg.Gateway.BodyIdleTimeout != want.BodyIdleTimeout {
		t.Errorf("BodyIdleTimeout = %v, want default %v", cfg.Gateway.BodyIdleTimeout, want.BodyIdleTimeout)
	}
	if cfg.Gateway.AttemptTimeout != want.AttemptTimeout {
		t.Errorf("AttemptTimeout = %v, want default %v", cfg.Gateway.AttemptTimeout, want.AttemptTimeout)
	}
	if cfg.Gateway.TLSHandshakeTimeout != want.TLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want default %v", cfg.Gateway.TLSHandshakeTimeout, want.TLSHandshakeTimeout)
	}
}

// writeInstallLayout builds the directory layout the installer produces —
// <app-home>/bin/<binary> plus <app-home>/configs/config.yaml — and returns the
// app-home and the path of the binary inside it.
func writeInstallLayout(t *testing.T, configBody string) (appHome, exe string) {
	t.Helper()
	appHome = t.TempDir()
	binDir := filepath.Join(appHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	exe = filepath.Join(binDir, "yolorouter")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(appHome, "configs"), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appHome, "configs", "config.yaml"), []byte(configBody), 0o600); err != nil {
		t.Fatalf("write install config: %v", err)
	}
	return appHome, exe
}

// evalSymlinks resolves path for comparison against the config path resolution, which
// follows the executable symlink and so returns a fully resolved path.
func evalSymlinks(t *testing.T, path string) string {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// TestLoadExistingNeverGenerates: commands that act on an already-running
// deployment must fail loudly when no config can be found, not silently
// generate one (a fresh provider_master_key and an empty data directory) and
// then report on that empty deployment.
func TestLoadExistingNeverGenerates(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if _, _, err := LoadExisting("", "stop"); err == nil {
		t.Fatal("LoadExisting should fail when no config exists")
	}
	if _, err := os.Stat(filepath.Join(dir, "configs", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("LoadExisting must not generate a config, stat err = %v", err)
	}
}

// TestLoadExistingReadsResolvedConfig proves LoadExisting goes through the same
// resolution as Load, so an installed deployment is found from any cwd.
func TestLoadExistingReadsResolvedConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	path := filepath.Join(dir, "configs", "config.yaml")
	body := "server:\n    port: 9123\ndatabase:\n    driver: sqlite\n    sqlite_path: ../data/yolorouter.db\nsecurity:\n    provider_master_key: dGVzdC1rZXktZm9yLWxvYWQtZXhpc3RpbmctdGVzdHM=\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, gotPath, err := LoadExisting(path, "stop")
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	if cfg.Server.Port != 9123 {
		t.Fatalf("Server.Port = %d, want 9123", cfg.Server.Port)
	}
	if evalSymlinks(t, gotPath) != evalSymlinks(t, path) {
		t.Fatalf("returned path = %q, want the file that was read %q", gotPath, path)
	}
}

// TestLoadWithPathReturnsTheFileItRead: callers record the returned path and
// later print it as the deployment they acted on (db:reset's confirmation,
// db:rollback's target line). Resolving a second time to recover it could name
// a different file than the one loaded, since resolution prefers paths that
// exist and one can appear between the two calls.
func TestLoadWithPathReturnsTheFileItRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	path := filepath.Join(dir, "configs", "config.yaml")
	body := "server:\n    port: 9124\nsecurity:\n    provider_master_key: dGVzdC1rZXktZm9yLWxvYWQtZXhpc3RpbmctdGVzdHM=\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, got, err := LoadWithPath(path)
	if err != nil {
		t.Fatalf("LoadWithPath: %v", err)
	}
	if cfg.Server.Port != 9124 {
		t.Fatalf("Server.Port = %d, want 9124 (config not actually read)", cfg.Server.Port)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("returned path %q is not absolute", got)
	}
	if evalSymlinks(t, got) != evalSymlinks(t, path) {
		t.Fatalf("returned path = %q, want the file that was read %q", got, path)
	}
}

// TestLoadWithPathReturnsGeneratedPath: the first-run path generates the file,
// and the caller still needs to be told which one.
func TestLoadWithPathReturnsGeneratedPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, got, err := LoadWithPath("")
	if err != nil {
		t.Fatalf("LoadWithPath: %v", err)
	}
	want := filepath.Join(dir, "configs", "config.yaml")
	if evalSymlinks(t, got) != evalSymlinks(t, want) {
		t.Fatalf("returned path = %q, want the generated config %q", got, want)
	}
}

// TestLoadDerivesTheSameDatabasePathThroughEveryRouteToTheConfig: the database
// path is not only used to open a file. It is the identity two processes agree
// on — serve and stop derive the lock file from it, and on Windows the name of
// the kernel event stop signals, which is matched byte for byte. serve resolves
// its config from the working directory while stop can resolve the same config
// through the executable's installation, and those two routes produce different
// spellings of one directory whenever a symlink is involved. Anchoring on the
// config directory's resolved form is what makes both arrive at one string.
func TestLoadDerivesTheSameDatabasePathThroughEveryRouteToTheConfig(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "configs"), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	body := "server:\n    port: 8080\ndatabase:\n    driver: sqlite\n    sqlite_path: ../data/yolorouter.db\n" +
		"security:\n    provider_master_key: dGVzdC1rZXktZm9yLWxvYWQtZXhpc3RpbmctdGVzdHM=\n"
	if err := os.WriteFile(filepath.Join(real, "configs", "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(real, "data"), 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	link := filepath.Join(t.TempDir(), "app")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	viaReal, err := Load(filepath.Join(real, "configs", "config.yaml"))
	if err != nil {
		t.Fatalf("load via the real path: %v", err)
	}
	viaLink, err := Load(filepath.Join(link, "configs", "config.yaml"))
	if err != nil {
		t.Fatalf("load via the symlinked path: %v", err)
	}

	if viaReal.Database.SQLitePath != viaLink.Database.SQLitePath {
		t.Fatalf("database path differs by route to the same config:\n  via real path: %s\n  via symlink:   %s",
			viaReal.Database.SQLitePath, viaLink.Database.SQLitePath)
	}
}

// TestLoadKeepsTheDatabaseInsideTheDeploymentWhenConfigsIsASymlink: sqlite_path
// is relative to the deployment, and the shipped default walks up out of
// configs/ to reach data/. Resolving the config directory before that join
// would let the "../" escape the link target's parent instead — moving the
// database out of the deployment entirely, so the next start opens a path that
// does not exist, creates an empty file there and leaves the real one orphaned.
func TestLoadKeepsTheDatabaseInsideTheDeploymentWhenConfigsIsASymlink(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatalf("mkdir elsewhere: %v", err)
	}
	body := "server:\n    port: 8080\ndatabase:\n    driver: sqlite\n    sqlite_path: ../data/yolorouter.db\n" +
		"security:\n    provider_master_key: dGVzdC1rZXktZm9yLWxvYWQtZXhpc3RpbmctdGVzdHM=\n"
	if err := os.WriteFile(filepath.Join(elsewhere, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(app, "configs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A deployment that has been started has this; without it the path is left
	// in its unresolved spelling, which is a separate property covered below.
	if err := os.MkdirAll(filepath.Join(app, "data"), 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}

	cfg, err := Load(filepath.Join(app, "configs", "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := filepath.Join(evalSymlinks(t, app), "data", "yolorouter.db")
	if cfg.Database.SQLitePath != want {
		t.Fatalf("database placed at %q, want it inside the deployment at %q", cfg.Database.SQLitePath, want)
	}
}

// TestLoadLeavesTheDatabasePathUnresolvedBeforeTheDataDirectoryExists pins the
// edge of the identity guarantee rather than pretending it has none. Canonical
// spelling needs the directory to exist, which it does for any deployment that
// has been started once — but between writing a config and the first start it
// does not, and the path is then left exactly as it was joined. That is the
// pre-existing behaviour, and both processes only disagree if one of them ran
// in that window.
func TestLoadLeavesTheDatabasePathUnresolvedBeforeTheDataDirectoryExists(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	if err := os.MkdirAll(filepath.Join(app, "configs"), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	body := "server:\n    port: 8080\ndatabase:\n    driver: sqlite\n    sqlite_path: ../data/yolorouter.db\n" +
		"security:\n    provider_master_key: dGVzdC1rZXktZm9yLWxvYWQtZXhpc3RpbmctdGVzdHM=\n"
	if err := os.WriteFile(filepath.Join(app, "configs", "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(filepath.Join(app, "configs", "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if want := filepath.Join(app, "data", "yolorouter.db"); cfg.Database.SQLitePath != want {
		t.Fatalf("database path = %q, want the plain join %q", cfg.Database.SQLitePath, want)
	}
}

// TestInstallHintPointsAtTheInstallationWithoutActingOnIt covers what replaced
// the installation search. The executable's location is a good guess at which
// deployment someone meant, and a bad basis for acting: a guess that turns into
// a signalled process or a dropped table has to be right every time, while a
// guess printed as "try this" costs a line when it is wrong.
func TestInstallHintPointsAtTheInstallationWithoutActingOnIt(t *testing.T) {
	appHome, exe := writeInstallLayout(t, "server:\n    port: 8080\n")

	hint := installHint(func() (string, error) { return exe, nil }, "stop")

	installed := filepath.Join(appHome, "configs", "config.yaml")
	if !strings.Contains(hint, evalSymlinks(t, installed)) {
		t.Fatalf("hint = %q, want it to name the installation's config %q", hint, installed)
	}
	for _, want := range []string{"--config", "yolorouter stop"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint = %q, want it to contain %q", hint, want)
		}
	}
}

// TestLoadExistingReportsThePathItTriedAndChangesNothing: with no config in the
// working directory and no suggestion to offer, the error still has to name
// where it looked — and the command must leave the directory as it found it,
// which is the whole difference from the generating loader.
func TestLoadExistingReportsThePathItTriedAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, _, err := LoadExisting("", "stop")
	if err == nil {
		t.Fatal("expected an error: the working directory holds no config")
	}
	if !strings.Contains(err.Error(), filepath.Join("configs", "config.yaml")) {
		t.Fatalf("error should name the path it tried, got: %v", err)
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 0 {
		t.Fatalf("working directory should be untouched, got %v (err %v)", entries, readErr)
	}
}

// TestInstallHintIsSilentForABinaryThatBelongsToNoInstallation: a bare binary
// in a temporary directory has nothing to suggest, and the error then just
// names the path it looked at.
func TestInstallHintIsSilentForABinaryThatBelongsToNoInstallation(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "yolorouter")
	if err := os.WriteFile(bare, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	if hint := installHint(func() (string, error) { return bare, nil }, "stop"); hint != "" {
		t.Fatalf("installHint = %q, want no suggestion", hint)
	}
}

// TestInstallHintSurvivesAnUnlocatableExecutable: os.Executable does not work
// in a chroot with no /proc. That costs the suggestion, nothing else — the
// command still reports the path it tried.
func TestInstallHintSurvivesAnUnlocatableExecutable(t *testing.T) {
	if hint := installHint(func() (string, error) { return "", os.ErrNotExist }, "stop"); hint != "" {
		t.Fatalf("installHint = %q, want no suggestion", hint)
	}
}

// TestInstallHintQuotesThePathForPasting: the line exists to be pasted, and the
// machine-wide Windows install puts the deployment under %ProgramFiles%, so an
// unquoted path splits at the space in "Program Files" and the pasted command
// fails to parse instead of reaching the deployment.
func TestInstallHintQuotesThePathForPasting(t *testing.T) {
	root := t.TempDir()
	appHome := filepath.Join(root, "Program Files", "yolorouter")
	if err := os.MkdirAll(filepath.Join(appHome, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(appHome, "configs"), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	exe := filepath.Join(appHome, "bin", "yolorouter")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	installed := filepath.Join(appHome, "configs", "config.yaml")
	if err := os.WriteFile(installed, []byte("server:\n    port: 8080\n"), 0o600); err != nil {
		t.Fatalf("write install config: %v", err)
	}

	hint := installHint(func() (string, error) { return exe, nil }, "stop")

	quoted := quoteForShell(evalSymlinks(t, installed))
	if !strings.Contains(hint, quoted) {
		t.Fatalf("hint = %q, want the path quoted as %q", hint, quoted)
	}
	// The point of the quoting is that the argument survives as one word.
	after := hint[strings.Index(hint, "--config ")+len("--config "):]
	if strings.Contains(strings.TrimSuffix(after, "\n"), " ") && !strings.HasPrefix(after, quoted) {
		t.Fatalf("path is not a single shell word in %q", hint)
	}
}
