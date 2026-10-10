package systemsettings

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/pkg/errcode"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newSvcTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Pin the pool to a single connection so :memory: is shared across all
	// queries (otherwise modernc/sqlite gives each connection its own private
	// in-memory DB) and concurrent transactions serialize through it — this
	// is what makes the CAS conflict deterministic in the concurrent test.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('custom_system_prompt_enabled','false'),('custom_system_prompt','')`)
	return db
}

// newSvcTestDBWithIC returns a test DB with both the CSP rows and the
// input_compression_enabled row seeded at version 1, disabled.
func newSvcTestDBWithIC(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSvcTestDB(t)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('input_compression_enabled','false')`)
	return db
}

// newSvcTestDBWithKAR returns a test DB with the key-auto-recovery pair
// seeded at the migration default: enabled + 30 minutes, both rows at v1.
func newSvcTestDBWithKAR(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSvcTestDB(t)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('key_auto_recovery_enabled','true'),('key_auto_recovery_interval_minutes','30')`)
	return db
}

// --- CSP regression (behavior must be unchanged by the generic-cache refactor) ---

func TestSystemSettingsServiceReadReturnsSeededDisabled(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t))
	s, ver, err := svc.CustomSystemPrompt(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s.Enabled || s.Text != "" || ver != 1 {
		t.Fatalf("want disabled/empty/v1, got %+v v%d", s, ver)
	}
}

func TestSystemSettingsServiceUpdatePublishesImmediately(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t))
	if _, _, err := svc.UpdateCustomSystemPrompt(context.Background(), 1, true, "hi"); err != nil {
		t.Fatalf("update: %v", err)
	}
	// read path sees the new value without any invalidate
	s, _, err := svc.CustomSystemPrompt(context.Background())
	if err != nil || !s.Enabled || s.Text != "hi" {
		t.Fatalf("read after update mismatch: %+v err=%v", s, err)
	}
}

func TestSystemSettingsServiceUpdateConflict(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t))
	if _, _, err := svc.UpdateCustomSystemPrompt(context.Background(), 1, true, "a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, _, err := svc.UpdateCustomSystemPrompt(context.Background(), 1, false, "")
	if !errors.Is(err, errcode.ErrCustomSystemPromptConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestSystemSettingsServiceRejectsTooLong(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t))
	long := make([]rune, MaxCustomSystemPromptLen+1)
	for i := range long {
		long[i] = 'x'
	}
	_, _, err := svc.UpdateCustomSystemPrompt(context.Background(), 1, true, string(long))
	if !errors.Is(err, errcode.ErrCustomSystemPromptTooLong) {
		t.Fatalf("want too-long, got %v", err)
	}
}

func TestSystemSettingsServiceRejectsEnabledEmpty(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t))
	_, _, err := svc.UpdateCustomSystemPrompt(context.Background(), 1, true, "")
	if !errors.Is(err, errcode.ErrCustomSystemPromptEmpty) {
		t.Fatalf("want empty, got %v", err)
	}
}

// Two concurrent PUTs: the loser must see Conflict, and the cache must end on
// the winner's version (monotonic publish — a late-publishing loser can't
// roll the cache back to an older version).
func TestSystemSettingsServiceConcurrentPUTsMonotonic(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t))
	var wg sync.WaitGroup
	var conflicts atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.UpdateCustomSystemPrompt(context.Background(), 1, true, "race")
			if errors.Is(err, errcode.ErrCustomSystemPromptConflict) {
				conflicts.Add(1)
			}
		}()
	}
	wg.Wait()
	if conflicts.Load() != 1 {
		t.Fatalf("want exactly 1 conflict, got %d", conflicts.Load())
	}
	s, ver, err := svc.CustomSystemPrompt(context.Background())
	if err != nil || !s.Enabled || s.Text != "race" || ver != 2 {
		t.Fatalf("cache not at winner v2: %+v v%d err=%v", s, ver, err)
	}
}

// TestSystemSettingsServiceRefreshFailureDoesNotHammer verifies that after a
// refresh failure the negative-TTL window prevents repeated DB queries: the
// first stale read triggers one refresh (which fails and returns
// last-known-good + error), and subsequent reads within the failure window
// return last-known-good silently (nil error, no refresh).
func TestSystemSettingsServiceRefreshFailureDoesNotHammer(t *testing.T) {
	db := newSvcTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}
	svc := NewSystemSettingsService(db)

	// Warm the cache with a known value.
	if _, _, err := svc.UpdateCustomSystemPrompt(context.Background(), 1, true, "warm"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, _, err := svc.CustomSystemPrompt(context.Background())
	if err != nil || !s.Enabled || s.Text != "warm" {
		t.Fatalf("warm read: %+v err=%v", s, err)
	}

	// Force the cache stale so the next read triggers a refresh.
	csp := svc.cspEntry()
	svc.mu.Lock()
	csp.refreshExpiry = time.Now().Add(-time.Second)
	svc.mu.Unlock()

	// Break the DB so the refresh will fail.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// First call: triggers refresh, fails, sets failure window, returns
	// last-known-good + error.
	s1, _, err1 := svc.CustomSystemPrompt(context.Background())
	if err1 == nil {
		t.Fatalf("first stale read: want error, got nil")
	}
	if !s1.Enabled || s1.Text != "warm" {
		t.Fatalf("first stale read: want last-known-good warm, got %+v", s1)
	}

	// Verify failure window is active.
	svc.mu.RLock()
	failureUntil := csp.refreshFailureUntil
	svc.mu.RUnlock()
	if !time.Now().Before(failureUntil) {
		t.Fatalf("failure window not set or already expired: %v", failureUntil)
	}

	// Subsequent calls within the failure window: no refresh, no error,
	// return last-known-good. If these calls hit the DB they would error
	// (the DB is closed), so a nil error proves no DB query happened.
	for i := 0; i < 5; i++ {
		s2, _, err2 := svc.CustomSystemPrompt(context.Background())
		if err2 != nil {
			t.Fatalf("call %d: want nil error in failure window, got %v", i, err2)
		}
		if !s2.Enabled || s2.Text != "warm" {
			t.Fatalf("call %d: want last-known-good warm, got %+v", i, s2)
		}
	}
}

// TestSystemSettingsServiceRefreshFailureColdStartDoesNotHammer verifies the
// cold-start failure path: the first call triggers one refresh (fails, returns
// zero/disabled + error), and subsequent calls within the failure window return
// zero/disabled silently without re-querying the DB.
func TestSystemSettingsServiceRefreshFailureColdStartDoesNotHammer(t *testing.T) {
	db := newSvcTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}
	svc := NewSystemSettingsService(db)

	// Close the DB before the first read — cold start with a broken DB.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// First call: triggers refresh, fails, sets failure window, returns
	// zero/disabled + error.
	s1, _, err1 := svc.CustomSystemPrompt(context.Background())
	if err1 == nil {
		t.Fatalf("first cold read: want error, got nil")
	}
	if s1.Enabled || s1.Text != "" {
		t.Fatalf("first cold read: want zero/disabled, got %+v", s1)
	}

	// Verify failure window is active.
	csp := svc.cspEntry()
	svc.mu.RLock()
	failureUntil := csp.refreshFailureUntil
	svc.mu.RUnlock()
	if !time.Now().Before(failureUntil) {
		t.Fatalf("failure window not set or already expired: %v", failureUntil)
	}

	// Subsequent calls within the failure window: no refresh, no error,
	// zero/disabled. If these calls hit the DB they would error (the DB is
	// closed), so a nil error proves no DB query happened.
	for i := 0; i < 5; i++ {
		s2, _, err2 := svc.CustomSystemPrompt(context.Background())
		if err2 != nil {
			t.Fatalf("call %d: want nil error in failure window, got %v", i, err2)
		}
		if s2.Enabled || s2.Text != "" {
			t.Fatalf("call %d: want zero/disabled, got %+v", i, s2)
		}
	}
}

