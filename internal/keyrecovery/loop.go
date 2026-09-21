// Package keyrecovery runs the background loop that periodically retests
// provider keys the system itself kicked out of rotation (management
// switch still on, verification status failed) and lets the existing
// single-key retest path restore them — flip verification back to passed,
// re-enter rotation, and release the gateway's in-memory demotion — with
// no admin pressing the button.
//
// The loop owns ONLY the "when to probe" scheduling. "How to probe" is
// entirely the existing provider.TestProviderKey chain (every routable
// destination, media fallbacks, test_generation CAS, committed
// last_test_* columns); nothing here re-implements any of it.
package keyrecovery

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// DefaultHeartbeat is the production wake-up cadence. Each beat re-reads
// the setting snapshot (served from the settings service's 30s cache, so
// the database is queried at most once per TTL window) and decides
// whether the scan point has been reached. A constructor parameter rather
// than a constant so tests can inject a millisecond-level beat, the same
// seam the price-catalog refresh loop uses.
const DefaultHeartbeat = 30 * time.Second

// DefaultProbeGap is the pause inserted between two consecutive key
// probes inside one sweep, so a pool of failed keys never fires a burst
// of simultaneous upstream calls. Serial probing with a small gap keeps
// the request rate flat at any pool size.
const DefaultProbeGap = time.Second

// SettingsSource is the loop's narrow view of the settings service: one
// cached, fail-open read of the key-auto-recovery snapshot per beat.
// *systemsettings.SystemSettingsService implements it; the interface
// exists so tests can drive the snapshot (interval changes, read
// failures) at millisecond scale instead of waiting out the 30s cache
// TTL the real service carries.
type SettingsSource interface {
	GetKeyAutoRecovery(ctx context.Context) (settings.KeyAutoRecoverySetting, int64, error)
}

// Config carries the loop's dependencies and knobs. Heartbeat and ProbeGap
// default when zero (or negative); DB, Settings, and Providers are
// required.
type Config struct {
	DB *gorm.DB
	// Settings supplies the per-beat snapshot. The real settings service
	// fails open: on a database outage it returns the last-known-good (or
	// shipped-default) snapshot alongside the error, so the loop keeps
	// running on the last known configuration.
	Settings SettingsSource
	// Providers is the SAME provider service instance the router serves
	// admin traffic with — the one whose retest-passed listener releases
	// the gateway key pool's rate-limit bench. A loop probing through any
	// other instance would flip the database row but never fire that
	// listener, leaving the gateway's in-memory demotion in place.
	Providers *provider.ProviderService

	Heartbeat time.Duration
	ProbeGap  time.Duration
}

// Loop is the single-background-goroutine probe loop. Construct with New,
// start with Start; it holds no goroutine until started and cannot be
// started twice.
type Loop struct {
	db        *gorm.DB
	settings  SettingsSource
	providers *provider.ProviderService
	heartbeat time.Duration
	probeGap  time.Duration

	// lastSweep anchors the debounce arithmetic: the next scan point is
	// lastSweep + the CURRENT interval, recomputed on every beat from the
	// freshly read snapshot, so an interval change applies to the very
	// next decision without a restart. Set to the start instant so a
	// freshly booted instance waits one full interval before its first
	// sweep (startup anti-stampede). Only the loop goroutine touches it
	// after Start.
	lastSweep time.Time
}

// NewLoop builds the loop. It performs no I/O; everything happens once
// Start launches the goroutine.
func NewLoop(cfg Config) *Loop {
	heartbeat := cfg.Heartbeat
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeat
	}
	probeGap := cfg.ProbeGap
	if probeGap < 0 {
		probeGap = 0
	}
	return &Loop{
		db:        cfg.DB,
		settings:  cfg.Settings,
		providers: cfg.Providers,
		heartbeat: heartbeat,
		probeGap:  probeGap,
	}
}

// Start launches the loop's goroutine and returns a stop function that
// cancels it and waits for the goroutine to exit. Safe to call the stop
// function any number of times; the loop dies with the passed context as
// well. The first sweep happens no earlier than one full interval after
// start.
func (l *Loop) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(l.heartbeat)
		defer ticker.Stop()

		// The startup anchor: without this, a beat landing at start+ε
		// would immediately see "interval elapsed" (lastSweep zero time
		// is infinitely old) and sweep before the instance has finished
		// settling. Anchoring to now makes the first sweep wait one full
		// interval, deliberately not warming immediately the way the
		// price-catalog loop does — a probe round costs upstream quota,
		// unlike a catalog fetch.
		l.lastSweep = time.Now()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.tick(ctx)
			}
		}
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(cancel)
		<-done
	}
}

