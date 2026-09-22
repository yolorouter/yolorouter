// Black-box tests for the key auto-recovery loop: an external test package
// so the loop is driven only through its exported surface (NewLoop +
// Start), against a real migrated SQLite database and the real provider
// service with the canned provider-client double — the same fixture shape
// the provider service tests use. The settings snapshot is read through
// the REAL settings service wherever the scenario allows (the interval is
// injected by writing the settings rows directly, bypassing the PUT
// validation's 1-minute floor), and through a stub source only where the
// scenario needs mid-run changes or injected failures that the real
// service's 30-second cache TTL makes unobservable at test speed.
package keyrecovery_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/keyrecovery"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/service/providerclient/providerclienttest"
	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// stubSettings is a controllable SettingsSource: tests flip the snapshot
// or arm an error mid-run. On error it keeps returning the last snapshot —
// mirroring the real service's documented fail-open contract (error plus
// last-known-good value), so the loop's error handling is exercised
// against the same shape production serves.
type stubSettings struct {
	mu   sync.Mutex
	snap settings.KeyAutoRecoverySetting
	err  error
}

func (s *stubSettings) GetKeyAutoRecovery(ctx context.Context) (settings.KeyAutoRecoverySetting, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap, 1, s.err
}

func (s *stubSettings) set(snap settings.KeyAutoRecoverySetting) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = snap
}

func (s *stubSettings) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// fixture is one loop test's world: a migrated database, the real provider
// service over the canned client, and the real settings service.
type fixture struct {
	db       *gorm.DB
	client   *providerclienttest.Fake
	prov     *provider.ProviderService
	settings *systemsettings.SystemSettingsService
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	client := &providerclienttest.Fake{Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}}
	prov := provider.NewProviderService(db, testutil.ProviderSecrets(), client)
	return &fixture{
		db:       db,
		client:   client,
		prov:     prov,
		settings: systemsettings.NewSystemSettingsService(db),
	}
}

// setRecoveryRows writes the key-auto-recovery settings pair directly,
// bypassing the PUT validation's 1-minute floor so tests can inject a
// sub-minute interval (0 = sweep on every heartbeat). The settings
// service's first (cold-cache) read picks the rows up.
func (f *fixture) setRecoveryRows(t *testing.T, enabled, intervalMinutes string) {
	t.Helper()
	exec := func(query string, value string) {
		if err := f.db.Exec(query, value).Error; err != nil {
			t.Fatalf("write settings row: %v", err)
		}
	}
	exec("UPDATE system_settings SET value = ? WHERE key = 'key_auto_recovery_enabled'", enabled)
	exec("UPDATE system_settings SET value = ? WHERE key = 'key_auto_recovery_interval_minutes'", intervalMinutes)
}

// seedProvider creates an enabled provider whose first key passes and is
// enabled — the "healthy before the incident" starting state.
func (f *fixture) seedProvider(t *testing.T, name string) *provider.ProviderView {
	t.Helper()
	view, err := f.prov.CreateProvider(context.Background(), provider.CreateProviderInput{
		Name: name, BaseURL: "https://" + name + ".example.com",
		KeyLabel: "k-" + name, KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234",
		TestModel: "tm-" + name, ManagementStatus: model.ProviderStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	return view
}

// addKey appends another key to a provider through the public path:
// created disabled, tested, and enabled because the canned client is
// (temporarily) answering success.
func (f *fixture) addKey(t *testing.T, providerID uint, label, testModel string) *provider.ProviderKeyView {
	t.Helper()
	view, err := f.prov.CreateProviderKey(context.Background(), providerID, provider.CreateKeyInput{
		Label: label, Plaintext: "sk-abcdefghijklmnopqrstuvwxyz1234",
		TestModel: testModel, ManagementStatus: model.ProviderKeyStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProviderKey failed: %v", err)
	}
	return view
}

// demoteKey reproduces "the system kicked this key out of rotation": a
// retest whose decisive failure flips verification to Failed while the
// management switch stays on. Restores the canned success answer
// afterwards, as if the upstream healed.
func (f *fixture) demoteKey(t *testing.T, providerID, keyID uint) {
	t.Helper()
	f.client.Result = providerclient.TestResult{Outcome: providerclient.TestAuthFailed, DurationMs: 5}
	view, err := f.prov.TestProviderKey(context.Background(), providerID, keyID, time.Now().UTC())
	if err != nil {
		t.Fatalf("demoting retest failed: %v", err)
	}
	if view.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("test setup: expected the demoting retest to leave the key Failed, got %d", view.VerificationStatus)
	}
	f.client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
}

// probeRecorder records (model, timestamp) for every canned-client call,
// read from inside the client's side effect. The loop probes serially on
// one goroutine, and test reads go through the mutex, so the record is
// race-free even while the loop runs.
type probeRecorder struct {
	mu     sync.Mutex
	events []probeEvent
}

type probeEvent struct {
	model string
	at    time.Time
}

func (r *probeRecorder) record(model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, probeEvent{model: model, at: time.Now()})
}

func (r *probeRecorder) models() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.model
	}
	return out
}

