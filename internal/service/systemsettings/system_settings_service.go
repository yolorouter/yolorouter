package systemsettings

import (
	"context"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/pkg/errcode"

	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

// MaxCustomSystemPromptLen bounds the prompt text by utf8 rune count. Enforced
// in the service layer (not the DDL) so PG's VARCHAR(2000) and SQLite's TEXT
// behave identically.
const MaxCustomSystemPromptLen = 2000

// refreshTimeout caps how long a cache refresh may block the request path.
const refreshTimeout = 500 * time.Millisecond

// cacheTTL is how long a successfully refreshed snapshot is served without
// re-querying the database.
const cacheTTL = 30 * time.Second

// refreshFailureTTL is the negative-TTL window applied after a refresh
// failure. During this window the read path serves last-known-good (or
// zero/disabled on cold start) without re-querying the DB, preventing a
// refresh storm and duplicate warning logs when the DB is down.
const refreshFailureTTL = 5 * time.Second

// Cache keys — the singleflight group and the entries map are both keyed by
// these strings, so each setting gets its own collapse lane and cache slot.
const (
	cspCacheKey              = "csp"
	inputCompressionCacheKey = "input_compression"
	visionFallbackCacheKey   = "vision_fallback"
)

// settingEntry holds the cached state for one setting key. The cache is shared
// across all settings (CSP today, input compression switch added on top) so
// every setting inherits the same five invariants hardened for CSP:
//
//  1. On refresh SUCCESS the cache returns its CURRENT snapshot (read under
//     lock) — NOT the value the reader just returned. This defeats the race
//     where a concurrent Update committed + published a newer version between
//     our DB read and our publishIfNewer; publishIfNewer rejects ours and we
//     must hand singleflight waiters the newer snapshot the cache now holds.
//  2. publishIfNewer is monotonic (reject ver < current) AND clears the
//     failure window on any successful DB read regardless of monotonicity.
//  3. negative-TTL: a failed refresh sets refreshFailureUntil; reads in that
//     window serve last-known-good (or the zero value on cold start) without
//     re-querying the DB.
//  4. The refresh query runs under
//     context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout) so one
//     client disconnect can't abort the shared singleflight refresh other
//     waiters depend on. Handler authoritative reads bind to the request ctx.
//  5. singleflight collapses concurrent refreshes per setting (keyed by name).
type settingEntry struct {
	snapshot            any // typed by the caller: settings.CustomSystemPromptSetting or bool
	version             int64
	hasSnapshot         bool
	refreshExpiry       time.Time
	refreshFailureUntil time.Time
}

// cacheSnapshot is the refresh result carried through singleflight. It carries
// the cache's current (post-publish) value+version, not the reader's return.
type cacheSnapshot struct {
	value   any
	version int64
}

// settingReader is the authoritative DB read backing one cache entry. The
// refresh path binds it to a detached context with refreshTimeout; the
// implementation must not bind to the caller's request ctx on its own.
type settingReader func(ctx context.Context) (value any, version int64, err error)

// SystemSettingsService caches the global settings the gateway reads on the
// hot path (custom system prompt, input-compression switch) and owns their
// read/write contract. It implements gateway.SettingsProvider (read path) and
// serves the handler GET/PUT. Every setting here is best-effort behavior
// guidance, NOT a security boundary — on refresh failure the read path fails
// OPEN (returns last-known-good or the zero value), never blocks the request.
type SystemSettingsService struct {
	db *gorm.DB

	mu           sync.RWMutex
	entries      map[string]*settingEntry
	refreshGroup singleflight.Group
}

// NewSystemSettingsService constructs the service. Both caches prime lazily on
// the first read.
func NewSystemSettingsService(db *gorm.DB) *SystemSettingsService {
	return &SystemSettingsService{
		db: db,
		entries: map[string]*settingEntry{
			cspCacheKey:              {},
			inputCompressionCacheKey: {},
			visionFallbackCacheKey:   {},
		},
	}
}

// cspEntry returns the CSP cache slot. The entries map is populated once in
// the constructor, so the lookup never misses; the helper exists so callers
// (and tests) don't sprinkle the string literal across the file.
func (s *SystemSettingsService) cspEntry() *settingEntry { return s.entries[cspCacheKey] }

// inputCompressionEntry returns the input-compression cache slot.
func (s *SystemSettingsService) inputCompressionEntry() *settingEntry {
	return s.entries[inputCompressionCacheKey]
}

// visionFallbackEntry returns the vision-fallback cache slot.
func (s *SystemSettingsService) visionFallbackEntry() *settingEntry {
	return s.entries[visionFallbackCacheKey]
}

// readCached is the shared hot-path read for any registered setting. It serves
// fresh cache, else serves last-known-good during a failure window (cold
// start only), else singleflights one refresh and returns the cache's current
// snapshot. failOpenZero is the value returned on cold-start failure (CSP:
// the zero CustomSystemPromptSetting; input compression: false).
func (s *SystemSettingsService) readCached(
	ctx context.Context,
	key string,
	entry *settingEntry,
	reader settingReader,
	failOpenZero any,
) (any, int64, error) {
	if v, ver, ok := s.cachedEntry(entry); ok {
		return v, ver, nil
	}
	// Cold-cache negative TTL: if a recent refresh failed and we have no
	// snapshot yet, serve the zero value silently instead of re-querying the
	// DB on every call. (When a snapshot exists, cachedEntry already served it
	// through the failure window above.)
	if s.inFailureWindow(entry) {
		return failOpenZero, 0, nil
	}
	// singleflight collapses concurrent refreshes into one DB query per key.
	v, err, _ := s.refreshGroup.Do(key, func() (interface{}, error) {
		// Re-check freshness inside the group. singleflight only collapses
		// Do calls that overlap in time: a goroutine that saw a cold cache,
		// was descheduled while another caller's refresh ran to completion,
		// and reached Do afterwards leads a whole second refresh — correct
		// result, one redundant query. Leadership is handed to a straggler
		// only after the previous leader's closure (including its publish)
		// fully returned, so this check reliably sees that publish.
		if v, ver, ok := s.cachedEntry(entry); ok {
			return cacheSnapshot{value: v, version: ver}, nil
		}
		return s.refreshEntry(ctx, entry, reader)
	})
	if err != nil {
		// fail-open: serve last-known-good (or the zero value on cold cache) + error
		s.mu.RLock()
		defer s.mu.RUnlock()
		if entry.hasSnapshot {
			return entry.snapshot, entry.version, err
		}
		return failOpenZero, 0, err
	}
	snap := v.(cacheSnapshot)
	return snap.value, snap.version, nil
}

// cachedEntry returns the cached snapshot when it is still considered fresh.
// A snapshot is fresh when refreshExpiry hasn't passed, OR when we're inside a
// failure window (negative TTL — keep serving last-known-good instead of
// re-querying the DB while it's down).
func (s *SystemSettingsService) cachedEntry(entry *settingEntry) (any, int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !entry.hasSnapshot {
		return nil, 0, false
	}
	now := time.Now()
	if now.Before(entry.refreshExpiry) {
		return entry.snapshot, entry.version, true
	}
	if now.Before(entry.refreshFailureUntil) {
		return entry.snapshot, entry.version, true
	}
	return nil, 0, false
}

// inFailureWindow reports whether a recent refresh failed and the negative-TTL
// cooldown is still active.
func (s *SystemSettingsService) inFailureWindow(entry *settingEntry) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Now().Before(entry.refreshFailureUntil)
}