// tick is one heartbeat: read the setting snapshot, then decide whether
// this beat is the scan point. Never panics and never returns an abort
// signal — a broken settings read degrades to the last known snapshot so
// a database hiccup cannot kill the loop.
func (l *Loop) tick(ctx context.Context) {
	// The settings source is expected to fail open: on a read error the
	// real service returns the last-known-good (or shipped-default)
	// snapshot alongside the error, so the snapshot that comes back is
	// adopted whether or not the read errored. The loop keeps beating
	// either way — a settings outage must not stop the schedule, only
	// freeze it at what was last known.
	snap, _, _ := l.settings.GetKeyAutoRecovery(ctx)

	if !snap.Enabled {
		return
	}
	now := time.Now()
	if now.Before(l.lastSweep.Add(time.Duration(snap.IntervalMinutes) * time.Minute)) {
		return
	}
	// Stamped BEFORE the sweep so a long-running round does not re-arm
	// itself mid-flight on the next beat.
	l.lastSweep = now
	l.sweep(ctx)
}

// sweep runs one probe round: load the scan set, filter it in memory,
// then retest the survivors serially with a fixed gap between probes.
// A scan-query failure ends the round (the next interval retries); a
// single key's probe error is logged and the round moves on to the next
// key.
func (l *Loop) sweep(ctx context.Context) {
	candidates, err := repository.ListAutoRecoveryCandidates(l.db)
	if err != nil {
		logger.Warn("key auto-recovery scan failed",
			zap.Error(err))
		return
	}

	probed := false
	for _, c := range candidates {
		// In-memory filters, per the auto-recovery rules:
		// - needs re-entry: the authorized destination version trails
		//   the provider's current one; the stored plaintext cannot be
		//   tested against the new destination at all (TestProviderKey
		//   would reject it too — skipping here avoids the doomed call
		//   and its error log every round).
		// - provider management-disabled: the whole provider is off; a
		//   key under it must not spend upstream calls.
		if c.AuthorizedDestinationVersion != c.ProviderDestinationVersion {
			continue
		}
		if c.ProviderManagementStatus != model.ProviderStatusEnabled {
			continue
		}

		if probed {
			select {
			case <-ctx.Done():
				return
			case <-time.After(l.probeGap):
			}
		}
		probed = true

		view, err := l.providers.TestProviderKey(ctx, c.ProviderID, c.ID, time.Now().UTC())
		switch {
		case err != nil:
			// One key's failure (lost CAS race, decrypt error, client
			// refusal) must not abort the round — log and continue with
			// the remaining keys.
			logger.Warn("key auto-recovery probe errored",
				zap.Uint("provider_id", c.ProviderID),
				zap.String("provider_name", c.ProviderName),
				zap.Uint("key_id", c.ID),
				zap.String("key_label", c.Label),
				zap.Error(err))
		case view != nil && view.VerificationStatus == model.VerificationStatusPassed:
			// The one record an operator needs: how a key came back by
			// itself. The retest-passed listener (gateway bench release)
			// has already fired inside TestProviderKey's commit path.
			logger.Info("provider key auto-recovered",
				zap.Uint("provider_id", c.ProviderID),
				zap.String("provider_name", c.ProviderName),
				zap.Uint("key_id", c.ID),
				zap.String("key_label", c.Label))
		default:
			// The probe ran but did not prove the key (inconclusive
			// outcome, or a decisive failure that keeps it failed).
			// Debug, not info: a pool of dead keys would otherwise emit
			// one line per key per round, forever, saying nothing new —
			// the row's own last_test_* columns already carry the
			// outcome.
			logger.Debug("key auto-recovery probe left key unrecovered",
				zap.Uint("provider_id", c.ProviderID),
				zap.String("provider_name", c.ProviderName),
				zap.Uint("key_id", c.ID),
				zap.String("key_label", c.Label))
		}
	}
}