func (r *probeRecorder) times() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]time.Time, len(r.events))
	for i, e := range r.events {
		out[i] = e.at
	}
	return out
}

// listenerRecorder collects retest-passed listener callbacks — the
// gateway's bench-release signal. Zero callbacks means a recovery never
// fired the clear-demotion hook.
type listenerRecorder struct {
	mu    sync.Mutex
	calls []uint
}

func (r *listenerRecorder) onPassed(keyID uint, configVersion int, observedAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, keyID)
}

func (r *listenerRecorder) keyIDs() []uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint(nil), r.calls...)
}

func keyRow(t *testing.T, db *gorm.DB, id uint) model.ProviderKey {
	t.Helper()
	var k model.ProviderKey
	if err := db.Where("id = ?", id).First(&k).Error; err != nil {
		t.Fatalf("reload key %d: %v", id, err)
	}
	return k
}

func providerRow(t *testing.T, db *gorm.DB, id uint) model.Provider {
	t.Helper()
	var p model.Provider
	if err := db.Where("id = ?", id).First(&p).Error; err != nil {
		t.Fatalf("reload provider %d: %v", id, err)
	}
	return p
}

func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForQuiet polls calls() until the count has held steady for a full
// quiet window and returns the settled value — the sweep in flight when
// the observed window opened is over, and no new one has started. Freezing
// an observation mid-stream would credit the previous interval's momentum
// to the setting under test, so anything that freezes a count settles
// first. The quiet window must exceed a full sweep on a loaded CI runner
// (probe + commits + scan query), hence the generous 200ms in callers.
func waitForQuiet(t *testing.T, timeout time.Duration, quiet time.Duration, calls func() int) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := calls()
	lastChange := time.Now()
	for time.Since(lastChange) < quiet {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the probe count to settle (last=%d)", last)
		}
		time.Sleep(2 * time.Millisecond)
		if now := calls(); now != last {
			last = now
			lastChange = time.Now()
		}
	}
	return last
}

// runFor starts the loop with the given settings source, lets it run for
// the given duration, then stops it and returns. The "nothing must happen"
// tests are built on this.
func runFor(t *testing.T, fx *fixture, src keyrecovery.SettingsSource, d time.Duration, gap time.Duration) {
	t.Helper()
	loop := keyrecovery.NewLoop(keyrecovery.Config{
		DB: fx.db, Settings: src, Providers: fx.prov,
		Heartbeat: 2 * time.Millisecond, ProbeGap: gap,
	})
	stop := loop.Start(context.Background())
	time.Sleep(d)
	stop()
}

