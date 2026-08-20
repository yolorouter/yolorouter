package systemsettings

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
