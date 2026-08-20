// Package config loads and manages the config file: built-in defaults, strict
// YAML parsing, and auto-generation of configs/config.yaml on first run.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Database     DatabaseConfig     `yaml:"database"`
	Log          LogConfig          `yaml:"log"`
	Security     SecurityConfig     `yaml:"security"`
	Update       UpdateConfig       `yaml:"update"`
	Gateway      GatewayConfig      `yaml:"gateway"`
	PriceCatalog PriceCatalogConfig `yaml:"price_catalog"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
	// ExternalURL is the origin this deployment is reached at from a
	// browser (e.g. "https://router.example.com"). When set, external-login
	// callback URLs are built from it verbatim; when empty they are derived
	// from each request's Host header, which is the right zero-config
	// default for single-host setups but trusts the client-controlled Host
	// on deployments whose proxy does not pin it. Set this on any
	// internet-exposed instance.
	ExternalURL string `yaml:"external_url"`
}

// GatewayConfig holds gateway (upstream relay) timeouts. All fields default
// to the idle-keepalive values when the `gateway` block is absent, so an
// upgrade without config changes picks up the new model automatically.
//
// Not every relay timeout is admin-tunable: the short first-byte/total
// budgets used while reading a non-2xx upstream error body
// (errorBodyFirstByteTimeout / errorBodyTotalBudget in
// internal/gateway/idle_reader.go) are deliberately fixed at 10s rather than
// exposed here. They exist purely to keep a stuck error body from stalling
// candidate failover and bound a diagnostic read, not to shape steady-state
// traffic, so there is no deployment-specific value worth tuning.
type GatewayConfig struct {
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	HeaderTimeout  time.Duration `yaml:"header_timeout"`
	// FirstByteTimeout bounds the wait from response header received to the
	// first body chunk. Reasoning models often flush a 200 header and then
	// silently think for minutes before emitting the first token; transport
	// ResponseHeaderTimeout cannot cover this gap because it stops ticking
	// as soon as the header arrives. Defaults to 600s (matches HeaderTimeout).
	FirstByteTimeout    time.Duration `yaml:"first_byte_timeout"`
	BodyIdleTimeout     time.Duration `yaml:"body_idle_timeout"`
	AttemptTimeout      time.Duration `yaml:"attempt_timeout"`
	RequestTimeout      time.Duration `yaml:"request_timeout"`
	TLSHandshakeTimeout time.Duration `yaml:"tls_handshake_timeout"`
	// MaxUpstreamAttempts caps how many upstream dispatches one request may
	// spend across all candidates and key rotations combined — the count
	// companion to RequestTimeout's wall-clock cap. Defaults to 3: the first
	// dispatch plus two retries.
	MaxUpstreamAttempts int `yaml:"max_upstream_attempts"`
	// MaxCandidateProbes caps candidate walks that are abandoned before
	// anything is sent (a rewriter refusing the candidate, and future
	// pre-dispatch skips), so a large candidate pool cannot be walked end to
	// end for free. Defaults to 20.
	MaxCandidateProbes int `yaml:"max_candidate_probes"`
	// CircuitFailureThreshold is how many consecutive provider faults open
	// that provider's breaker (traffic then skips it). Defaults to 5.
	CircuitFailureThreshold int `yaml:"circuit_failure_threshold"`
	// CircuitSuccessThreshold is how many successful probes close an open
	// breaker again. Defaults to 2.
	CircuitSuccessThreshold int `yaml:"circuit_success_threshold"`
	// CircuitOpenTimeout is how long an open breaker refuses traffic before
	// letting probe requests through. Defaults to 60s.
	CircuitOpenTimeout time.Duration `yaml:"circuit_open_timeout"`
	// KeyRateLimitCooldown is how long a plain 429 benches the key it hit in
	// its provider's rotation. The upstream's Retry-After is honoured
	// (clamped to 1s..10min); this value is the fallback when the header is
	// absent or unparsable. Defaults to 60s.
	KeyRateLimitCooldown time.Duration `yaml:"key_rate_limit_cooldown"`
}

// Default sizes for the gateway's per-request count budgets. Exported so the
// gateway can normalise a zero-valued config to the same numbers Load applies,
// without duplicating the literals.
const (
	DefaultMaxUpstreamAttempts = 3
	DefaultMaxCandidateProbes  = 20
)

// Default sizes for the per-provider circuit breaker, matching the values
// battle-tested in production deployments of this gateway's lineage.
const (
	DefaultCircuitFailureThreshold = 5
	DefaultCircuitSuccessThreshold = 2
	DefaultCircuitOpenTimeout      = 60 * time.Second
)

// DefaultKeyRateLimitCooldown is the fallback bench for a rate-limited key
// whose upstream stated no usable Retry-After.
const DefaultKeyRateLimitCooldown = 60 * time.Second

type DatabaseConfig struct {
	Driver     string `yaml:"driver"`
	SQLitePath string `yaml:"sqlite_path"`
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	User       string `yaml:"user"`
	Password   string `yaml:"password"`
	DBName     string `yaml:"dbname"`
	// SSLMode is a libpq sslmode value (disable/require/verify-ca/verify-full).
	// Defaults to "disable" for local-dev convenience; remote/production
	// Postgres deployments should set this explicitly (e.g. "require" or
	// "verify-full") to avoid sending credentials and data in the clear.
	SSLMode string `yaml:"sslmode"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