// captureLogs runs fn with the global logger writing to a temp file at the
// given level and returns what came out — the settlement-log test's
// pattern. Sequential tests only (the logger is a process global); no test
// in this package uses t.Parallel.
func captureLogs(t *testing.T, level string, fn func()) string {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "loop.log")
	logger.Init(logger.Config{Level: level, Filename: logFile, Console: false})
	t.Cleanup(func() {
		_ = logger.Sync()
		logger.Init(logger.Config{Filename: os.DevNull})
	})

	fn()
	_ = logger.Sync()

	b, err := os.ReadFile(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

// TestLoopRecoversAKickedKeyThroughTheRealRetestPath is the end-to-end
// recovery chain: a provider key the system demoted (decisive retest
// failure, management switch still on) is probed by the loop once the
// scan point arrives, and the EXISTING retest path does everything —
// verification flips back to Passed, last_tested_at and the per-target
// breakdown land in the columns, the retest-passed listener (the gateway
// bench release) fires, and the management switch is never touched. The
// interval is injected by writing the settings rows directly (0 minutes =
// every heartbeat is a scan point); the read goes through the real
// settings service.
func TestLoopRecoversAKickedKeyThroughTheRealRetestPath(t *testing.T) {
	fx := newFixture(t)
	p := fx.seedProvider(t, "healed")
	keyID := p.Keys[0].ID
	fx.demoteKey(t, p.ID, keyID)

	rec := &listenerRecorder{}
	fx.prov.SetKeyRetestPassedListener(rec.onPassed)

	fx.setRecoveryRows(t, "true", "0")
	started := time.Now().UTC()

	loop := keyrecovery.NewLoop(keyrecovery.Config{
		DB: fx.db, Settings: fx.settings, Providers: fx.prov,
		Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
	})
	stop := loop.Start(context.Background())
	defer stop()

	waitFor(t, 3*time.Second, "the key to be auto-recovered", func() bool {
		return keyRow(t, fx.db, keyID).VerificationStatus == model.VerificationStatusPassed
	})

	row := keyRow(t, fx.db, keyID)
	if row.ManagementStatus != model.ProviderKeyStatusEnabled {
		t.Errorf("management status changed to %d — the loop must never touch the management plane", row.ManagementStatus)
	}
	if row.LastTestedAt == nil || row.LastTestedAt.Before(started) {
		t.Errorf("last_tested_at = %v, want a fresh timestamp after %v — the loop must land in the real retest columns", row.LastTestedAt, started)
	}
	// The per-target breakdown is the proof the probe ran the full
	// destination-verification chain rather than a lightweight ping: the
	// column holds one JSON entry per destination probed.
	targets := row.LastTestTargets
	if targets == nil || !strings.Contains(*targets, string(protocols.ProtocolOpenAI)) {
		t.Errorf("last_test_targets = %v, want the openai destination breakdown the retest path records", targets)
	}
	if ids := rec.keyIDs(); len(ids) != 1 || ids[0] != keyID {
		t.Errorf("retest-passed listener calls = %v, want exactly [%d] — recovery must release the gateway bench", ids, keyID)
	}
}

// TestLoopHonorsTheDisabledSwitch pins the first boundary: with the
// setting off, a failed key sitting at an elapsed scan point is never
// probed. The rows are written before the loop's first (cold-cache) read.
func TestLoopHonorsTheDisabledSwitch(t *testing.T) {
	fx := newFixture(t)
	p := fx.seedProvider(t, "off")
	keyID := p.Keys[0].ID
	fx.demoteKey(t, p.ID, keyID)
	fx.setRecoveryRows(t, "false", "0")

	callsBefore := fx.client.Calls
	runFor(t, fx, fx.settings, 150*time.Millisecond, 0)

	if got := fx.client.Calls - callsBefore; got != 0 {
		t.Fatalf("client made %d probe calls with the setting disabled, want 0", got)
	}
	if row := keyRow(t, fx.db, keyID); row.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("verification status = %d, want still Failed", row.VerificationStatus)
	}
}

// TestLoopWaitsAFullIntervalAfterStart pins the startup debounce: the
// scan-point anchor is the start instant, so with a 1-minute interval no
// heartbeat inside a freshly started loop reaches the scan point. The
// stub source stands in for the settings service because the scenario
// needs an interval the real service's cache would keep serving anyway —
// only the decision arithmetic is under test here.
func TestLoopWaitsAFullIntervalAfterStart(t *testing.T) {
	fx := newFixture(t)
	p := fx.seedProvider(t, "debounce")
	fx.demoteKey(t, p.ID, p.Keys[0].ID)

	src := &stubSettings{}
	src.set(settings.KeyAutoRecoverySetting{Enabled: true, IntervalMinutes: 1})

	callsBefore := fx.client.Calls
	runFor(t, fx, src, 150*time.Millisecond, 0)

	if got := fx.client.Calls - callsBefore; got != 0 {
		t.Fatalf("client made %d probe calls inside the first interval, want 0 (startup debounce)", got)
	}
}

// TestLoopRecomputesTheScanPointWhenTheIntervalChanges pins the interval
// change propagation: the next scan point is lastSweep + the interval read
// on the CURRENT beat, not one captured at start or at sweep time. The
// sequence 0 → 1 minute → 0 exercises both directions: sweeps stop the
// moment the interval grows (a stale captured interval would keep
// sweeping) and resume the moment it shrinks again (a stale scan point
// computed from the 1-minute interval would keep waiting). The key never
// recovers (inconclusive outcome), so every sweep probes it again — that
// repetition is what makes each phase observable.
func TestLoopRecomputesTheScanPointWhenTheIntervalChanges(t *testing.T) {
	fx := newFixture(t)
	p := fx.seedProvider(t, "recompute")
	fx.demoteKey(t, p.ID, p.Keys[0].ID)
	// Inconclusive outcome so the key stays in the scan set forever.
	fx.client.Result = providerclient.TestResult{Outcome: providerclient.TestUnreachable, DurationMs: 5}

	src := &stubSettings{}
	src.set(settings.KeyAutoRecoverySetting{Enabled: true, IntervalMinutes: 0})

	rec := &probeRecorder{}
	fx.client.SideEffect = func() { rec.record(fx.client.LastModel) }

	calls := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.events)
	}

	loop := keyrecovery.NewLoop(keyrecovery.Config{
		DB: fx.db, Settings: src, Providers: fx.prov,
		Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
	})
	stop := loop.Start(context.Background())
	defer stop()

	// Phase 1: interval 0 — sweeps happen.
	waitFor(t, 3*time.Second, "the first sweep", func() bool { return calls() >= 2 })

	// Phase 2: grow the interval — the next scan point moves one minute
	// out, so the probe count must freeze. The change binds on the beat
	// after it lands; a sweep already in flight (past its settings read)
	// legitimately finishes, so settle first, then observe the freeze.
	src.set(settings.KeyAutoRecoverySetting{Enabled: true, IntervalMinutes: 1})
	frozen := waitForQuiet(t, 5*time.Second, 200*time.Millisecond, calls)
	time.Sleep(150 * time.Millisecond)
	if got := calls(); got != frozen {
		t.Fatalf("probe count moved from %d to %d while the interval was 1 minute, want frozen", frozen, got)
	}

	// Phase 3: shrink it back — the scan point is recomputed from the new
	// interval, so sweeps resume immediately rather than waiting out the
	// stale 1-minute point.
	src.set(settings.KeyAutoRecoverySetting{Enabled: true, IntervalMinutes: 0})
	waitFor(t, 3*time.Second, "sweeps to resume after the interval shrank", func() bool { return calls() > frozen })
}