// --- Input compression cache tests ------------------------------------------

// TestInputCompressionReadReturnsSeededDisabled verifies the cold-cache read
// path primes from the seeded disabled row.
func TestInputCompressionReadReturnsSeededDisabled(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithIC(t))
	enabled, ver, err := svc.GetInputCompression(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if enabled || ver != 1 {
		t.Fatalf("want disabled/v1, got enabled=%v v%d", enabled, ver)
	}
}

// TestInputCompressionMissingRowReturnsDefault verifies that a not-yet-migrated
// database (row absent) reads as disabled without erroring — the gateway hot
// path must never fail-closed on this.
func TestInputCompressionMissingRowReturnsDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t)) // no IC row seeded
	enabled, ver, err := svc.GetInputCompression(context.Background())
	if err != nil {
		t.Fatalf("read on missing row: want nil err, got %v", err)
	}
	if enabled || ver != 0 {
		t.Fatalf("want disabled/v0, got enabled=%v v%d", enabled, ver)
	}
}

// TestInputCompressionUpdatePublishesImmediately verifies that a CAS update
// publishes the new value to the cache so the next read sees it without an
// invalidate round-trip.
func TestInputCompressionUpdatePublishesImmediately(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithIC(t))
	got, ver, err := svc.UpdateInputCompression(context.Background(), 1, true)
	if err != nil || !got || ver != 2 {
		t.Fatalf("update: got enabled=%v v%d err=%v", got, ver, err)
	}
	// Cached read sees the new value immediately.
	enabled, _, err := svc.GetInputCompression(context.Background())
	if err != nil || !enabled {
		t.Fatalf("read after update: want enabled=true, got %v err=%v", enabled, err)
	}
}

// TestInputCompressionUpdateConflict verifies the CAS conflict path: a second
// save with the stale version must return ErrInputCompressionConflict.
func TestInputCompressionUpdateConflict(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithIC(t))
	if _, _, err := svc.UpdateInputCompression(context.Background(), 1, true); err != nil {
		t.Fatalf("first update: %v", err)
	}
	_, _, err := svc.UpdateInputCompression(context.Background(), 1, false)
	if !errors.Is(err, errcode.ErrInputCompressionConflict) {
		t.Fatalf("want ErrInputCompressionConflict, got %v", err)
	}
}

// TestInputCompressionHandlerReadBypassesCache verifies that
// GetInputCompressionForHandler reads straight from the DB (ignoring the
// cache) so the admin always sees authoritative state.
func TestInputCompressionHandlerReadBypassesCache(t *testing.T) {
	db := newSvcTestDBWithIC(t)
	svc := NewSystemSettingsService(db)

	// Prime the cache with disabled.
	if enabled, _, err := svc.GetInputCompression(context.Background()); err != nil || enabled {
		t.Fatalf("prime read: enabled=%v err=%v", enabled, err)
	}

	// Mutate the DB OUT FROM UNDER the cache (no service call, so cache stays
	// stale at disabled/v1).
	if res := db.Exec(`UPDATE system_settings SET value='true', version=version+1 WHERE key='input_compression_enabled'`); res.Error != nil {
		t.Fatalf("raw update: %v", res.Error)
	}

	// Handler read must see the committed true/v2, not the cached disabled/v1.
	enabled, ver, err := svc.GetInputCompressionForHandler(context.Background())
	if err != nil || !enabled || ver != 2 {
		t.Fatalf("handler read: want enabled=true/v2, got enabled=%v v%d err=%v", enabled, ver, err)
	}
}

// TestInputCompressionConcurrentPUTsMonotonic verifies that two concurrent
// updates collapse to exactly one winner; the loser sees Conflict and the
// cache ends on the winner's value+version (monotonic publish).
func TestInputCompressionConcurrentPUTsMonotonic(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithIC(t))
	var wg sync.WaitGroup
	var conflicts atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.UpdateInputCompression(context.Background(), 1, true)
			if errors.Is(err, errcode.ErrInputCompressionConflict) {
				conflicts.Add(1)
			}
		}()
	}
	wg.Wait()
	if conflicts.Load() != 1 {
		t.Fatalf("want exactly 1 conflict, got %d", conflicts.Load())
	}
	enabled, ver, err := svc.GetInputCompression(context.Background())
	if err != nil || !enabled || ver != 2 {
		t.Fatalf("cache not at winner v2/true: enabled=%v v%d err=%v", enabled, ver, err)
	}
}

// TestInputCompressionRefreshFailureDoesNotHammer verifies the negative-TTL
// fail-open path on a warm cache: the first stale read triggers one refresh
// (fails, returns last-known-good + error), and subsequent reads within the
// failure window return last-known-good silently (nil error, no DB query).
func TestInputCompressionRefreshFailureDoesNotHammer(t *testing.T) {
	db := newSvcTestDBWithIC(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}
	svc := NewSystemSettingsService(db)

	// Warm the cache with enabled=true.
	if _, _, err := svc.UpdateInputCompression(context.Background(), 1, true); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if enabled, _, err := svc.GetInputCompression(context.Background()); err != nil || !enabled {
		t.Fatalf("warm read: enabled=%v err=%v", enabled, err)
	}

	// Force the cache stale so the next read triggers a refresh.
	ic := svc.inputCompressionEntry()
	svc.mu.Lock()
	ic.refreshExpiry = time.Now().Add(-time.Second)
	svc.mu.Unlock()

	// Break the DB so the refresh will fail.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// First call: triggers refresh, fails, sets failure window, returns
	// last-known-good + error.
	enabled1, _, err1 := svc.GetInputCompression(context.Background())
	if err1 == nil {
		t.Fatalf("first stale read: want error, got nil")
	}
	if !enabled1 {
		t.Fatalf("first stale read: want last-known-good true, got enabled=%v", enabled1)
	}

	// Verify failure window is active.
	svc.mu.RLock()
	failureUntil := ic.refreshFailureUntil
	svc.mu.RUnlock()
	if !time.Now().Before(failureUntil) {
		t.Fatalf("failure window not set or already expired: %v", failureUntil)
	}

	// Subsequent calls within the failure window: no refresh, no error,
	// return last-known-good. If these calls hit the DB they would error
	// (the DB is closed), so a nil error proves no DB query happened.
	for i := 0; i < 5; i++ {
		enabled2, _, err2 := svc.GetInputCompression(context.Background())
		if err2 != nil {
			t.Fatalf("call %d: want nil error in failure window, got %v", i, err2)
		}
		if !enabled2 {
			t.Fatalf("call %d: want last-known-good true, got enabled=%v", i, enabled2)
		}
	}
}

// TestInputCompressionRefreshFailureColdStartDoesNotHammer verifies the
// cold-start failure path: the first call triggers one refresh (fails, returns
// disabled + error), and subsequent calls within the failure window return
// disabled silently (nil error, no DB query) — fail-open disabled, never block.
func TestInputCompressionRefreshFailureColdStartDoesNotHammer(t *testing.T) {
	db := newSvcTestDBWithIC(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}
	svc := NewSystemSettingsService(db)

	// Close the DB before the first read — cold start with a broken DB.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// First call: triggers refresh, fails, sets failure window, returns
	// disabled + error.
	enabled1, _, err1 := svc.GetInputCompression(context.Background())
	if err1 == nil {
		t.Fatalf("first cold read: want error, got nil")
	}
	if enabled1 {
		t.Fatalf("first cold read: want disabled, got enabled=%v", enabled1)
	}

	// Verify failure window is active.
	ic := svc.inputCompressionEntry()
	svc.mu.RLock()
	failureUntil := ic.refreshFailureUntil
	svc.mu.RUnlock()
	if !time.Now().Before(failureUntil) {
		t.Fatalf("failure window not set or already expired: %v", failureUntil)
	}

	// Subsequent calls within the failure window: no refresh, no error,
	// disabled. If these calls hit the DB they would error (the DB is closed),
	// so a nil error proves no DB query happened.
	for i := 0; i < 5; i++ {
		enabled2, _, err2 := svc.GetInputCompression(context.Background())
		if err2 != nil {
			t.Fatalf("call %d: want nil error in failure window, got %v", i, err2)
		}
		if enabled2 {
			t.Fatalf("call %d: want disabled, got enabled=%v", i, enabled2)
		}
	}
}