type SecurityConfig struct {
	ProviderMasterKey string `yaml:"provider_master_key"`
	// AllowPrivateUpstreams, when true, lets the SSRF guard dial
	// loopback/private/link-local/CGNAT/unique-local destinations for BOTH
	// connection tests and gateway relay (multicast, benchmark, reserved, and
	// unspecified addresses stay blocked regardless). Off by default: only a
	// single-tenant, self-hosted operator who deliberately points Yolorouter
	// at a LAN/localhost model server (Ollama, vLLM, LM Studio, one-api, …)
	// should turn it on. Never enable it on a multi-tenant or internet-exposed
	// deployment — it lets a provider base_url reach internal services and
	// cloud metadata endpoints (169.254.169.254).
	AllowPrivateUpstreams bool `yaml:"allow_private_upstreams"`
}

// UpdateConfig controls the version-update feature (the background update
// check surfaced via the system info API + the `update` CLI). Enabled
// defaults true so an auto-generated or legacy config that omits the whole
// `update` section does not silently disable updates — only an explicit
// `enabled: false` does. GitHubRepo overrides the binary's compiled-in
// version.DefaultGitHubRepo; empty falls back to it, and both empty (or
// Enabled=false) disable the feature entirely.
//
// GitHubProxy, when non-empty, is a prefix through which every update-related
// GitHub request (release lookup + asset download) is routed, for deployments
// where GitHub is slow or blocked. Empty means direct GitHub. On a mirror
// install it is seeded automatically from the YOLO_UPDATE_GITHUB_PROXY
// environment variable when the config is first generated.
type UpdateConfig struct {
	Enabled     bool   `yaml:"enabled"`
	GitHubRepo  string `yaml:"github_repo"`
	GitHubProxy string `yaml:"github_proxy"`
}