// TestLoopSkipsNeedsReentryManualDisableAndDisabledProviders pins the
// scan-set and in-memory filters in one scenario:
//   - a needs-reentry key (authorized version trails the provider's
//     destination) is skipped — retesting it cannot work;
//   - a manually disabled key never even enters the scan set;
//   - a key under a management-disabled provider is skipped in memory;
//   - an enabled+untested key is not scan-set material either;
//   - the one eligible key (enabled, failed, current destination,
//     enabled provider) is the only one probed, and recovers.
func TestLoopSkipsNeedsReentryManualDisableAndDisabledProviders(t *testing.T) {
	fx := newFixture(t)

	// Provider "mixed": k-reentry fails first, THEN the destination moves,
	// stranding its authorization at the old version. The move goes through
	// the repository's real base-URL path (the write that bumps
	// destination_version).
	mixed := fx.seedProvider(t, "mixed")
	reentryKey := mixed.Keys[0].ID
	fx.demoteKey(t, mixed.ID, reentryKey)
	if _, err := repository.UpdateProviderBaseURL(fx.db, mixed.ID, "https://moved.example.com", time.Now().UTC()); err != nil {
		t.Fatalf("move base URL: %v", err)
	}
	if p := providerRow(t, fx.db, mixed.ID); p.DestinationVersion != 2 {
		t.Fatalf("test setup: destination version = %d, want 2", p.DestinationVersion)
	}

	// k-ok joins after the move, so its authorization is current; then the
	// system kicks it out.
	okKey := fx.addKey(t, mixed.ID, "k-ok", "tm-ok")
	if okKey.NeedsReentry {
		t.Fatal("test setup: k-ok must be current-destination")
	}
	fx.demoteKey(t, mixed.ID, okKey.ID)

	// k-manual is kicked out too, then the admin disables it by hand.
	manualKey := fx.addKey(t, mixed.ID, "k-manual", "tm-manual")
	fx.demoteKey(t, mixed.ID, manualKey.ID)
	if err := fx.prov.SetProviderKeyStatus(mixed.ID, manualKey.ID, false, time.Now().UTC()); err != nil {
		t.Fatalf("disable k-manual: %v", err)
	}

	// Provider "shelf": management-disabled with a kicked key under it.
	shelf := fx.seedProvider(t, "shelf")
	shelfKey := shelf.Keys[0].ID
	fx.demoteKey(t, shelf.ID, shelfKey)
	if err := fx.prov.SetProviderStatus(shelf.ID, false, time.Now().UTC()); err != nil {
		t.Fatalf("disable provider shelf: %v", err)
	}

	// k-untested: enabled but never verified (inconclusive create probe).
	// Enabled only by direct write — the service layer correctly refuses
	// to enable an unverified key, and this row state exists solely to
	// pin the scan query's verification predicate.
	fx.client.Result = providerclient.TestResult{Outcome: providerclient.TestUnreachable, DurationMs: 5}
	untestedKey := fx.addKey(t, mixed.ID, "k-untested", "tm-untested")
	fx.client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}
	if err := fx.db.Exec("UPDATE provider_keys SET management_status = ? WHERE id = ?",
		model.ProviderKeyStatusEnabled, untestedKey.ID).Error; err != nil {
		t.Fatalf("force-enable k-untested: %v", err)
	}

	rec := &probeRecorder{}
	fx.client.SideEffect = func() { rec.record(fx.client.LastModel) }

	fx.setRecoveryRows(t, "true", "0")
	loop := keyrecovery.NewLoop(keyrecovery.Config{
		DB: fx.db, Settings: fx.settings, Providers: fx.prov,
		Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
	})
	stop := loop.Start(context.Background())

	// The warn level is part of the filter contract: keys skipped in
	// memory never reach TestProviderKey, so no per-round error noise
	// may appear. A needs-reentry key that lost its filter would be
	// retried and refused by the retest path every round — visible here.
	out := captureLogs(t, "warn", func() {
		waitFor(t, 3*time.Second, "k-ok to recover", func() bool {
			return keyRow(t, fx.db, okKey.ID).VerificationStatus == model.VerificationStatusPassed
		})
		time.Sleep(30 * time.Millisecond) // a further round, were a filter missing
	})
	stop()

	if strings.Contains(out, "probe errored") {
		t.Errorf("a scan-set key was retried and refused — an in-memory filter is missing; log said: %s", out)
	}

	for _, m := range rec.models() {
		if m != "tm-ok" {
			t.Errorf("probe for model %q — only tm-ok (the eligible key) may be probed; full order: %v", m, rec.models())
		}
	}
	if row := keyRow(t, fx.db, reentryKey); row.VerificationStatus != model.VerificationStatusFailed {
		t.Errorf("needs-reentry key was probed/recovered: verification = %d, want Failed", row.VerificationStatus)
	}
	if row := keyRow(t, fx.db, manualKey.ID); row.VerificationStatus != model.VerificationStatusFailed || row.ManagementStatus != model.ProviderKeyStatusDisabled {
		t.Errorf("manually disabled key changed: verification = %d management = %d, want Failed/Disabled", row.VerificationStatus, row.ManagementStatus)
	}
	if row := keyRow(t, fx.db, shelfKey); row.VerificationStatus != model.VerificationStatusFailed {
		t.Errorf("key under the disabled provider was probed/recovered: verification = %d, want Failed", row.VerificationStatus)
	}
	if row := keyRow(t, fx.db, untestedKey.ID); row.VerificationStatus != model.VerificationStatusUntested {
		t.Errorf("untested key was probed: verification = %d, want Untested", row.VerificationStatus)
	}
}