// queryCountWitness is test-only scaffolding that counts SELECT statements
// issued against the system_settings table via a gorm query callback. It is
// used to verify the singleflight contract: N concurrent refresh waiters must
// collapse to exactly one DB read.
type queryCountWitness struct {
	hits atomic.Int64
}

// attachQueryCountWitness registers a Before("gorm:query") callback on db that
// increments the witness for every query whose target table is system_settings.
// The callback is scoped to this db handle (each test opens its own gorm.DB),
// so there is no cross-test interference.
func attachQueryCountWitness(t *testing.T, db *gorm.DB) *queryCountWitness {
	t.Helper()
	w := &queryCountWitness{}
	const cbName = "_test_count_system_settings_reads"
	if err := db.Callback().Query().Before("gorm:query").Register(cbName, func(tx *gorm.DB) {
		if tx.Statement == nil {
			return
		}
		// All settings reads use Table("system_settings"); Update path goes
		// through Update callbacks and does not fire Query callbacks.
		if tx.Statement.Table == "system_settings" {
			w.hits.Add(1)
		}
	}); err != nil {
		t.Fatalf("register query-count callback: %v", err)
	}
	return w
}

// TestInputCompressionSingleflightCollapse verifies that concurrent cold reads
// collapse into a SINGLE DB query against system_settings. The witness counts
// actual SELECT statements issued by the gorm layer; the singleflight
// contract is: N concurrent waiters => 1 leader refresh, 0 follower queries.
func TestInputCompressionSingleflightCollapse(t *testing.T) {
	db := newSvcTestDBWithIC(t)
	witness := attachQueryCountWitness(t, db)
	svc := NewSystemSettingsService(db)

	// Make the cache cold so all callers miss simultaneously and race into the
	// singleflight group.
	ic := svc.inputCompressionEntry()
	svc.mu.Lock()
	ic.hasSnapshot = false
	ic.refreshExpiry = time.Time{}
	ic.refreshFailureUntil = time.Time{}
	svc.mu.Unlock()

	// Spawn many concurrent readers; they should all settle on disabled/v1
	// (the seeded row) and never error.
	const N = 32
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			enabled, ver, err := svc.GetInputCompression(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if enabled || ver != 1 {
				errs <- fmt.Errorf("want disabled/v1, got enabled=%v v%d", enabled, ver)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}

	// Hard assertion: exactly ONE system_settings SELECT was issued. Anything
	// higher means concurrent refreshes were NOT collapsed by singleflight.
	if got := witness.hits.Load(); got != 1 {
		t.Fatalf("singleflight leak: want exactly 1 system_settings read, got %d", got)
	}
}

// TestInputCompressionRefreshReturnsCurrentSnapshotOnConcurrentPublish
// verifies the most subtle invariant of the cache: when a concurrent Update
// publishes a NEWER version to the cache BETWEEN the refresh reader's DB-read
// and the refresh's publishIfNewer, the refresh MUST return the cache's CURRENT
// snapshot (the newer one), NOT the value the reader just returned.
//
// Mechanism: publishIfNewer rejects the reader's older value (monotonic); the
// refresh path must then hand singleflight waiters the newer snapshot the cache
// now holds. Returning the reader's stale value would serve a superseded
// snapshot despite the update having landed.
//
// The reader used here is injected through readCached's reader parameter (the
// same private method GetInputCompression calls). The reader returns a FIXED
// older snapshot (false, v1), then pauses at a channel barrier so the main
// goroutine can publish (true, v2) to the cache before publishIfNewer runs.
// When the barrier releases, publishIfNewer rejects (false, v1) because the
// cache is already at v2; the assertion then checks that the refresh returned
// (true, v2) — distinguishable from the reader's (false, v1).
func TestInputCompressionRefreshReturnsCurrentSnapshotOnConcurrentPublish(t *testing.T) {
	db := newSvcTestDBWithIC(t)
	svc := NewSystemSettingsService(db)

	// Prime cache at v1 disabled.
	if enabled, _, err := svc.GetInputCompression(context.Background()); err != nil || enabled {
		t.Fatalf("prime read: enabled=%v err=%v", enabled, err)
	}

	// Force the cache stale so the next readCached triggers a refresh.
	ic := svc.inputCompressionEntry()
	svc.mu.Lock()
	ic.refreshExpiry = time.Now().Add(-time.Second)
	svc.mu.Unlock()

	// Channel barrier: the reader signals after it has produced its older
	// snapshot, then blocks until the main goroutine has published v2 to the
	// cache. This forces the exact ordering the invariant guards against.
	readDone := make(chan struct{})
	resumePublish := make(chan struct{})

	// reader returns an OLDER snapshot than what the cache will hold by the
	// time publishIfNewer runs. (false, v1) is distinguishable from the
	// (true, v2) a concurrent Update will publish.
	reader := func(ctx context.Context) (any, int64, error) {
		// Simulate reading a stale snapshot: DB was at v1 when we read it.
		readDone <- struct{}{}
		<-resumePublish
		return false, int64(1), nil
	}

	type refreshResult struct {
		value   any
		version int64
		err     error
	}
	resultCh := make(chan refreshResult, 1)

	// Drive the real refresh path (singleflight.Do -> refreshEntry -> reader ->
	// publishIfNewer -> re-read-under-RLock) by calling the private readCached
	// with our injected reader. This exercises the exact code path that
	// GetInputCompression uses.
	go func() {
		v, ver, err := svc.readCached(
			context.Background(),
			inputCompressionCacheKey,
			ic,
			reader,
			false,
		)
		resultCh <- refreshResult{v, ver, err}
	}()

	// Wait until the reader has produced its older (false, v1) snapshot and is
	// parked at the barrier — publishIfNewer has NOT run yet.
	<-readDone

	// Publish (true, v2) to the cache via the real Update path. This writes
	// v2 into the cache under s.mu; our paused reader's older v1 will be
	// rejected when its publishIfNewer runs.
	if _, ver, err := svc.UpdateInputCompression(context.Background(), 1, true); err != nil || ver != 2 {
		t.Fatalf("concurrent update v1->v2: ver=%d err=%v", ver, err)
	}

	// Release the barrier: the reader's publishIfNewer now runs against a cache
	// already at v2 > v1, so (false, v1) is REJECTED. refreshEntry must then
	// re-read the cache under RLock and return (true, v2) to singleflight.
	close(resumePublish)

	res := <-resultCh
	if res.err != nil {
		t.Fatalf("refresh returned error: %v", res.err)
	}
	got, ok := res.value.(bool)
	if !ok {
		t.Fatalf("refresh result value is not bool: %T(%v)", res.value, res.value)
	}
	// The distinguishing assertion: got == true / v2 proves the refresh handed
	// back the cache's CURRENT snapshot, not the reader's (false, v1). A broken
	// implementation that returned the reader's value would fail here with
	// enabled=false / v1.
	if !got || res.version != 2 {
		t.Fatalf("want cache current snapshot (enabled=true, v2); got (enabled=%v, v%d) — refresh leaked the reader's stale value", got, res.version)
	}
}

// --- Key auto recovery cache tests -------------------------------------------

// TestKeyAutoRecoveryReadReturnsSeededDefault verifies the cold-cache read
// path primes from the seeded enabled/30 rows.
func TestKeyAutoRecoveryReadReturnsSeededDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithKAR(t))
	s, ver, err := svc.GetKeyAutoRecovery(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !s.Enabled || s.IntervalMinutes != 30 || ver != 1 {
		t.Fatalf("want enabled/30/v1, got %+v v%d", s, ver)
	}
}

