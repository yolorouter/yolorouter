// Package auditretention runs the background loop that enforces the
// admin-audit retention setting: every heartbeat it re-reads the retention
// snapshot and, when a positive window is configured, drives one bounded
// sweep of the deletion engine in internal/repository — expired
// admin_audit_logs rows leave oldest-first in capped rounds, so a backlog
// drains gradually over successive rounds without one unbounded transaction
// ever pinning the table the audit capture worker flushes into.
//
// The loop owns ONLY the schedule: when a round happens and which setting
// value it acts on. What "expired" means and how one round removes rows is
// entirely the sweep engine's; nothing here re-implements any of it. Same
// shape as the request-log retention loop in internal/retention — one loop
// per retention domain, because each domain has its own setting row, its
// own sweep engine, and its own failure surface.
package auditretention

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// DefaultHeartbeat is the production wake-up cadence. Each beat re-reads
// the retention snapshot (served from the settings service's cache, so the
// database is queried at most once per TTL window) and decides whether a
// sweep round is due. A constructor parameter rather than a constant so
// tests can inject a millisecond-level beat — the same seam the request-log
// retention loop uses.
const DefaultHeartbeat = 30 * time.Second

// SettingsSource is the loop's narrow view of the settings service: one
// fail-open read of the retention snapshot per beat.
// *systemsettings.SystemSettingsService implements it; the interface exists
// so tests can hand the loop the same instance their update path uses,
// making mid-run setting changes observable at test speed.
type SettingsSource interface {
	GetAdminAuditRetention(ctx context.Context) (settings.AdminAuditRetentionSetting, int64, error)
}

// Config carries the loop's dependencies and knobs. Heartbeat and BatchLimit
// default when zero (or negative); DB and Settings are required.
type Config struct {
	DB *gorm.DB
	// Settings Supplies the per-beat snapshot. The real settings service
	// fails open: on a database outage it returns the last-known-good (or
	// shipped 180-day) snapshot alongside the error, so the loop keeps
	// running on the last known configuration.
	Settings SettingsSource

	Heartbeat time.Duration
	// BatchLimit caps how many admin_audit_logs rows one round deletes; zero
	// or negative falls back to the engine's named default
	// (repository.DefaultAdminAuditRetentionBatchLimit). Exists so callers
	// and tests can throttle rounds without recompiling the engine.
	BatchLimit int
}

// Loop is the single-background-goroutine audit retention loop. Construct
// with NewLoop, start with Start; it holds no goroutine until started,
// cannot be started twice, and re-reads the setting on every beat — the
// retention value is deliberately never captured at construction or start
// time, so an admin's change binds on the next round without a restart.
type Loop struct {
	db         *gorm.DB
	settings   SettingsSource
	heartbeat  time.Duration
	batchLimit int
}

// NewLoop builds the loop. It performs no I/O and reads no setting;
// everything happens once Start launches the goroutine.
func NewLoop(cfg Config) *Loop {
	heartbeat := cfg.Heartbeat
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeat
	}
	return &Loop{
		db:         cfg.DB,
		settings:   cfg.Settings,
		heartbeat:  heartbeat,
		batchLimit: cfg.BatchLimit,
	}
}

// Start launches the loop's goroutine and returns a stop function that
// cancels it and waits for the goroutine to exit. Safe to call the stop
// function any number of times; the loop dies with the passed context as
// well. The first round happens no earlier than one full heartbeat after
// start (ticker semantics), same schedule as the request-log retention
// loop.
func (l *Loop) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(l.heartbeat)
		defer ticker.Stop()

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

// tick is one heartbeat: re-read the retention snapshot, then decide
// whether this beat runs a sweep round. Never panics and never returns an
// abort signal — a broken settings read degrades to the last known
// snapshot, and a failed round is retried by a later beat; neither may
// kill the loop.
func (l *Loop) tick(ctx context.Context) {
	// The settings source is expected to fail open: on a read error the
	// real service returns the last-known-good (or shipped default)
	// snapshot alongside the error, so the snapshot that comes back is
	// adopted whether or not the read errored. The read happens on EVERY
	// beat: zero means keep forever (skip the round entirely), and any
	// positive window means this beat runs one bounded round. A window
	// shrunk by the admin takes effect the same way — the next beat simply
	// acts on the smaller value, and the backlog it creates drains over as
	// many bounded rounds as it takes rather than one synchronous purge.
	snap, _, _ := l.settings.GetAdminAuditRetention(ctx)

	if snap.Days <= 0 {
		return
	}
	l.sweepRound(ctx, snap.Days)
}

// sweepRound runs one bounded deletion round through the sweep engine and
// reports the outcome. A failed round logs a warning and moves on — the
// next beat retries it; a round that removed rows logs the count (the
// operator's "where did my audit trail go" trace); an idle round stays at
// debug level — a converged loop beats forever, and one info line per beat
// saying "deleted 0" would be pure noise.
func (l *Loop) sweepRound(ctx context.Context, days int) {
	deleted, err := repository.SweepExpiredAdminAuditLogs(l.db.WithContext(ctx), time.Now(), days, l.batchLimit)
	if err != nil {
		logger.Warn("admin audit retention sweep failed", zap.Error(err))
		return
	}
	if deleted == 0 {
		logger.Debug("admin audit retention round found nothing to delete")
		return
	}
	logger.Info("admin audit retention round deleted expired audit rows",
		zap.Int("rows", deleted),
		zap.Int("retention_days", days))
}