// TestLoopProbesSeriallyWithAGap pins the pacing: one round probes its
// keys one at a time, in scan order, with at least the configured gap
// between consecutive probes. Three kicked keys under one provider are
// seeded; their distinct test models make the probe order observable.
func TestLoopProbesSeriallyWithAGap(t *testing.T) {
	fx := newFixture(t)
	p := fx.seedProvider(t, "serial")
	k1 := p.Keys[0].ID
	k2 := fx.addKey(t, p.ID, "k-b", "tm-b").ID
	k3 := fx.addKey(t, p.ID, "k-c", "tm-c").ID
	for _, id := range []uint{k1, k2, k3} {
		fx.demoteKey(t, p.ID, id)
	}

	rec := &probeRecorder{}
	fx.client.SideEffect = func() { rec.record(fx.client.LastModel) }

	fx.setRecoveryRows(t, "true", "0")
	// The gap is deliberately large relative to the per-probe database
	// work (claim + commit + reload each cost a few milliseconds of
	// SQLite I/O, which alone would space probes slightly apart): a
	// removed gap sleep must fail the threshold below on its own merits,
	// not hide behind incidental I/O latency.
	const gap = 150 * time.Millisecond
	loop := keyrecovery.NewLoop(keyrecovery.Config{
		DB: fx.db, Settings: fx.settings, Providers: fx.prov,
		Heartbeat: 2 * time.Millisecond, ProbeGap: gap,
	})
	stop := loop.Start(context.Background())
	defer stop()

	waitFor(t, 3*time.Second, "all three keys to recover", func() bool {
		return keyRow(t, fx.db, k1).VerificationStatus == model.VerificationStatusPassed &&
			keyRow(t, fx.db, k2).VerificationStatus == model.VerificationStatusPassed &&
			keyRow(t, fx.db, k3).VerificationStatus == model.VerificationStatusPassed
	})

	got := rec.models()
	want := []string{"tm-serial", "tm-b", "tm-c"}
	if len(got) != len(want) {
		t.Fatalf("probe order = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("probe order = %v, want %v (scan order, serial)", got, want)
		}
	}
	times := rec.times()
	for i := 1; i < len(times); i++ {
		if !times[i].After(times[i-1]) {
			t.Errorf("probe %d did not start after probe %d — probes must be serial", i, i-1)
		}
	}
	// Consecutive probes of DIFFERENT keys must be at least most of the
	// configured gap apart (each key probes exactly one destination here,
	// so consecutive events are consecutive keys). The 50ms tolerance
	// absorbs scheduler jitter without letting the gap collapse to zero.
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < gap-50*time.Millisecond {
			t.Errorf("gap between probe %d and %d = %v, want >= %v", i-1, i, d, gap-50*time.Millisecond)
		}
	}
}