// TestKeyAutoRecoveryMissingRowsReturnDefault verifies that a not-yet-migrated
// database (pair absent) reads as the shipped default with version 0 — the
// fallback path only; the PUT handler still refuses version < 1, so the row
// can only ever be created by the seeding migration.
func TestKeyAutoRecoveryMissingRowsReturnDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t)) // no KAR rows seeded
	s, ver, err := svc.GetKeyAutoRecovery(context.Background())
	if err != nil {
		t.Fatalf("read on missing rows: want nil err, got %v", err)
	}
	if !s.Enabled || s.IntervalMinutes != 30 || ver != 0 {
		t.Fatalf("want enabled/30/v0 (shipped default), got %+v v%d", s, ver)
	}
}

// TestKeyAutoRecoveryUpdatePublishesImmediately verifies that a CAS update
// publishes the new snapshot to the cache so the next loop read sees it
// without an invalidate round-trip.
func TestKeyAutoRecoveryUpdatePublishesImmediately(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithKAR(t))
	got, ver, err := svc.UpdateKeyAutoRecovery(context.Background(), 1, false, 45)
	if err != nil || got.Enabled || got.IntervalMinutes != 45 || ver != 2 {
		t.Fatalf("update: got %+v v%d err=%v", got, ver, err)
	}
	// Cached read sees the new value immediately.
	s, _, err := svc.GetKeyAutoRecovery(context.Background())
	if err != nil || s.Enabled || s.IntervalMinutes != 45 {
		t.Fatalf("read after update: want disabled/45, got %+v err=%v", s, err)
	}
}

// TestKeyAutoRecoveryUpdateConflict verifies the CAS conflict path: a second
// save with the stale version must return ErrKeyAutoRecoveryConflict.
func TestKeyAutoRecoveryUpdateConflict(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithKAR(t))
	if _, _, err := svc.UpdateKeyAutoRecovery(context.Background(), 1, false, 45); err != nil {
		t.Fatalf("first update: %v", err)
	}
	_, _, err := svc.UpdateKeyAutoRecovery(context.Background(), 1, true, 30)
	if !errors.Is(err, errcode.ErrKeyAutoRecoveryConflict) {
		t.Fatalf("want ErrKeyAutoRecoveryConflict, got %v", err)
	}
}

// TestKeyAutoRecoveryRejectsIntervalOutOfBounds verifies the interval bounds
// (whole minutes, 1..1440) at every edge: the inclusive endpoints pass, and
// 0 / negative / 1441 are rejected with the setting's own validation error.
func TestKeyAutoRecoveryRejectsIntervalOutOfBounds(t *testing.T) {
	for _, bad := range []int{0, -1, 1441} {
		svc := NewSystemSettingsService(newSvcTestDBWithKAR(t))
		_, _, err := svc.UpdateKeyAutoRecovery(context.Background(), 1, true, bad)
		if !errors.Is(err, errcode.ErrKeyAutoRecoveryIntervalInvalid) {
			t.Fatalf("interval %d: want ErrKeyAutoRecoveryIntervalInvalid, got %v", bad, err)
		}
	}
	// The inclusive endpoints pass; each accepted update advances the
	// version, so the second save carries the first one's new version.
	svc := NewSystemSettingsService(newSvcTestDBWithKAR(t))
	if _, ver, err := svc.UpdateKeyAutoRecovery(context.Background(), 1, true, 1); err != nil || ver != 2 {
		t.Fatalf("interval 1: want accepted at v2, got ver=%d err=%v", ver, err)
	}
	if _, ver, err := svc.UpdateKeyAutoRecovery(context.Background(), 2, true, 1440); err != nil || ver != 3 {
		t.Fatalf("interval 1440: want accepted at v3, got ver=%d err=%v", ver, err)
	}
}

// TestKeyAutoRecoveryHandlerReadBypassesCache verifies that
// GetKeyAutoRecoveryForHandler reads straight from the DB (ignoring the
// cache) so the admin always sees authoritative state.
func TestKeyAutoRecoveryHandlerReadBypassesCache(t *testing.T) {
	db := newSvcTestDBWithKAR(t)
	svc := NewSystemSettingsService(db)

	// Prime the cache with the seeded enabled/30/v1.
	if s, _, err := svc.GetKeyAutoRecovery(context.Background()); err != nil || !s.Enabled || s.IntervalMinutes != 30 {
		t.Fatalf("prime read: %+v err=%v", s, err)
	}

	// Mutate the DB OUT FROM UNDER the cache (no service call, so the cache
	// stays stale at enabled/30/v1).
	if res := db.Exec(`UPDATE system_settings SET value='false' WHERE key='key_auto_recovery_enabled'`); res.Error != nil {
		t.Fatalf("raw update: %v", res.Error)
	}

	// Handler read must see the committed disabled, not the cached enabled.
	s, ver, err := svc.GetKeyAutoRecoveryForHandler(context.Background())
	if err != nil || s.Enabled || ver != 1 {
		t.Fatalf("handler read: want enabled=false/v1, got %+v v%d err=%v", s, ver, err)
	}
}

// TestKeyAutoRecoveryConcurrentPUTsMonotonic verifies that two concurrent
// updates collapse to exactly one winner; the loser sees Conflict and the
// cache ends on the winner's snapshot+version (monotonic publish).
func TestKeyAutoRecoveryConcurrentPUTsMonotonic(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithKAR(t))
	var wg sync.WaitGroup
	var conflicts atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.UpdateKeyAutoRecovery(context.Background(), 1, false, 45)
			if errors.Is(err, errcode.ErrKeyAutoRecoveryConflict) {
				conflicts.Add(1)
			}
		}()
	}
	wg.Wait()
	if conflicts.Load() != 1 {
		t.Fatalf("want exactly 1 conflict, got %d", conflicts.Load())
	}
	s, ver, err := svc.GetKeyAutoRecovery(context.Background())
	if err != nil || s.Enabled || s.IntervalMinutes != 45 || ver != 2 {
		t.Fatalf("cache not at winner v2 disabled/45: %+v v%d err=%v", s, ver, err)
	}
}

// TestKeyAutoRecoveryRefreshFailureDoesNotHammer verifies the negative-TTL
// fail-open path on a warm cache: the first stale read triggers one refresh
// (fails, returns last-known-good + error), and subsequent reads within the
// failure window return last-known-good silently (nil error, no DB query).
func TestKeyAutoRecoveryRefreshFailureDoesNotHammer(t *testing.T) {
	db := newSvcTestDBWithKAR(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}
	svc := NewSystemSettingsService(db)

	// Warm the cache with disabled/45 via a real update.
	if _, _, err := svc.UpdateKeyAutoRecovery(context.Background(), 1, false, 45); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if s, _, err := svc.GetKeyAutoRecovery(context.Background()); err != nil || s.Enabled || s.IntervalMinutes != 45 {
		t.Fatalf("warm read: %+v err=%v", s, err)
	}

	// Force the cache stale so the next read triggers a refresh.
	kar := svc.keyAutoRecoveryEntry()
	svc.mu.Lock()
	kar.refreshExpiry = time.Now().Add(-time.Second)
	svc.mu.Unlock()

	// Break the DB so the refresh will fail.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// First call: triggers refresh, fails, sets failure window, returns
	// last-known-good + error.
	s1, _, err1 := svc.GetKeyAutoRecovery(context.Background())
	if err1 == nil {
		t.Fatalf("first stale read: want error, got nil")
	}
	if s1.Enabled || s1.IntervalMinutes != 45 {
		t.Fatalf("first stale read: want last-known-good disabled/45, got %+v", s1)
	}

	// Verify failure window is active.
	svc.mu.RLock()
	failureUntil := kar.refreshFailureUntil
	svc.mu.RUnlock()
	if !time.Now().Before(failureUntil) {
		t.Fatalf("failure window not set or already expired: %v", failureUntil)
	}

	// Subsequent calls within the failure window: no refresh, no error,
	// return last-known-good. If these calls hit the DB they would error
	// (the DB is closed), so a nil error proves no DB query happened.
	for i := 0; i < 5; i++ {
		s2, _, err2 := svc.GetKeyAutoRecovery(context.Background())
		if err2 != nil {
			t.Fatalf("call %d: want nil error in failure window, got %v", i, err2)
		}
		if s2.Enabled || s2.IntervalMinutes != 45 {
			t.Fatalf("call %d: want last-known-good disabled/45, got %+v", i, s2)
		}
	}
}