// refreshEntry runs the authoritative DB read under a detached context with
// refreshTimeout, then publishes (monotonically) and returns the cache's
// CURRENT snapshot — NOT the value just read. See settingEntry invariant 1.
func (s *SystemSettingsService) refreshEntry(ctx context.Context, entry *settingEntry, reader settingReader) (cacheSnapshot, error) {
	// WithoutCancel detaches the caller's cancellation signal so a single
	// client disconnect does NOT abort the shared singleflight refresh other
	// waiters depend on. Only refreshTimeout bounds the query.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	val, ver, err := reader(rctx)
	if err != nil {
		// Negative TTL: record the failure so subsequent reads serve
		// last-known-good (or zero) without re-querying the DB.
		s.mu.Lock()
		entry.refreshFailureUntil = time.Now().Add(refreshFailureTTL)
		s.mu.Unlock()
		return cacheSnapshot{}, err
	}
	s.publishIfNewer(entry, val, ver)
	// Return the cache's current snapshot, not the value we just read: if a
	// concurrent Update committed and published a newer version between our DB
	// read and here, publishIfNewer rejected ours and the cache holds the
	// newer one. Returning the stale read to singleflight waiters would serve
	// a superseded value despite a completed update.
	s.mu.RLock()
	cur := cacheSnapshot{value: entry.snapshot, version: entry.version}
	s.mu.RUnlock()
	return cur, nil
}