// TestLoopContinuesTheRoundWhenOneProbeErrors pins the per-key error
// isolation: provider err's destination makes the client itself refuse
// the call (a transport-level error, not a test outcome); provider ok's
// key sits later in the same round and must still be probed and recover.
func TestLoopContinuesTheRoundWhenOneProbeErrors(t *testing.T) {
	fx := newFixture(t)
	broken := fx.seedProvider(t, "broken")
	healthy := fx.seedProvider(t, "healthy")
	fx.demoteKey(t, broken.ID, broken.Keys[0].ID)
	fx.demoteKey(t, healthy.ID, healthy.Keys[0].ID)

	fx.client.PerTarget = map[string]providerclienttest.TargetResponse{
		string(protocols.ProtocolOpenAI) + "|https://broken.example.com": {
			Err: errors.New("client refused the call"),
		},
	}

	fx.setRecoveryRows(t, "true", "0")
	loop := keyrecovery.NewLoop(keyrecovery.Config{
		DB: fx.db, Settings: fx.settings, Providers: fx.prov,
		Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
	})
	stop := loop.Start(context.Background())
	defer stop()

	waitFor(t, 3*time.Second, "the healthy provider's key to recover", func() bool {
		return keyRow(t, fx.db, healthy.Keys[0].ID).VerificationStatus == model.VerificationStatusPassed
	})
	if row := keyRow(t, fx.db, broken.Keys[0].ID); row.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("broken provider's key verification = %d, want still Failed", row.VerificationStatus)
	}
}

// TestLoopNeverRecoversOnInconclusiveOutcomes pins the most dangerous
// direction: an inconclusive probe (the destination cannot judge the
// credential) must leave the key exactly where it was — still Failed, not
// back in rotation — while still having actually probed the upstream. The
// canned outcomes are the same shapes the provider-client tests use for
// unreachable, timeout, rate-limited, and verification-unsupported.
func TestLoopNeverRecoversOnInconclusiveOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome providerclient.TestOutcome
	}{
		{"unreachable", providerclient.TestUnreachable},
		{"timeout", providerclient.TestTimeout},
		{"rate limited", providerclient.TestRateLimited},
		{"verification unsupported", providerclient.TestVerificationUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			p := fx.seedProvider(t, "inconclusive")
			keyID := p.Keys[0].ID
			fx.demoteKey(t, p.ID, keyID)
			fx.client.Result = providerclient.TestResult{Outcome: tc.outcome, DurationMs: 5}

			rec := &listenerRecorder{}
			fx.prov.SetKeyRetestPassedListener(rec.onPassed)

			fx.setRecoveryRows(t, "true", "0")
			callsBefore := fx.client.Calls
			loop := keyrecovery.NewLoop(keyrecovery.Config{
				DB: fx.db, Settings: fx.settings, Providers: fx.prov,
				Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
			})
			stop := loop.Start(context.Background())
			// Waiting for the second sweep's probe rather than running a
			// fixed window: a loaded runner can take longer than any fixed
			// sleep to finish even one sweep, and the assertion needs proof
			// the loop KEPT probing, not that it managed to inside 120ms.
			waitFor(t, 3*time.Second, "a second sweep to probe the key again", func() bool {
				return fx.client.Calls-callsBefore >= 2
			})
			stop()

			if row := keyRow(t, fx.db, keyID); row.VerificationStatus != model.VerificationStatusFailed {
				t.Fatalf("verification status = %d, want still Failed — an inconclusive probe must not recover a key", row.VerificationStatus)
			}
			if ids := rec.keyIDs(); len(ids) != 0 {
				t.Fatalf("retest-passed listener fired %v — the key never re-entered rotation", ids)
			}
		})
	}
}