// TestKeyAutoRecoveryRefreshFailureColdStartDoesNotHammer verifies the
// cold-start failure path: the first call triggers one refresh (fails,
// returns the shipped default + error), and subsequent calls within the
// failure window return the default silently (nil error, no DB query) —
// fail-open to the default, never block.
func TestKeyAutoRecoveryRefreshFailureColdStartDoesNotHammer(t *testing.T) {
	db := newSvcTestDBWithKAR(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}
	svc := NewSystemSettingsService(db)

	// Close the DB before the first read — cold start with a broken DB.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// First call: triggers refresh, fails, sets failure window, returns the
	// shipped default + error.
	s1, _, err1 := svc.GetKeyAutoRecovery(context.Background())
	if err1 == nil {
		t.Fatalf("first cold read: want error, got nil")
	}
	if !s1.Enabled || s1.IntervalMinutes != 30 {
		t.Fatalf("first cold read: want shipped default enabled/30, got %+v", s1)
	}

	// Verify failure window is active.
	kar := svc.keyAutoRecoveryEntry()
	svc.mu.RLock()
	failureUntil := kar.refreshFailureUntil
	svc.mu.RUnlock()
	if !time.Now().Before(failureUntil) {
		t.Fatalf("failure window not set or already expired: %v", failureUntil)
	}

	// Subsequent calls within the failure window: no refresh, no error,
	// shipped default. If these calls hit the DB they would error (the DB is
	// closed), so a nil error proves no DB query happened.
	for i := 0; i < 5; i++ {
		s2, _, err2 := svc.GetKeyAutoRecovery(context.Background())
		if err2 != nil {
			t.Fatalf("call %d: want nil error in failure window, got %v", i, err2)
		}
		if !s2.Enabled || s2.IntervalMinutes != 30 {
			t.Fatalf("call %d: want shipped default enabled/30, got %+v", i, s2)
		}
	}
}

// TestKeyAutoRecoveryCacheSlotIsolation verifies that the key-auto-recovery
// slot is independent of the other settings' slots: priming and breaking the
// KAR cache has no effect on the CSP cache lane and vice versa (invariant 5:
// cache keys isolate singleflight lanes and entries).
func TestKeyAutoRecoveryCacheSlotIsolation(t *testing.T) {
	db := newSvcTestDBWithKAR(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}
	svc := NewSystemSettingsService(db)

	// Prime both caches.
	if _, _, err := svc.CustomSystemPrompt(context.Background()); err != nil {
		t.Fatalf("prime csp: %v", err)
	}
	if _, _, err := svc.GetKeyAutoRecovery(context.Background()); err != nil {
		t.Fatalf("prime kar: %v", err)
	}

	// Force ONLY the KAR slot stale, then break the DB: the next KAR read
	// must fail (refresh attempted), while a CSP read inside the KAR failure
	// window still serves its own fresh snapshot without error.
	kar := svc.keyAutoRecoveryEntry()
	svc.mu.Lock()
	kar.refreshExpiry = time.Now().Add(-time.Second)
	svc.mu.Unlock()

	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, _, err := svc.GetKeyAutoRecovery(context.Background()); err == nil {
		t.Fatal("stale KAR read against a closed DB: want error, got nil")
	}
	if _, _, err := svc.CustomSystemPrompt(context.Background()); err != nil {
		t.Fatalf("fresh CSP read must not be affected by the KAR failure window: %v", err)
	}
}

// --- Request log retention ---------------------------------------------------