// publishIfNewer writes the cache only when ver >= current version (monotonic).
// This defeats the "Update A committed N+1, paused; Update B published N+2; A
// then published N+1" rollback. A successful DB read also clears the failure
// window regardless of the monotonicity check below.
func (s *SystemSettingsService) publishIfNewer(entry *settingEntry, val any, ver int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A successful DB read means the DB is healthy — clear the failure
	// window regardless of the version-monotonicity check below.
	entry.refreshFailureUntil = time.Time{}
	if entry.hasSnapshot && ver < entry.version {
		return
	}
	entry.snapshot = val
	entry.version = ver
	entry.hasSnapshot = true
	entry.refreshExpiry = time.Now().Add(cacheTTL)
}

// --- Custom system prompt ---------------------------------------------------

// CustomSystemPrompt returns the cached snapshot (non-blocking). On cold cache
// or stale snapshot it triggers a singleflight refresh with a strict short
// timeout; on failure it returns last-known-good + error (fail-open).
func (s *SystemSettingsService) CustomSystemPrompt(ctx context.Context) (settings.CustomSystemPromptSetting, int64, error) {
	v, ver, err := s.readCached(ctx, cspCacheKey, s.cspEntry(),
		func(ctx context.Context) (any, int64, error) {
			setting, v, e := repository.GetCustomSystemPrompt(s.db.WithContext(ctx))
			return setting, v, e
		},
		settings.CustomSystemPromptSetting{})
	if err != nil {
		return v.(settings.CustomSystemPromptSetting), ver, err
	}
	return v.(settings.CustomSystemPromptSetting), ver, nil
}

// GetCustomSystemPrompt is the authoritative read for the handler GET: it
// bypasses the cache and reads straight from the DB so the admin always sees
// authoritative state. The query is bound to the request ctx so a client
// disconnect/timeout cancels the in-flight DB call rather than running to
// completion against a dead request.
func (s *SystemSettingsService) GetCustomSystemPrompt(ctx context.Context) (settings.CustomSystemPromptSetting, int64, error) {
	return repository.GetCustomSystemPrompt(s.db.WithContext(ctx))
}

// UpdateCustomSystemPrompt validates, CAS-upserts both rows in one tx, then
// atomically publishes the committed snapshot. Returns the new snapshot +
// version so the PUT response can hand the fresh version to the caller.
func (s *SystemSettingsService) UpdateCustomSystemPrompt(ctx context.Context, expectedVersion int64, enabled bool, text string) (settings.CustomSystemPromptSetting, int64, error) {
	if utf8.RuneCountInString(text) > MaxCustomSystemPromptLen {
		return settings.CustomSystemPromptSetting{}, 0, errcode.ErrCustomSystemPromptTooLong
	}
	if enabled && text == "" {
		return settings.CustomSystemPromptSetting{}, 0, errcode.ErrCustomSystemPromptEmpty
	}
	// Bind the CAS write to the request ctx so a client disconnect/timeout
	// aborts the transaction mid-flight rather than committing after the caller
	// is gone. The cache publish below is in-process and stays unbounded.
	setting, ver, err := repository.UpdateCustomSystemPrompt(s.db.WithContext(ctx), expectedVersion, enabled, text)
	if err != nil {
		return settings.CustomSystemPromptSetting{}, 0, err
	}
	s.publishIfNewer(s.cspEntry(), setting, ver)
	return setting, ver, nil
}