// TestLoopSurvivesSettingsReadFailures pins the resilience rule: once a
// snapshot is known, a settings source that starts erroring (with the
// fail-open last-known value, the real service's contract on a database
// outage) must not stop or kill the loop — the scan-point decision keeps
// running on that last-known snapshot. Mutation check: a loop that
// returns on a settings error stops sweeping, freezing the probe count —
// exactly what this test refuses to accept.
func TestLoopSurvivesSettingsReadFailures(t *testing.T) {
	fx := newFixture(t)
	p := fx.seedProvider(t, "resilient")
	fx.demoteKey(t, p.ID, p.Keys[0].ID)
	// Inconclusive outcome so the key stays in the scan set and every
	// sweep probes it — the probe count is the loop's heartbeat.
	fx.client.Result = providerclient.TestResult{Outcome: providerclient.TestUnreachable, DurationMs: 5}

	src := &stubSettings{}
	src.set(settings.KeyAutoRecoverySetting{Enabled: true, IntervalMinutes: 0})

	rec := &probeRecorder{}
	fx.client.SideEffect = func() { rec.record(fx.client.LastModel) }
	calls := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.events)
	}

	loop := keyrecovery.NewLoop(keyrecovery.Config{
		DB: fx.db, Settings: src, Providers: fx.prov,
		Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
	})
	stop := loop.Start(context.Background())
	defer stop()

	waitFor(t, 3*time.Second, "sweeps before the outage", func() bool { return calls() >= 2 })
	src.failWith(errors.New("settings store unavailable"))
	waitFor(t, 3*time.Second, "sweeps to continue through the outage", func() bool { return calls() >= 4 })
}

// TestLoopKeepsTickingWhenTheDatabaseDropsMidRun is the real-service
// variant of the resilience rule, with the database itself closed: the
// loop must neither panic nor wedge, and its beats must keep reaching the
// sweep (which then fails its scan query and logs it) — observable as the
// scan-failure warning appearing while the settings cache still serves
// the last-known snapshot.
func TestLoopKeepsTickingWhenTheDatabaseDropsMidRun(t *testing.T) {
	fx := newFixture(t)
	p := fx.seedProvider(t, "outage")
	fx.demoteKey(t, p.ID, p.Keys[0].ID)
	fx.client.Result = providerclient.TestResult{Outcome: providerclient.TestUnreachable, DurationMs: 5}
	fx.setRecoveryRows(t, "true", "0")

	rec := &probeRecorder{}
	fx.client.SideEffect = func() { rec.record(fx.client.LastModel) }
	probes := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.events)
	}

	logFile := filepath.Join(t.TempDir(), "loop.log")
	logger.Init(logger.Config{Level: "warn", Filename: logFile, Console: false})
	t.Cleanup(func() {
		_ = logger.Sync()
		logger.Init(logger.Config{Filename: os.DevNull})
	})

	loop := keyrecovery.NewLoop(keyrecovery.Config{
		DB: fx.db, Settings: fx.settings, Providers: fx.prov,
		Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
	})
	stop := loop.Start(context.Background())
	defer stop()

	waitFor(t, 3*time.Second, "a sweep before the database drops", func() bool {
		return probes() >= 1
	})

	testutil.CloseDB(t, fx.db)
	waitFor(t, 3*time.Second, "the scan-failure warning after the database dropped", func() bool {
		_ = logger.Sync()
		b, err := os.ReadFile(logFile)
		return err == nil && strings.Contains(string(b), "key auto-recovery scan failed")
	})
}

// TestManualRetestRacingTheAutoProbeKeepsTheNewerGeneration pins the
// concurrency contract: when a manual retest of the same key runs while
// the loop's probe is in flight, the manual retest (which claims the
// newer test_generation) commits first, and the loop's later commit is
// discarded by the generation CAS — the final state is the newer
// generation's verdict, not whichever wrote last. The interleave is
// forced through the canned client's side effect: the auto probe's
// network call runs the manual retest synchronously mid-flight.
func TestManualRetestRacingTheAutoProbeKeepsTheNewerGeneration(t *testing.T) {
	fx := newFixture(t)
	p := fx.seedProvider(t, "racy")
	keyID := p.Keys[0].ID
	fx.demoteKey(t, p.ID, keyID)

	rec := &listenerRecorder{}
	fx.prov.SetKeyRetestPassedListener(rec.onPassed)

	order := &probeRecorder{}
	armed := false
	fx.client.SideEffect = func() {
		order.record(fx.client.LastModel)
		if !armed {
			return
		}
		armed = false
		// The manual retest, issued mid-flight, must see a DIFFERENT
		// verdict than the auto probe already read (the auto probe's
		// canned answer was captured before this side effect started):
		// it gets a decisive failure and commits Failed on generation
		// N+1 while the auto probe holds generation N.
		fx.client.Result = providerclient.TestResult{Outcome: providerclient.TestAuthFailed, DurationMs: 5}
		if _, err := fx.prov.TestProviderKey(context.Background(), p.ID, keyID, time.Now().UTC()); err != nil {
			t.Errorf("in-flight manual retest failed: %v", err)
		}
	}

	src := &stubSettings{}
	src.set(settings.KeyAutoRecoverySetting{Enabled: true, IntervalMinutes: 0})
	armed = true

	loop := keyrecovery.NewLoop(keyrecovery.Config{
		DB: fx.db, Settings: src, Providers: fx.prov,
		Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
	})
	stop := loop.Start(context.Background())
	defer stop()

	// The interleave itself is the thing to wait for: the auto probe's
	// record, then the manual retest's record inside its side effect.
	// (Waiting on the row's last_test_result would pass immediately —
	// the demoting setup retest already wrote AuthFailed.)
	waitFor(t, 3*time.Second, "the racing retests to settle", func() bool {
		return len(order.models()) >= 2
	})
	// Let the losing side's commit attempt land before reading the final
	// state.
	time.Sleep(20 * time.Millisecond)

	row := keyRow(t, fx.db, keyID)
	if row.VerificationStatus != model.VerificationStatusFailed {
		t.Fatalf("verification = %d, want Failed — the newer generation's verdict must stand", row.VerificationStatus)
	}
	if row.LastTestResult == nil || *row.LastTestResult != int(providerclient.TestAuthFailed) {
		t.Fatalf("last_test_result = %v, want the manual retest's decisive failure", row.LastTestResult)
	}
	if ids := rec.keyIDs(); len(ids) != 0 {
		t.Fatalf("retest-passed listener fired %v — neither retest proved the key", ids)
	}
	// Both probes ran: the auto probe entered first, the manual retest
	// second, inside the same side-effect chain.
	if models := order.models(); len(models) < 2 {
		t.Fatalf("probe order = %v, want the auto probe and the manual retest both recorded", models)
	}
}