// newSvcTestDBWithRetention returns a test DB with the
// request_log_retention_days row seeded at the migration default: 0 (keep
// forever), version 1.
func newSvcTestDBWithRetention(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSvcTestDB(t)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('request_log_retention_days','0')`)
	return db
}

// TestRequestLogRetentionReadReturnsSeededDefault verifies the cold-cache
// read path primes from the seeded 0 row.
func TestRequestLogRetentionReadReturnsSeededDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithRetention(t))
	s, ver, err := svc.GetRequestLogRetention(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s.Days != 0 || ver != 1 {
		t.Fatalf("want 0/v1, got %+v v%d", s, ver)
	}
}

// TestRequestLogRetentionMissingRowsReturnDefault verifies that a
// not-yet-migrated database (row absent) reads as the keep-forever default
// with version 0 — the fallback path only; the PUT handler still refuses
// version < 1, so the row can only ever be created by the seeding migration.
func TestRequestLogRetentionMissingRowsReturnDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t)) // no retention row seeded
	s, ver, err := svc.GetRequestLogRetention(context.Background())
	if err != nil {
		t.Fatalf("read on missing rows: want nil err, got %v", err)
	}
	if s.Days != 0 || ver != 0 {
		t.Fatalf("want 0/v0 (keep-forever default), got %+v v%d", s, ver)
	}
}

// TestRequestLogRetentionCorruptRowFailOpensToDefault pins the fail-open
// contract on a failing read: a corrupt stored value surfaces as a
// repository error, and the cached read answers with the keep-forever
// default instead of propagating a zero that could be mistaken for a
// configured value or blocking the caller — nothing is ever deleted
// because the settings row went bad.
func TestRequestLogRetentionCorruptRowFailOpensToDefault(t *testing.T) {
	db := newSvcTestDBWithRetention(t)
	db.Exec(`UPDATE system_settings SET value = 'not-a-number' WHERE key = 'request_log_retention_days'`)
	svc := NewSystemSettingsService(db)
	s, _, err := svc.GetRequestLogRetention(context.Background())
	if err == nil {
		t.Fatal("want the underlying read error surfaced alongside the fail-open value")
	}
	if s.Days != 0 {
		t.Fatalf("fail-open value = %+v, want the keep-forever default (0)", s)
	}
}

// TestRequestLogRetentionUpdatePublishesImmediately verifies that a CAS
// update publishes the new snapshot to the cache so the next cleanup-loop
// read sees it without an invalidate round-trip.
func TestRequestLogRetentionUpdatePublishesImmediately(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithRetention(t))
	got, ver, err := svc.UpdateRequestLogRetention(context.Background(), 1, 30)
	if err != nil || got.Days != 30 || ver != 2 {
		t.Fatalf("update: got %+v v%d err=%v", got, ver, err)
	}
	// Cached read sees the new value immediately.
	s, _, err := svc.GetRequestLogRetention(context.Background())
	if err != nil || s.Days != 30 {
		t.Fatalf("read after update: want 30, got %+v err=%v", s, err)
	}
}

// TestRequestLogRetentionUpdateConflict verifies the CAS conflict path: a
// second save with the stale version must return
// ErrRequestLogRetentionConflict.
func TestRequestLogRetentionUpdateConflict(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithRetention(t))
	if _, _, err := svc.UpdateRequestLogRetention(context.Background(), 1, 30); err != nil {
		t.Fatalf("first update: %v", err)
	}
	_, _, err := svc.UpdateRequestLogRetention(context.Background(), 1, 7)
	if !errors.Is(err, errcode.ErrRequestLogRetentionConflict) {
		t.Fatalf("want ErrRequestLogRetentionConflict, got %v", err)
	}
}

// TestRequestLogRetentionRejectsDaysOutOfBounds verifies the days domain
// (whole days, 0..3650) at the edges: the inclusive endpoints pass, and
// negative / 3651 are rejected with the setting's own validation error —
// and the rejection leaves the stored value untouched.
func TestRequestLogRetentionRejectsDaysOutOfBounds(t *testing.T) {
	for _, bad := range []int{-1, 3651} {
		svc := NewSystemSettingsService(newSvcTestDBWithRetention(t))
		_, _, err := svc.UpdateRequestLogRetention(context.Background(), 1, bad)
		if !errors.Is(err, errcode.ErrRequestLogRetentionDaysInvalid) {
			t.Fatalf("days %d: want ErrRequestLogRetentionDaysInvalid, got %v", bad, err)
		}
		s, _, rerr := svc.GetRequestLogRetentionForHandler(context.Background())
		if rerr != nil || s.Days != 0 {
			t.Fatalf("days %d: stored value after rejection = %+v err=%v, want 0 unchanged", bad, s, rerr)
		}
	}
	// The inclusive endpoints pass; each accepted update advances the
	// version, so the second save carries the first one's new version.
	svc := NewSystemSettingsService(newSvcTestDBWithRetention(t))
	if _, ver, err := svc.UpdateRequestLogRetention(context.Background(), 1, 0); err != nil || ver != 2 {
		t.Fatalf("days 0: want accepted at v2, got ver=%d err=%v", ver, err)
	}
	if _, ver, err := svc.UpdateRequestLogRetention(context.Background(), 2, 3650); err != nil || ver != 3 {
		t.Fatalf("days 3650: want accepted at v3, got ver=%d err=%v", ver, err)
	}
}

// TestRequestLogRetentionHandlerReadBypassesCache verifies that
// GetRequestLogRetentionForHandler reads straight from the DB (ignoring the
// cache) so the admin always sees authoritative state.
func TestRequestLogRetentionHandlerReadBypassesCache(t *testing.T) {
	db := newSvcTestDBWithRetention(t)
	svc := NewSystemSettingsService(db)

	// Prime the cache with the seeded 0/v1.
	if s, _, err := svc.GetRequestLogRetention(context.Background()); err != nil || s.Days != 0 {
		t.Fatalf("prime read: %+v err=%v", s, err)
	}

	// Mutate the DB OUT FROM UNDER the cache.
	if res := db.Exec(`UPDATE system_settings SET value='30' WHERE key='request_log_retention_days'`); res.Error != nil {
		t.Fatalf("raw update: %v", res.Error)
	}

	// The handler read must see 30 even though the cache still holds 0.
	s, ver, err := svc.GetRequestLogRetentionForHandler(context.Background())
	if err != nil || s.Days != 30 || ver != 1 {
		t.Fatalf("handler read: want 30/v1, got %+v v%d err=%v", s, ver, err)
	}
}

// --- Admin audit retention --------------------------------------------------

// newSvcTestDBWithAuditRetention returns a test DB with the
// admin_audit_retention_days row seeded at the migration default (180).
func newSvcTestDBWithAuditRetention(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSvcTestDB(t)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('admin_audit_retention_days','180')`)
	return db
}

// TestAdminAuditRetentionReadReturnsSeededDefault verifies the cold-cache
// read path primes from the seeded 180-day row.
func TestAdminAuditRetentionReadReturnsSeededDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithAuditRetention(t))
	s, ver, err := svc.GetAdminAuditRetention(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s.Days != 180 || ver != 1 {
		t.Fatalf("want 180/v1, got %+v v%d", s, ver)
	}
}

// TestAdminAuditRetentionMissingRowsReturnDefault verifies that a
// not-yet-migrated database (row absent) reads as the shipped 180-day
// default with version 0 — the fallback path only; the PUT handler still
// refuses version < 1, so the row can only ever be created by the seeding
// migration.
func TestAdminAuditRetentionMissingRowsReturnDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t)) // no audit retention row seeded
	s, ver, err := svc.GetAdminAuditRetention(context.Background())
	if err != nil {
		t.Fatalf("read on missing row: want nil err, got %v", err)
	}
	if s.Days != 180 || ver != 0 {
		t.Fatalf("want 180/v0 (shipped default), got %+v v%d", s, ver)
	}
}

// TestAdminAuditRetentionCorruptRowFailOpensToDefault pins the fail-open
// contract on a failing read: a corrupt stored value surfaces as a
// repository error, and the cached read answers with the shipped default
// instead of propagating a value that could be mistaken for a configured
// one or blocking the caller — the loop keeps beating on the last known
// sane window, never on garbage.
func TestAdminAuditRetentionCorruptRowFailOpensToDefault(t *testing.T) {
	db := newSvcTestDBWithAuditRetention(t)
	db.Exec(`UPDATE system_settings SET value = 'not-a-number' WHERE key = 'admin_audit_retention_days'`)
	svc := NewSystemSettingsService(db)
	s, _, err := svc.GetAdminAuditRetention(context.Background())
	if err == nil {
		t.Fatal("want the underlying read error surfaced alongside the fail-open value")
	}
	if s.Days != 180 {
		t.Fatalf("fail-open value = %+v, want the shipped default (180)", s)
	}
}

// TestAdminAuditRetentionUpdatePublishesImmediately verifies that a CAS
// update publishes the new snapshot to the cache so the audit retention
// loop's next read sees it without an invalidate round-trip.
func TestAdminAuditRetentionUpdatePublishesImmediately(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithAuditRetention(t))
	got, ver, err := svc.UpdateAdminAuditRetention(context.Background(), 1, 30)
	if err != nil || got.Days != 30 || ver != 2 {
		t.Fatalf("update: got %+v v%d err=%v", got, ver, err)
	}
	// Cached read sees the new value immediately.
	s, _, err := svc.GetAdminAuditRetention(context.Background())
	if err != nil || s.Days != 30 {
		t.Fatalf("read after update: want 30, got %+v err=%v", s, err)
	}
}

// TestAdminAuditRetentionUpdateConflict verifies the CAS conflict path: a
// second save with the stale version must return
// ErrAdminAuditRetentionConflict.
func TestAdminAuditRetentionUpdateConflict(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithAuditRetention(t))
	if _, _, err := svc.UpdateAdminAuditRetention(context.Background(), 1, 30); err != nil {
		t.Fatalf("first update: %v", err)
	}
	_, _, err := svc.UpdateAdminAuditRetention(context.Background(), 1, 7)
	if !errors.Is(err, errcode.ErrAdminAuditRetentionConflict) {
		t.Fatalf("want ErrAdminAuditRetentionConflict, got %v", err)
	}
}