// PriceCatalogConfig controls the background refresh of the built-in model
// price seed (internal/pricecatalog). The seed is //go:embed-ded into the
// binary as a compile-time fallback; this section launches a background
// goroutine (pricecatalog.StartRefresh) that fetches a fresher catalog from the
// distribution endpoint (a Cloudflare Worker) and atomically swaps it in.
// A failed fetch never wipes a warm index — the worst case is "stays as it was"
// (only cover, never delete).
//
// Endpoint defaults to the live distribution Worker (prices.yolorouter.com), so
// every instance refreshes daily with zero config. Set it to "" to disable
// refresh and stay on the embedded seed, or to your own URL to self-host the
// distribution source.
//
// RefreshInterval bounds the gap between successful refreshes. The first fetch
// happens immediately on start, so a restart warms without waiting for a tick.
type PriceCatalogConfig struct {
	Endpoint        string        `yaml:"endpoint"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{Port: 8080},
		// Relative paths resolve against the config file's own directory
		// (see loadStrict below), and the default config lives at
		// configs/config.yaml — so "../data" lands the default data
		// directory as a top-level sibling of configs/, not nested inside
		// it.
		Database: DatabaseConfig{Driver: "sqlite", SQLitePath: "../data/yolorouter.db", SSLMode: "disable"},
		Log:      LogConfig{Level: "info"},
		// Enabled defaults true so a config that omits the `update` section
		// entirely (auto-generated, legacy) keeps updates ON — only an
		// explicit `enabled: false` disables them.
		Update: UpdateConfig{Enabled: true},
		// Gateway defaults mirror every other top-level section (Server,
		// Database, …) so generateDefaultConfig writes the real idle-keepalive
		// values to disk instead of five zero-value 0s that diverge from the
		// actual runtime behaviour and from configs/config.example.yaml.
		Gateway: DefaultGatewayConfig(),
		// The distribution Worker (prices.yolorouter.com) is live, so every
		// instance refreshes by default — no opt-in config needed. An operator
		// who wants a different source (self-hosted, or to disable refresh
		// entirely) overrides Endpoint with "" or their own URL.
		PriceCatalog: PriceCatalogConfig{
			Endpoint:        "https://prices.yolorouter.com/catalog.json",
			RefreshInterval: 24 * time.Hour,
		},
	}
}

// applyGatewayDefaults fills zero-value gateway timeouts with the idle-
// keepalive defaults so an absent `gateway` block upgrades automatically.
func applyGatewayDefaults(g *GatewayConfig) {
	if g.ConnectTimeout == 0 {
		g.ConnectTimeout = 5 * time.Second
	}
	if g.HeaderTimeout == 0 {
		g.HeaderTimeout = 600 * time.Second
	}
	if g.FirstByteTimeout == 0 {
		g.FirstByteTimeout = 600 * time.Second
	}
	if g.BodyIdleTimeout == 0 {
		g.BodyIdleTimeout = 60 * time.Second
	}
	if g.AttemptTimeout == 0 {
		g.AttemptTimeout = 20 * time.Minute
	}
	if g.RequestTimeout == 0 {
		g.RequestTimeout = 30 * time.Minute
	}
	if g.TLSHandshakeTimeout == 0 {
		g.TLSHandshakeTimeout = 10 * time.Second
	}
	if g.MaxUpstreamAttempts == 0 {
		g.MaxUpstreamAttempts = DefaultMaxUpstreamAttempts
	}
	if g.MaxCandidateProbes == 0 {
		g.MaxCandidateProbes = DefaultMaxCandidateProbes
	}
	if g.CircuitFailureThreshold == 0 {
		g.CircuitFailureThreshold = DefaultCircuitFailureThreshold
	}
	if g.CircuitSuccessThreshold == 0 {
		g.CircuitSuccessThreshold = DefaultCircuitSuccessThreshold
	}
	if g.CircuitOpenTimeout == 0 {
		g.CircuitOpenTimeout = DefaultCircuitOpenTimeout
	}
	if g.KeyRateLimitCooldown == 0 {
		g.KeyRateLimitCooldown = DefaultKeyRateLimitCooldown
	}
}

// DefaultGatewayConfig returns a GatewayConfig with the idle-keepalive defaults
// applied. Callers that need a value-shaped view of the production defaults
// (including tests in other packages, which cannot reach the unexported
// applyGatewayDefaults) use this instead of duplicating the literals.
func DefaultGatewayConfig() GatewayConfig {
	var g GatewayConfig
	applyGatewayDefaults(&g)
	return g
}

// resolveDefaultPath returns where Load looks when no --config was given: the
// process working directory. Deliberately NOT the installed-deployment search
// that resolveExisting does — every command that can generate a config goes
// through here, and having them silently retarget an installation would mean
// `serve` from an admin's shell attaching to the running service's database,
// `db:reset` wiping it, and `db:migrate` altering its schema, none of which the
// working-directory rule ever did.
func resolveDefaultPath() string {
	return filepath.Join("configs", "config.yaml")
}

// resolveExisting picks the config file for a command that acts on a deployment
// that must already exist: explicitPath when given, otherwise the working
// directory's configs/config.yaml. The same two places Load looks, and
// deliberately no more.
//
// It does not go looking for the installation this executable belongs to.
// Acting on a deployment nobody named is what made `stop` report "no running
// instance" against a live server to begin with, and every way of making that
// safe raises a question the command cannot answer: whose config is this, may
// this user signal that pid, which of two deployments did the operator mean.
// The installation is used to *suggest* instead — see installHint — which turns
// a wrong guess from a silent action into a line the operator reads and decides
// on.
func resolveExisting(explicitPath string) string {
	if explicitPath != "" {
		return explicitPath
	}
	return resolveDefaultPath()
}

// installHint describes the deployment this executable appears to belong to, in
// the imperative, for an error message: "try: yolorouter stop --config …".
// Empty when there is nothing to suggest.
//
// Being only a suggestion is what lets this guess liberally. Both layouts in
// the wild are offered — the installers put the binary in <app-home>/bin with
// the config in <app-home>/configs, while the machine-wide Windows install and
// a release binary dropped in a directory keep both together — and a wrong
// guess costs a line of output, not a signalled process or a dropped table.
func installHint(executable func() (string, error), command string) string {
	exe, err := executable()
	if err != nil {
		return ""
	}
	// A PATH entry may be a symlink into the install directory, and
	// os.Executable is permitted to return it unresolved. Following it is a
	// refinement: on failure the unresolved path is still worth guessing from.
	if resolved, resolveErr := filepath.EvalSymlinks(exe); resolveErr == nil && resolved != "" {
		exe = resolved
	}
	exeDir := filepath.Dir(exe)

	// One directory up first: <app-home>/bin/<binary> with the config in
	// <app-home>/configs is the layout every installer produces, so it is the
	// better guess when both hold a config — running an older binary once from
	// <app-home>/bin left a stray config there. At the filesystem root the
	// parent is the directory itself, which would only repeat the other
	// candidate.
	var candidates []string
	if parent := filepath.Dir(exeDir); parent != exeDir {
		candidates = append(candidates, filepath.Join(parent, "configs", "config.yaml"))
	}
	candidates = append(candidates, filepath.Join(exeDir, "configs", "config.yaml"))
	for _, candidate := range candidates {
		if _, statErr := os.Stat(candidate); statErr == nil {
			// The whole value of this line is that it can be pasted verbatim,
			// so the path is quoted for the shell the operator is holding.
			// The machine-wide Windows install lives under %ProgramFiles%,
			// where an unquoted path splits at "Program Files".
			return fmt.Sprintf("this binary looks installed at %s; try: yolorouter %s --config %s",
				filepath.Dir(filepath.Dir(candidate)), command, quoteForShell(candidate))
		}
	}
	return ""
}

// Load resolves the config path (explicitPath wins if non-empty, otherwise
// "configs/config.yaml" relative to the process cwd at call time), then:
//   - if the path exists: strict-parse it, no auto-generation ever happens
//   - if explicitPath was given but doesn't exist: hard error
//   - if using the default path and it doesn't exist: apply built-in
//     defaults, generate a random provider_master_key, and atomically write
//     the effective config out to that path so restarts reuse the same key
func Load(explicitPath string) (*Config, error) {
	cfg, _, err := LoadWithPath(explicitPath)
	return cfg, err
}

// LoadWithPath is Load, additionally returning the absolute path of the file it
// loaded (or generated), so a caller that has to name the file it acted on
// takes it from the call that read it rather than resolving a second time.
func LoadWithPath(explicitPath string) (*Config, string, error) {
	path := explicitPath
	if path == "" {
		path = resolveDefaultPath()
	}

	if _, err := os.Stat(path); err != nil {
		if explicitPath != "" {
			return nil, "", fmt.Errorf("config file not found at explicit path %s: %w", absOrSelf(path), err)
		}
		cfg, genErr := generateDefaultConfig(path)
		if genErr != nil {
			return nil, "", genErr
		}
		return cfg, absOrSelf(path), nil
	}
	cfg, err := loadResolved(path)
	if err != nil {
		return nil, "", err
	}
	return cfg, absOrSelf(path), nil
}

// absOrSelf makes path absolute, falling back to path itself. Callers print
// this for a human to act on, and a relative path only resolves in the
// process's own working directory — which for a service-launched process is
// nowhere near the install directory. Abs only fails if the working directory
// is unreadable, in which case the relative form still beats no path at all.
func absOrSelf(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// LoadExisting loads a config that must already exist, and never generates one.
// Commands that act on a deployment someone else created — stop, which signals a
// running server; db:rollback, which drops schema — use this instead of Load:
// generating a config would mint a fresh provider_master_key and an empty data
// directory, then have the command report on, or act on, that empty deployment
// rather than the real one.
//
// command names the subcommand, for the "try: yolorouter <command> --config …"
// suggestion built when nothing is there.
//
// The absolute path of the file it read comes back with the config so callers
// can name the deployment they acted on.
func LoadExisting(explicitPath, command string) (*Config, string, error) {
	path := resolveExisting(explicitPath)

	if _, err := os.Stat(path); err != nil {
		return nil, "", notFoundError(absOrSelf(path), explicitPath != "", installHint(os.Executable, command), err)
	}
	cfg, err := loadResolved(path)
	if err != nil {
		return nil, "", err
	}
	return cfg, absOrSelf(path), nil
}

// loadResolved strict-parses an existing config at an already-resolved path.
func loadResolved(path string) (*Config, error) {
	cfg, err := loadStrict(path)
	if err != nil {
		return nil, err
	}
	// An existing config is never regenerated, so a mirror installer upgrading a
	// prior direct install can't seed the proxy at generation time. Fill it from
	// the env the installer injects into the service unit — only when the file
	// leaves it empty, so an explicit value in the config still wins.
	applyUpdateProxyEnv(cfg)
	return cfg, nil
}

// applyUpdateProxyEnv fills update.github_proxy from YOLO_UPDATE_GITHUB_PROXY
// when the config leaves it empty, so the env var the mirror installer injects
// into the service unit takes effect without overriding an explicit config value.
func applyUpdateProxyEnv(cfg *Config) {
	if cfg.Update.GitHubProxy == "" {
		if proxy := os.Getenv("YOLO_UPDATE_GITHUB_PROXY"); proxy != "" {
			cfg.Update.GitHubProxy = proxy
		}
	}
}

func generateDefaultConfig(path string) (*Config, error) {
	cfg := defaults()
	key, err := randomMasterKey()
	if err != nil {
		return nil, fmt.Errorf("generate provider_master_key: %w", err)
	}
	cfg.Security.ProviderMasterKey = key

	// A mirror install (install.sh with YOLO_MIRROR) exports this, so record the
	// proxy in the generated file — self-update then routes through the same
	// mirror with no manual edit. Persisted here (before the write) so the CLI
	// `update`, run from a shell without the env, also reads it from config.
	applyUpdateProxyEnv(cfg)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}

	// The SQLite path is resolved relative to the config file's directory
	// (see loadStrict below), not the process cwd, so the data directory
	// must be created at that same resolved location.
	absConfigDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	sqlitePath := cfg.Database.SQLitePath
	if !filepath.IsAbs(sqlitePath) {
		sqlitePath = filepath.Join(absConfigDir, sqlitePath)
	}
	if err := os.MkdirAll(filepath.Dir(sqlitePath), 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	if err := atomicWriteConfig(path, cfg); err != nil {
		return nil, err
	}

	// Re-read to handle the race where a concurrent process won the write.
	return loadStrict(path)
}

func atomicWriteConfig(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal generated config: %w", err)
	}

	// Use a unique temp filename (not a fixed path+".tmp") so two processes
	// racing to auto-generate the same config on first boot can't clobber or
	// truncate each other's in-progress temp file.
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp config file: %w", err)
	}
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp config file: %w", err)
	}
	// fsync before Close so the freshly-generated provider_master_key is
	// actually durable on disk — without this, a crash or power loss right
	// after Link "publishes" the file below could leave an entry pointing
	// at content the kernel never flushed, and the only copy of that key
	// (nothing else knows it) is gone.
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temp config file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp config file: %w", err)
	}

	// Publish via a hard link, not Stat-then-Rename: Stat-then-Rename has a
	// TOCTOU race — two processes can both observe "doesn't exist" and both
	// proceed, and a plain os.Rename never fails just because the
	// destination already exists, so the second process to rename silently
	// overwrites the first process's file (and, worse, its
	// already-generated master key — anything encrypted under the first
	// key becomes unreadable). os.Link atomically fails with an "already
	// exists" error if the destination is taken, so at most one of any
	// racing processes ever successfully publishes.
	if err := os.Link(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		if errors.Is(err, os.ErrExist) {
			// Lost the race to another process generating the same file
			// first; discard our version and let the caller re-read the
			// winner.
			return nil
		}
		return fmt.Errorf("finalize config file: %w", err)
	}
	_ = os.Remove(tmpPath) // tmpPath and path are now two links to the same inode

	// Best-effort: fsync the parent directory too, so the new directory
	// entry itself (not just the file's data) survives a crash. Not fatal
	// if unsupported (some filesystems reject fsync on a directory fd) —
	// the file's own content is already durable from the Sync above either way.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func loadStrict(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config file %s: %w", path, err)
	}
	// The file holds security.provider_master_key in plaintext, so it is
	// treated like any other secret file. The actual check is platform-split
	// (perm_unix.go / perm_windows.go) because Unix permission bits only
	// exist on Unix — see PermEnforcementSupported for what that means on
	// Windows.
	if err := checkConfigFilePerm(info, path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	cfg := defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}
	// yaml.Decoder.Decode only consumes the first "---"-delimited document
	// in the stream — decoding again and requiring io.EOF here rejects a
	// config.yaml with a second document instead of silently ignoring it,
	// which could otherwise hide a real config value the file's author
	// expected to take effect.
	if err := decoder.Decode(new(Config)); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("config file %s contains more than one YAML document", path)
		}
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	// Note: applyGatewayDefaults is NOT called here. defaults() already
	// seeds every gateway field with the idle-keepalive values, and the
	// yaml decoder only overwrites fields the user actually set — omitted
	// fields keep their default. An explicit `0s` in the file overrides
	// the default to zero, which validateGatewayTimeouts rejects (> 0
	// required), so a typo surfaces as a load error instead of being
	// silently papered over with the default.
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config file %s: %w", path, err)
	}

	absDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	if !filepath.IsAbs(cfg.Database.SQLitePath) {
		cfg.Database.SQLitePath = filepath.Join(absDir, cfg.Database.SQLitePath)
	}
	// Follow links only after that join, never before it. The shipped default
	// walks up out of configs/ to reach data/, and resolving the config
	// directory first would let the "../" climb out of the link's target
	// instead of out of the deployment — putting the database somewhere else
	// entirely, so the next start would open a path that does not exist, create
	// an empty file there, and leave the real one orphaned. Joining first fixes
	// which file is meant; canonicalising what is left can only change how that
	// same file is spelled.
	//
	// The spelling is worth fixing because this path is also an identity two
	// processes have to agree on: serve and stop both derive the instance lock
	// from it, and on Windows the name of the kernel event stop signals, which
	// is matched byte for byte. serve resolves its config from the working
	// directory while stop can resolve the same config through the executable's
	// installation, and those two routes spell one directory differently as
	// soon as a link is involved. Best-effort: before a deployment's first
	// start the data directory does not exist yet, and the unresolved spelling
	// is what this replaces.
	if resolvedDir, resolveErr := filepath.EvalSymlinks(filepath.Dir(cfg.Database.SQLitePath)); resolveErr == nil {
		cfg.Database.SQLitePath = filepath.Join(resolvedDir, filepath.Base(cfg.Database.SQLitePath))
	}

	return cfg, nil
}

// validSSLModes is libpq's known sslmode value set — validated only when
// database.driver is "postgres" (SQLite deployments carry the SSLMode field's
// harmless "disable" zero-value default and never use it).
var validSSLModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true,
	"require": true, "verify-ca": true, "verify-full": true,
}

func validate(cfg *Config) error {
	if cfg.Database.Driver != "sqlite" && cfg.Database.Driver != "postgres" {
		return fmt.Errorf("database.driver must be \"sqlite\" or \"postgres\", got %q", cfg.Database.Driver)
	}
	if cfg.Database.Driver == "postgres" {
		if cfg.Database.Host == "" {
			return fmt.Errorf("database.host must not be empty when database.driver is \"postgres\"")
		}
		if cfg.Database.User == "" {
			return fmt.Errorf("database.user must not be empty when database.driver is \"postgres\"")
		}
		if cfg.Database.DBName == "" {
			return fmt.Errorf("database.dbname must not be empty when database.driver is \"postgres\"")
		}
		if cfg.Database.Port <= 0 || cfg.Database.Port > 65535 {
			return fmt.Errorf("database.port must be between 1 and 65535 when database.driver is \"postgres\", got %d", cfg.Database.Port)
		}
		if !validSSLModes[cfg.Database.SSLMode] {
			return fmt.Errorf("database.sslmode must be one of disable/allow/prefer/require/verify-ca/verify-full, got %q", cfg.Database.SSLMode)
		}
	}
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", cfg.Server.Port)
	}
	if !validLogLevels[cfg.Log.Level] {
		return fmt.Errorf("log.level must be one of debug/info/warn/error, got %q", cfg.Log.Level)
	}
	if err := validateMasterKey(cfg.Security.ProviderMasterKey); err != nil {
		return fmt.Errorf("security.provider_master_key: %w", err)
	}
	if err := validateGitHubRepo(cfg.Update.GitHubRepo); err != nil {
		return fmt.Errorf("update.github_repo: %w", err)
	}
	if err := validateGatewayTimeouts(&cfg.Gateway); err != nil {
		return err
	}
	if err := validatePriceCatalog(&cfg.PriceCatalog); err != nil {
		return err
	}
	return nil
}

// validatePriceCatalog rejects only the one genuinely broken combination: an
// endpoint configured (so the operator wants live refresh) with a non-positive
// refresh interval (which would make the ticker fire constantly or never). An
// empty endpoint is valid — it means "stay on the embedded seed", the default.
func validatePriceCatalog(p *PriceCatalogConfig) error {
	if p.Endpoint != "" && p.RefreshInterval <= 0 {
		return fmt.Errorf("price_catalog: refresh_interval must be > 0 when endpoint is set, got %v", p.RefreshInterval)
	}
	return nil
}

// validateGatewayTimeouts enforces two kinds of rules:
//
//  1. Every field must be strictly positive — a zero/negative value would
//     make a timer fire immediately or disable a dial timeout outright.
//
//  2. Only the ordering constraints that reflect a REAL nesting relationship
//     between phases of the same attempt:
//     - header_timeout <= attempt_timeout and first_byte_timeout <=
//     attempt_timeout: both are post-connect, pre-body-completion budgets
//     that occur WITHIN a single attempt, so neither can legitimately
//     exceed the attempt's own total budget.
//     - attempt_timeout < request_timeout: a single attempt must leave room
//     for at least the possibility of failover within the total request
//     budget.
//
// ConnectTimeout (TCP dial) and BodyIdleTimeout (inter-chunk gap once the
// body is already streaming) are deliberately NOT ordered against each
// other or against HeaderTimeout: they bound independent, non-overlapping
// phases of the same attempt (dial, then headers, then body), so a
// dial-timeout/idle-timeout/header-timeout combination like
// connect_timeout=30s (slow network) + body_idle_timeout=10s (tight
// steady-state gap) is a perfectly valid deployment choice, not a
// misconfiguration — a prior version of this function rejected it.
func validateGatewayTimeouts(g *GatewayConfig) error {
	if g.ConnectTimeout <= 0 || g.HeaderTimeout <= 0 || g.FirstByteTimeout <= 0 || g.BodyIdleTimeout <= 0 || g.AttemptTimeout <= 0 || g.RequestTimeout <= 0 || g.TLSHandshakeTimeout <= 0 {
		return fmt.Errorf("gateway: all timeouts must be > 0")
	}
	if g.HeaderTimeout > g.AttemptTimeout {
		return fmt.Errorf("gateway: header_timeout (%v) must be <= attempt_timeout (%v)", g.HeaderTimeout, g.AttemptTimeout)
	}
	if g.FirstByteTimeout > g.AttemptTimeout {
		return fmt.Errorf("gateway: first_byte_timeout (%v) must be <= attempt_timeout (%v)", g.FirstByteTimeout, g.AttemptTimeout)
	}
	if g.AttemptTimeout >= g.RequestTimeout {
		return fmt.Errorf("gateway: attempt_timeout (%v) must be < request_timeout (%v)", g.AttemptTimeout, g.RequestTimeout)
	}
	if g.MaxUpstreamAttempts <= 0 || g.MaxCandidateProbes <= 0 {
		return fmt.Errorf("gateway: max_upstream_attempts and max_candidate_probes must be > 0")
	}

	if g.CircuitFailureThreshold <= 0 || g.CircuitSuccessThreshold <= 0 {
		return fmt.Errorf("gateway: circuit_failure_threshold and circuit_success_threshold must be > 0")
	}
	// A sub-second open window makes no sense against LLM upstreams and, cut
	// into per-probe intervals, degenerates toward an always-pass rate limit.
	if g.CircuitOpenTimeout < time.Second {
		return fmt.Errorf("gateway: circuit_open_timeout must be >= 1s")
	}
	// Same sub-second floor for a key's rate-limit bench: shorter than this
	// cannot separate two requests and only adds pool-map churn.
	if g.KeyRateLimitCooldown < time.Second {
		return fmt.Errorf("gateway: key_rate_limit_cooldown must be >= 1s")
	}
	return nil
}

// validateGitHubRepo accepts an empty repo (falls back to the compiled-in
// version.DefaultGitHubRepo, or disables updates if that is also empty) but
// rejects a malformed non-empty value early — a typo like "ownerrepo" or
// "owner/repo/extra" would otherwise only surface as a 404 from GitHub's
// releases API at runtime, with no hint that the config value was the cause.
func validateGitHubRepo(repo string) error {
	if repo == "" {
		return nil
	}
	if strings.ContainsAny(repo, " \t") {
		return fmt.Errorf("must not contain whitespace, got %q", repo)
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("must be %q with exactly one slash, got %q", "owner/repo", repo)
	}
	return nil
}

// validLogLevels are the level strings pkg/logger.Init actually recognizes
// via zapcore.Level.UnmarshalText. That function's own copied-verbatim
// implementation silently falls back to info on any unparseable value
// rather than erroring — validating here instead means a typo'd log.level
// (e.g. "debu") fails loudly at config-load time instead of silently
// running at the wrong verbosity forever.
var validLogLevels = map[string]bool{
	"debug": true, "info": true, "warn": true, "error": true,
}

// validateMasterKey requires a standard-base64-encoded 32-byte AES-256 key,
// matching what randomMasterKey generates — not just "non-empty", since a
// malformed or wrong-length key would only surface as a confusing failure
// later, at first encrypt/decrypt use.
func validateMasterKey(key string) error {
	if key == "" {
		return fmt.Errorf("must not be empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return fmt.Errorf("must be standard base64, got invalid value: %w", err)
	}
	if len(decoded) != 32 {
		return fmt.Errorf("must decode to exactly 32 bytes (AES-256), got %d", len(decoded))
	}
	return nil
}

func randomMasterKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// notFoundError explains a missing config in terms of how its path was chosen,
// and — when the path came from the working directory rather than the operator
// — appends what to type instead. Telling someone who just passed --config to
// pass --config is the failure mode this exists to avoid; so is naming a
// default path without saying where the real one probably is.
func notFoundError(path string, explicit bool, hint string, err error) error {
	if explicit {
		return fmt.Errorf("config file not found at explicit path %s: %w", path, err)
	}
	if hint == "" {
		return fmt.Errorf("config file not found at %s (pass --config with the path to the deployment's config.yaml): %w", path, err)
	}
	return fmt.Errorf("config file not found at %s — %s: %w", path, hint, err)
}