// TestRecoveryLogsOneInfoLineNamingTheKeyAndProvider pins the operator's
// audit trail: an auto-recovery emits exactly one info line carrying the
// key and provider identity — the answer to "how did this key fix
// itself".
func TestRecoveryLogsOneInfoLineNamingTheKeyAndProvider(t *testing.T) {
	out := captureLogs(t, "info", func() {
		fx := newFixture(t)
		p := fx.seedProvider(t, "logged")
		keyID := p.Keys[0].ID
		fx.demoteKey(t, p.ID, keyID)
		fx.setRecoveryRows(t, "true", "0")

		loop := keyrecovery.NewLoop(keyrecovery.Config{
			DB: fx.db, Settings: fx.settings, Providers: fx.prov,
			Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
		})
		stop := loop.Start(context.Background())
		defer stop()

		waitFor(t, 3*time.Second, "the key to recover", func() bool {
			return keyRow(t, fx.db, keyID).VerificationStatus == model.VerificationStatusPassed
		})
		time.Sleep(50 * time.Millisecond) // let any duplicate line land
	})

	if n := strings.Count(out, "provider key auto-recovered"); n != 1 {
		t.Fatalf("recovery info lines = %d, want exactly 1; log said: %s", n, out)
	}
	for _, needle := range []string{"k-logged", "logged"} {
		if !strings.Contains(out, needle) {
			t.Errorf("recovery line does not name %q; log said: %s", needle, out)
		}
	}
}

// TestPersistentFailuresDoNotSpamTheLog is the negative counterpart: a
// key that keeps failing its probes across many sweeps produces no
// per-probe line at info level — the de-noising rule. The row's own
// last_test_* columns carry the outcome; the log carries recoveries.
func TestPersistentFailuresDoNotSpamTheLog(t *testing.T) {
	out := captureLogs(t, "info", func() {
		fx := newFixture(t)
		p := fx.seedProvider(t, "silent")
		keyID := p.Keys[0].ID
		fx.demoteKey(t, p.ID, keyID)
		fx.client.Result = providerclient.TestResult{Outcome: providerclient.TestUnreachable, DurationMs: 5}
		fx.setRecoveryRows(t, "true", "0")

		callsBefore := fx.client.Calls
		loop := keyrecovery.NewLoop(keyrecovery.Config{
			DB: fx.db, Settings: fx.settings, Providers: fx.prov,
			Heartbeat: 2 * time.Millisecond, ProbeGap: 0,
		})
		stop := loop.Start(context.Background())
		// Same settle-by-event discipline: several sweeps must actually
		// have run before the log can be judged for de-noising — a fixed
		// window under-runs on a loaded runner and proves nothing.
		waitFor(t, 3*time.Second, "several sweeps to run", func() bool {
			return fx.client.Calls-callsBefore >= 3
		})
		stop()
	})

	if strings.Contains(out, "k-silent") || strings.Contains(out, "silent") {
		t.Fatalf("persistently failing key produced log lines at info level; log said: %s", out)
	}
	if strings.Contains(out, "auto-recovery probe errored") {
		t.Fatalf("an inconclusive outcome was logged as an error; log said: %s", out)
	}
}