// TestAdminAuditRetentionRejectsDaysOutOfBounds verifies the days domain
// (whole days, 0..3650) at the edges: the inclusive endpoints pass, and
// negative / 3651 are rejected with the setting's own validation error —
// and the rejection leaves the stored value untouched.
func TestAdminAuditRetentionRejectsDaysOutOfBounds(t *testing.T) {
	for _, bad := range []int{-1, 3651} {
		svc := NewSystemSettingsService(newSvcTestDBWithAuditRetention(t))
		_, _, err := svc.UpdateAdminAuditRetention(context.Background(), 1, bad)
		if !errors.Is(err, errcode.ErrAdminAuditRetentionDaysInvalid) {
			t.Fatalf("days %d: want ErrAdminAuditRetentionDaysInvalid, got %v", bad, err)
		}
		s, _, rerr := svc.GetAdminAuditRetentionForHandler(context.Background())
		if rerr != nil || s.Days != 180 {
			t.Fatalf("days %d: stored value after rejection = %+v err=%v, want 180 unchanged", bad, s, rerr)
		}
	}
	// The inclusive endpoints pass; each accepted update advances the
	// version, so the second save carries the first one's new version.
	svc := NewSystemSettingsService(newSvcTestDBWithAuditRetention(t))
	if _, ver, err := svc.UpdateAdminAuditRetention(context.Background(), 1, 0); err != nil || ver != 2 {
		t.Fatalf("days 0: want accepted at v2, got ver=%d err=%v", ver, err)
	}
	if _, ver, err := svc.UpdateAdminAuditRetention(context.Background(), 2, 3650); err != nil || ver != 3 {
		t.Fatalf("days 3650: want accepted at v3, got ver=%d err=%v", ver, err)
	}
}

// TestAdminAuditRetentionHandlerReadBypassesCache verifies that
// GetAdminAuditRetentionForHandler reads straight from the DB (ignoring
// the cache) so the admin always sees authoritative state.
func TestAdminAuditRetentionHandlerReadBypassesCache(t *testing.T) {
	db := newSvcTestDBWithAuditRetention(t)
	svc := NewSystemSettingsService(db)

	// Prime the cache with the seeded 180/v1.
	if s, _, err := svc.GetAdminAuditRetention(context.Background()); err != nil || s.Days != 180 {
		t.Fatalf("prime read: %+v err=%v", s, err)
	}

	// Mutate the DB OUT FROM UNDER the cache.
	if res := db.Exec(`UPDATE system_settings SET value='30' WHERE key='admin_audit_retention_days'`); res.Error != nil {
		t.Fatalf("raw update: %v", res.Error)
	}

	// The handler read must see 30 even though the cache still holds 180.
	s, ver, err := svc.GetAdminAuditRetentionForHandler(context.Background())
	if err != nil || s.Days != 30 || ver != 1 {
		t.Fatalf("handler read: want 30/v1, got %+v v%d err=%v", s, ver, err)
	}
}

// --- Outbound proxy -----------------------------------------------------------

// newSvcTestDBWithProxy returns a test DB with the outbound-proxy pair
// seeded at the migration default: mode off, empty URL, both rows at v1.
func newSvcTestDBWithProxy(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSvcTestDB(t)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('proxy_mode','off'),('proxy_url','')`)
	return db
}

// The gateway read path serves the shipped off default both when the rows
// are absent (a pre-migration database) and on a cold-cache refresh
// failure — fail-open to DIRECT, never to a guessed proxy.
func TestOutboundProxyReadFailsOpenToOffDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t)) // no proxy rows seeded
	s, ver, err := svc.GetOutboundProxy(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s.Mode != "off" || s.URL != "" || ver != 0 {
		t.Fatalf("want off/empty default, got %+v v%d", s, ver)
	}
}

func TestOutboundProxyUpdatePublishesImmediately(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithProxy(t))
	s, ver, err := svc.UpdateOutboundProxy(context.Background(), 1, "custom", "socks5://192.168.1.5:1080")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if s.Mode != "custom" || s.URL != "socks5://192.168.1.5:1080" || ver != 2 {
		t.Fatalf("want custom/v2, got %+v v%d", s, ver)
	}
	// The gateway read sees the committed snapshot without any invalidate.
	got, _, err := svc.GetOutboundProxy(context.Background())
	if err != nil || got.Mode != "custom" || got.URL != "socks5://192.168.1.5:1080" {
		t.Fatalf("read after update mismatch: %+v err=%v", got, err)
	}
}

// All four transport-supported schemes are accepted at save time; socks5h
// rides through as the alias of socks5 (Go resolves DNS on the proxy side
// for both).
func TestOutboundProxyUpdateAcceptsFourSchemes(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:7890",
		"https://127.0.0.1:7890",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
	} {
		svc := NewSystemSettingsService(newSvcTestDBWithProxy(t))
		if _, _, err := svc.UpdateOutboundProxy(context.Background(), 1, "custom", raw); err != nil {
			t.Errorf("UpdateOutboundProxy(custom, %q) = %v, want accepted", raw, err)
		}
	}
}

// The three illegal shapes from the acceptance matrix — an unsupported
// scheme, a schemeless string, and an empty URL under custom mode — are
// all refused, and the refusal leaves the previously committed value in
// place (old value retained, version untouched).
func TestOutboundProxyUpdateRejectsIllegalValuesAndKeepsOld(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithProxy(t))
	// Commit a legal custom proxy first.
	if _, ver, err := svc.UpdateOutboundProxy(context.Background(), 1, "custom", "http://127.0.0.1:7890"); err != nil || ver != 2 {
		t.Fatalf("seed update: %+v v%d err=%v", nil, ver, err)
	}
	for _, raw := range []string{"ftp://proxy.example.com:21", "proxy.example.com:7890", ""} {
		if _, _, err := svc.UpdateOutboundProxy(context.Background(), 2, "custom", raw); err == nil {
			t.Errorf("UpdateOutboundProxy(custom, %q) = nil, want refused", raw)
		}
	}
	// The committed value survived every refusal, in the cache and in the DB.
	got, ver, err := svc.GetOutboundProxy(context.Background())
	if err != nil || got.Mode != "custom" || got.URL != "http://127.0.0.1:7890" || ver != 2 {
		t.Fatalf("refused saves must retain the old value, got %+v v%d err=%v", got, ver, err)
	}
	authoritative, _, err := svc.GetOutboundProxyForHandler(context.Background())
	if err != nil || authoritative != got {
		t.Fatalf("authoritative read after refusals: %+v err=%v", authoritative, err)
	}
}

// A URL parked behind a non-custom mode must still be storable-shape: a
// garbage URL cannot hide behind off/system and become active unvalidated
// when the mode later flips to custom.
func TestOutboundProxyUpdateRejectsBadURLEvenUnderOffMode(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithProxy(t))
	if _, _, err := svc.UpdateOutboundProxy(context.Background(), 1, "off", "ftp://proxy.example.com:21"); err == nil {
		t.Fatal("non-empty proxy_url must validate regardless of mode, got nil")
	}
	// An empty URL under off/system stays legal (the shipped state).
	if _, _, err := svc.UpdateOutboundProxy(context.Background(), 1, "off", ""); err != nil {
		t.Fatalf("off + empty URL must be accepted: %v", err)
	}
}

// A proxy URL whose address sits in an always-denied reserved range is
// refused; private and loopback addresses are accepted (save-time leg —
// the transport enforces the same ranges on every dial).
func TestOutboundProxyUpdateRejectsReservedRangeAddresses(t *testing.T) {
	for _, raw := range []string{
		"http://224.0.0.1:7890",  // multicast
		"http://240.0.0.1:7890",  // reserved
		"http://0.0.0.0:7890",    // unspecified
		"http://198.18.0.1:7890", // benchmark
	} {
		svc := NewSystemSettingsService(newSvcTestDBWithProxy(t))
		if _, _, err := svc.UpdateOutboundProxy(context.Background(), 1, "custom", raw); err == nil {
			t.Errorf("UpdateOutboundProxy(custom, %q) = nil, want refused", raw)
		}
	}
	for _, raw := range []string{"socks5://10.1.2.3:1080", "http://127.0.0.1:7890", "http://192.168.1.5:7890"} {
		svc := NewSystemSettingsService(newSvcTestDBWithProxy(t))
		if _, _, err := svc.UpdateOutboundProxy(context.Background(), 1, "custom", raw); err != nil {
			t.Errorf("UpdateOutboundProxy(custom, %q) = %v, want accepted", raw, err)
		}
	}
}

// Unknown modes never reach the DB: the write path validates the
// off/system/custom vocabulary first.
func TestOutboundProxyUpdateRejectsUnknownMode(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithProxy(t))
	if _, _, err := svc.UpdateOutboundProxy(context.Background(), 1, "sometimes", ""); err == nil {
		t.Fatal("unknown mode must be refused")
	}
	got, _, err := svc.GetOutboundProxy(context.Background())
	if err != nil || got.Mode != "off" {
		t.Fatalf("refused mode must leave the stored off default, got %+v err=%v", got, err)
	}
}

// CAS miss on the proxy pair surfaces as the setting family's own conflict
// sentinel, same contract as the other settings pairs.
func TestOutboundProxyUpdateCASConflict(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithProxy(t))
	if _, _, err := svc.UpdateOutboundProxy(context.Background(), 1, "system", ""); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if _, _, err := svc.UpdateOutboundProxy(context.Background(), 1, "custom", "http://127.0.0.1:7890"); !errors.Is(err, errcode.ErrOutboundProxyConflict) {
		t.Fatalf("want ErrOutboundProxyConflict, got %v", err)
	}
}

// --- Capability enforcement ---------------------------------------------------

// newSvcTestDBWithCapability returns a test DB with the seven
// capability-enforcement rows seeded at the migration default: every
// switch off, margin 5, all rows at v1.
func newSvcTestDBWithCapability(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSvcTestDB(t)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES
		('capability_context_limit_block','false'),
		('capability_temperature_default','false'),
		('capability_temperature_force','false'),
		('capability_tools_strict','false'),
		('capability_reasoning_strict','false'),
		('capability_reasoning_egress_clear','false'),
		('capability_context_margin_percent','5')`)
	return db
}