// --- Input compression switch -----------------------------------------------

// GetInputCompression returns the cached switch (non-blocking). On cold cache
// or stale snapshot it triggers a singleflight refresh with a strict short
// timeout; on failure it returns last-known-good + error (fail-open). The
// gateway hot path uses this so the DB is never queried per request.
func (s *SystemSettingsService) GetInputCompression(ctx context.Context) (bool, int64, error) {
	v, ver, err := s.readCached(ctx, inputCompressionCacheKey, s.inputCompressionEntry(),
		func(ctx context.Context) (any, int64, error) {
			enabled, v, e := repository.GetInputCompression(s.db.WithContext(ctx))
			return enabled, v, e
		},
		false)
	return v.(bool), ver, err
}

// GetInputCompressionForHandler is the authoritative read for the handler GET:
// it bypasses the cache and reads straight from the DB so the admin always
// sees authoritative state. Bound to the request ctx so a client disconnect
// cancels the in-flight DB call.
func (s *SystemSettingsService) GetInputCompressionForHandler(ctx context.Context) (bool, int64, error) {
	return repository.GetInputCompression(s.db.WithContext(ctx))
}

// UpdateInputCompression CAS-updates the single row, then atomically publishes
// the committed value so subsequent gateway reads see it without an invalidate
// round-trip. Returns the new version so the PUT response can hand the fresh
// version to the caller.
func (s *SystemSettingsService) UpdateInputCompression(ctx context.Context, expectedVersion int64, enabled bool) (bool, int64, error) {
	got, ver, err := repository.UpdateInputCompression(s.db.WithContext(ctx), expectedVersion, enabled)
	if err != nil {
		return false, 0, err
	}
	s.publishIfNewer(s.inputCompressionEntry(), got, ver)
	return got, ver, nil
}

// GetVisionFallback returns the cached vision-fallback snapshot
// (non-blocking, fail-open to the disabled zero value) — the gateway hot path
// read. An empty Model means the feature is off.
func (s *SystemSettingsService) GetVisionFallback(ctx context.Context) (settings.VisionFallbackSetting, int64, error) {
	v, ver, err := s.readCached(ctx, visionFallbackCacheKey, s.visionFallbackEntry(),
		func(ctx context.Context) (any, int64, error) {
			snap, v, e := repository.GetVisionFallback(s.db.WithContext(ctx))
			return snap, v, e
		},
		settings.VisionFallbackSetting{})
	return v.(settings.VisionFallbackSetting), ver, err
}

// GetVisionFallbackForHandler is the authoritative read for the handler GET:
// straight from the DB, bound to the request ctx.
func (s *SystemSettingsService) GetVisionFallbackForHandler(ctx context.Context) (settings.VisionFallbackSetting, int64, error) {
	return repository.GetVisionFallback(s.db.WithContext(ctx))
}

// UpdateVisionFallback validates that a non-empty model name refers to a
// model this gateway actually has (a typo would silently disable the feature
// at describe time, the worst place to find out), CAS-updates the pair, and
// publishes the committed snapshot to the cache.
func (s *SystemSettingsService) UpdateVisionFallback(ctx context.Context, expectedVersion int64, modelName, prompt string) (settings.VisionFallbackSetting, int64, error) {
	if modelName != "" {
		var n int64
		if err := s.db.WithContext(ctx).Table("models").Where("name = ?", modelName).Count(&n).Error; err != nil {
			return settings.VisionFallbackSetting{}, 0, err
		}
		if n == 0 {
			return settings.VisionFallbackSetting{}, 0, errcode.ErrVisionFallbackModelUnknown
		}
	}
	got, ver, err := repository.UpdateVisionFallback(s.db.WithContext(ctx), expectedVersion, modelName, prompt)
	if err != nil {
		return settings.VisionFallbackSetting{}, 0, err
	}
	s.publishIfNewer(s.visionFallbackEntry(), got, ver)
	return got, ver, nil
}