// allOnCapability is the every-switch-on snapshot used to prove a write
// round-trips every field (and, downstream, that a read failure never
// un-configures it).
func allOnCapability() settings.CapabilityEnforcementSetting {
	return settings.CapabilityEnforcementSetting{
		ContextLimitBlock:    true,
		TemperatureDefault:   true,
		TemperatureForce:     true,
		ToolsStrict:          true,
		ReasoningStrict:      true,
		ReasoningEgressClear: true,
		ContextMarginPercent: 12,
	}
}

// The seeded default reads every switch off with the 5% margin — an
// upgraded deployment changes nothing until an admin flips a switch.
func TestCapabilityEnforcementReadReturnsSeededDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithCapability(t))
	s, ver, err := svc.GetCapabilityEnforcement(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s != (settings.CapabilityEnforcementSetting{ContextMarginPercent: 5}) {
		t.Fatalf("want all-off + margin 5, got %+v", s)
	}
	if ver != 1 {
		t.Fatalf("want v1, got v%d", ver)
	}
}

// A database migrated before the seeding migration reads the same default
// without erroring — the gateway read path never fails closed on
// configuration.
func TestCapabilityEnforcementMissingRowsReturnDefault(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDB(t)) // no capability rows seeded
	s, ver, err := svc.GetCapabilityEnforcement(context.Background())
	if err != nil {
		t.Fatalf("read on missing rows: want nil err, got %v", err)
	}
	if s != settings.DefaultCapabilityEnforcementSetting() || ver != 0 {
		t.Fatalf("want shipped default/v0, got %+v v%d", s, ver)
	}
}

// A CAS update commits all seven rows and publishes the snapshot so the
// cached read sees it without an invalidate round-trip.
func TestCapabilityEnforcementUpdatePublishesImmediately(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithCapability(t))
	want := allOnCapability()
	got, ver, err := svc.UpdateCapabilityEnforcement(context.Background(), 1, want)
	if err != nil || ver != 2 {
		t.Fatalf("update: got %+v v%d err=%v", got, ver, err)
	}
	if got != want {
		t.Fatalf("update round-trip mismatch: got %+v want %+v", got, want)
	}
	again, _, err := svc.GetCapabilityEnforcement(context.Background())
	if err != nil || again != want {
		t.Fatalf("read after update: got %+v err=%v, want %+v", again, err, want)
	}
}

// A second save with the stale version conflicts, same CAS contract as the
// other settings families.
func TestCapabilityEnforcementUpdateConflict(t *testing.T) {
	svc := NewSystemSettingsService(newSvcTestDBWithCapability(t))
	if _, _, err := svc.UpdateCapabilityEnforcement(context.Background(), 1, allOnCapability()); err != nil {
		t.Fatalf("first update: %v", err)
	}
	_, _, err := svc.UpdateCapabilityEnforcement(context.Background(), 1, settings.CapabilityEnforcementSetting{})
	if !errors.Is(err, errcode.ErrCapabilityEnforcementConflict) {
		t.Fatalf("want ErrCapabilityEnforcementConflict, got %v", err)
	}
}

// The margin domain is validated before any row is touched; a refused
// write leaves the stored snapshot alone.
func TestCapabilityEnforcementRejectsMarginOutOfBounds(t *testing.T) {
	for _, margin := range []int{-1, 1001} {
		svc := NewSystemSettingsService(newSvcTestDBWithCapability(t))
		snap := allOnCapability()
		snap.ContextMarginPercent = margin
		if _, _, err := svc.UpdateCapabilityEnforcement(context.Background(), 1, snap); !errors.Is(err, errcode.ErrCapabilityContextMarginInvalid) {
			t.Fatalf("margin %d: want ErrCapabilityContextMarginInvalid, got %v", margin, err)
		}
		got, _, err := svc.GetCapabilityEnforcement(context.Background())
		if err != nil || got != (settings.CapabilityEnforcementSetting{ContextMarginPercent: 5}) {
			t.Fatalf("refused write must leave the stored default, got %+v err=%v", got, err)
		}
	}
	for _, margin := range []int{0, 1000} {
		svc := NewSystemSettingsService(newSvcTestDBWithCapability(t))
		snap := allOnCapability()
		snap.ContextMarginPercent = margin
		if _, _, err := svc.UpdateCapabilityEnforcement(context.Background(), 1, snap); err != nil {
			t.Fatalf("margin %d: want accepted, got %v", margin, err)
		}
	}
}

// Cold cache with a broken DB: the first read fails open to the shipped
// all-off default (with the error), and the failure window serves the same
// default silently — the cold-cache arm of "no row / cold cache = off".
func TestCapabilityEnforcementColdCacheFailsOpenToOff(t *testing.T) {
	db := newSvcTestDBWithCapability(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}
	svc := NewSystemSettingsService(db)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	s, _, err := svc.GetCapabilityEnforcement(context.Background())
	if err == nil {
		t.Fatal("first cold read: want error, got nil")
	}
	if s != settings.DefaultCapabilityEnforcementSetting() {
		t.Fatalf("cold read: want all-off + margin 5, got %+v", s)
	}
	for i := 0; i < 3; i++ {
		s, _, err := svc.GetCapabilityEnforcement(context.Background())
		if err != nil {
			t.Fatalf("call %d in failure window: want nil error, got %v", i, err)
		}
		if s != settings.DefaultCapabilityEnforcementSetting() {
			t.Fatalf("call %d in failure window: want all-off + margin 5, got %+v", i, s)
		}
	}
}

// A corrupt switch value (outside true/false) surfaces as an error rather
// than being read as off — corruption must be visible, not silent.
func TestCapabilityEnforcementCorruptSwitchSurfaces(t *testing.T) {
	db := newSvcTestDBWithCapability(t)
	if res := db.Exec(`UPDATE system_settings SET value='maybe' WHERE key='capability_tools_strict'`); res.Error != nil {
		t.Fatalf("corrupt row: %v", res.Error)
	}
	svc := NewSystemSettingsService(db)
	if _, _, err := svc.GetCapabilityEnforcement(context.Background()); err == nil {
		t.Fatal("corrupt switch value must surface as an error")
	}
}
