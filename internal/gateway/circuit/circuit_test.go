package circuit

import (
	"testing"
	"time"
)

func testBreaker() (*Breaker, *time.Time) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	b := NewWithClock(Config{FailureThreshold: 3, SuccessThreshold: 2, OpenTimeout: time.Minute},
		func() time.Time { return now })
	return b, &now
}

// allow is Allow with the admission asserted, returning the generation.
func allow(t *testing.T, b *Breaker, id uint) uint64 {
	t.Helper()
	ok, gen := b.Allow(id, 0)
	if !ok {
		t.Fatal("expected the breaker to allow")
	}
	return gen
}

func refused(b *Breaker, id uint) bool {
	ok, _ := b.Allow(id, 0)
	return !ok
}

func TestConsecutiveFailuresOpenTheBreaker(t *testing.T) {
	b, _ := testBreaker()
	b.RecordFailure(1, 0)
	b.RecordFailure(1, 0)
	if refused(b, 1) {
		t.Fatal("breaker opened below the threshold")
	}
	b.RecordFailure(1, 0)
	if !refused(b, 1) {
		t.Fatal("breaker still allows after the threshold was reached")
	}
}

func TestSuccessResetsTheStreak(t *testing.T) {
	b, _ := testBreaker()
	b.RecordFailure(1, 0)
	b.RecordFailure(1, 0)
	b.RecordSuccess(1, 0)
	b.RecordFailure(1, 0)
	b.RecordFailure(1, 0)
	if refused(b, 1) {
		t.Fatal("the threshold counted a lifetime, not a streak")
	}
}

func TestOpenWindowElapsesIntoAProbe(t *testing.T) {
	b, now := testBreaker()
	for range 3 {
		b.RecordFailure(1, 0)
	}
	if !refused(b, 1) {
		t.Fatal("open breaker allowed before the window elapsed")
	}
	*now = now.Add(time.Minute)
	allow(t, b, 1)
}

func TestHalfOpenRateLimitsProbes(t *testing.T) {
	b, now := testBreaker()
	for range 3 {
		b.RecordFailure(1, 0)
	}
	*now = now.Add(time.Minute)
	allow(t, b, 1) // the transition itself admits the first probe
	if !refused(b, 1) {
		t.Fatal("half-open admitted a second probe inside the interval")
	}
	// One probe per OpenTimeout/SuccessThreshold (30s here). The bound is on
	// time rather than outstanding probes, so an admission that never books
	// any verdict — a terminal 4xx, a hung-up caller, a pre-dispatch skip —
	// costs one interval and can never wedge the breaker.
	*now = now.Add(30 * time.Second)
	allow(t, b, 1)
	if !refused(b, 1) {
		t.Fatal("probe rate limit did not re-arm")
	}
}

func TestProbeSuccessesCloseTheBreaker(t *testing.T) {
	b, now := testBreaker()
	for range 3 {
		b.RecordFailure(1, 0)
	}
	*now = now.Add(time.Minute)
	gen := allow(t, b, 1)
	b.RecordSuccess(1, gen)
	b.RecordSuccess(1, gen)
	// Closed again: two fresh failures must NOT re-open (streak restarted),
	// and they carry the post-recovery generation.
	closedGen := allow(t, b, 1)
	b.RecordFailure(1, closedGen)
	b.RecordFailure(1, closedGen)
	if refused(b, 1) {
		t.Fatal("recovery did not restore a clean closed state")
	}
}

func TestProbeFailureReopensImmediately(t *testing.T) {
	b, now := testBreaker()
	for range 3 {
		b.RecordFailure(1, 0)
	}
	*now = now.Add(time.Minute)
	gen := allow(t, b, 1)
	b.RecordFailure(1, gen)
	if !refused(b, 1) {
		t.Fatal("failed probe did not re-open the breaker")
	}
}

// A result from a request dispatched before a transition carries the old
// generation and must not move the record: without this, an upstream call
// that outlives the open window (attempt timeouts are minutes, the window is
// seconds) would close or re-open the breaker on evidence from the wrong
// era.
func TestStaleResultsAreNotBooked(t *testing.T) {
	b, now := testBreaker()
	preTripGen := allow(t, b, 1) // a long-running request admitted while closed
	for range 3 {
		b.RecordFailure(1, 0)
	}
	*now = now.Add(time.Minute)
	gen := allow(t, b, 1)
	// The pre-trip request finally succeeds twice over — stale, dropped.
	b.RecordSuccess(1, preTripGen)
	b.RecordSuccess(1, preTripGen)
	if refused(b, 1) == false {
		// Still half-open with one live probe out: a second probe may be
		// admitted, but the breaker must NOT have closed. Prove it by
		// showing a live-generation failure still re-opens instantly, which
		// only a half-open breaker does.
		b.RecordFailure(1, gen)
		if !refused(b, 1) {
			t.Fatal("stale successes closed the breaker")
		}
		return
	}
}

// Soft faults are half-weight: they open the breaker only at twice the
// threshold, and a success clears the accumulation like any streak.
func TestSoftFaultsCountHalf(t *testing.T) {
	b, _ := testBreaker()
	for range 5 {
		b.RecordSoftFailure(1, 0)
	}
	if refused(b, 1) {
		t.Fatal("breaker opened below twice the threshold on soft faults")
	}
	b.RecordSoftFailure(1, 0) // 6th = 3 full failures = threshold
	if !refused(b, 1) {
		t.Fatal("persistent soft faults never opened the breaker")
	}
}

func TestSuccessClearsSoftAccumulation(t *testing.T) {
	b, _ := testBreaker()
	for range 5 {
		b.RecordSoftFailure(1, 0)
	}
	b.RecordSuccess(1, 0)
	for range 5 {
		b.RecordSoftFailure(1, 0)
	}
	if refused(b, 1) {
		t.Fatal("soft accumulation survived a success")
	}
}

// A healthy probe between two soft faults means they were not consecutive:
// the recovery must not flap back open on non-consecutive half-signals.
func TestHalfOpenSuccessClearsSoftAccumulation(t *testing.T) {
	b, now := testBreaker()
	for range 3 {
		b.RecordFailure(1, 0)
	}
	*now = now.Add(time.Minute)
	gen := allow(t, b, 1)
	b.RecordSoftFailure(1, gen)
	b.RecordSuccess(1, gen)
	b.RecordSoftFailure(1, gen)
	// Still half-open (not re-opened): after one probe interval the next
	// probe is admitted. A re-opened breaker would refuse — 30s is only half
	// its open window.
	*now = now.Add(30 * time.Second)
	if refused(b, 1) {
		t.Fatal("non-consecutive soft faults re-opened a recovering breaker")
	}
}

// A transition since the admission revokes it: StillAllowed is the pure read
// a rotation loop uses to stop dispatching after its own faults tripped the
// breaker.
func TestTransitionRevokesAnEarlierAdmission(t *testing.T) {
	b, _ := testBreaker()
	gen := allow(t, b, 1)
	if !b.StillAllowed(1, gen) {
		t.Fatal("admission revoked with no transition")
	}
	for range 3 {
		b.RecordFailure(1, 0)
	}
	if b.StillAllowed(1, gen) {
		t.Fatal("the trip did not revoke the earlier admission")
	}
}

// The probe interval never truncates to zero: an absurdly small open window
// divided by the success threshold must not turn the half-open rate limit
// into an always-pass.
func TestProbeIntervalNeverTruncatesToZero(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	b := NewWithClock(Config{FailureThreshold: 1, SuccessThreshold: 2, OpenTimeout: time.Nanosecond},
		func() time.Time { return now })
	b.RecordFailure(1, 0)
	now = now.Add(time.Nanosecond) // open window elapses
	ok, _ := b.Allow(1, 0)
	if !ok {
		t.Fatal("probe not admitted after the window")
	}
	now = now.Add(time.Nanosecond)
	if ok, _ := b.Allow(1, 0); ok {
		t.Fatal("a truncated-to-zero interval admitted every request")
	}
}

// A changed destination is a different backend: the record starts clean, and
// the generation bump stamps the old destination's in-flight results stale.
func TestDestinationChangeResetsTheRecord(t *testing.T) {
	b, _ := testBreaker()
	for range 3 {
		b.RecordFailure(1, 0)
	}
	if ok, _ := b.Allow(1, 0); ok {
		t.Fatal("breaker should be open on the old destination")
	}
	ok, gen := b.Allow(1, 7)
	if !ok {
		t.Fatal("a repaired destination inherited the old destination's open breaker")
	}
	// The old destination's in-flight failure arrives — stale, dropped.
	b.RecordFailure(1, 0)
	b.RecordFailure(1, gen)
	b.RecordFailure(1, gen)
	if ok, _ := b.Allow(1, 7); !ok {
		t.Fatal("fresh record tripped below the threshold")
	}
}

func TestProvidersAreIndependent(t *testing.T) {
	b, _ := testBreaker()
	for range 3 {
		b.RecordFailure(1, 0)
	}
	if refused(b, 2) {
		t.Fatal("one provider's faults tripped another's breaker")
	}
}

func TestNilBreakerNeverOpens(t *testing.T) {
	var b *Breaker
	b.RecordFailure(1, 0)
	b.RecordSuccess(1, 0)
	if refused(b, 1) {
		t.Fatal("nil breaker refused traffic")
	}
}

func TestIsOpenTracksTheOpenWindowWithoutTransitions(t *testing.T) {
	b, now := testBreaker()

	if b.IsOpen(1, 0) {
		t.Fatal("unknown provider reported open")
	}
	for i := 0; i < 3; i++ {
		allow(t, b, 1)
		b.RecordFailure(1, 0)
	}
	if !b.IsOpen(1, 0) {
		t.Fatal("open breaker within its window reported not open")
	}

	// The window elapses. IsOpen flips to false — the provider deserves
	// recovery traffic — while the read itself stays side-effect-free (the
	// generation-stability half of that promise is pinned by
	// TestIsOpenHasNoSideEffectsOnGeneration).
	*now = now.Add(2 * time.Minute)
	if b.IsOpen(1, 0) {
		t.Fatal("open breaker past its window still reported open")
	}
}

func TestIsOpenIgnoresRecordsFromAnotherDestination(t *testing.T) {
	b, _ := testBreaker()
	for i := 0; i < 3; i++ {
		allow(t, b, 1)
		b.RecordFailure(1, 0)
	}
	if !b.IsOpen(1, 0) {
		t.Fatal("open breaker not reported open for its own destination")
	}
	// The admin repaired the provider: a changed destination version means
	// the open record describes a backend that no longer exists, and holding
	// it against the repaired one would demote it for the remainder of a
	// stale window.
	if b.IsOpen(1, 1) {
		t.Fatal("a stale destination's open record was reported against the repaired destination")
	}
}

func TestIsOpenReportsHalfOpenAsAlive(t *testing.T) {
	b, now := testBreaker()
	for i := 0; i < 3; i++ {
		allow(t, b, 1)
		b.RecordFailure(1, 0)
	}
	*now = now.Add(time.Minute)
	allow(t, b, 1) // transitions to half-open and admits the probe
	if b.IsOpen(1, 0) {
		t.Fatal("half-open breaker reported open; it is already letting probes through")
	}
}

func TestIsOpenHasNoSideEffectsOnGeneration(t *testing.T) {
	b, now := testBreaker()
	for i := 0; i < 3; i++ {
		allow(t, b, 1)
		b.RecordFailure(1, 0)
	}
	// Trip bumped provider 1's generation. Admit a half-open probe to get a
	// LIVE generation, then hammer IsOpen: a read-only snapshot must neither
	// revoke the live admission nor resurrect one the trip already revoked.
	*now = now.Add(time.Minute)
	_, gen := b.Allow(1, 0)
	for i := 0; i < 10; i++ {
		b.IsOpen(1, 0)
	}
	if !b.StillAllowed(1, gen) {
		t.Fatal("IsOpen reads revoked a live admission; a read-only snapshot must not bump the generation")
	}
	// Generation 0 is what every admission carried before the trip (no
	// transition had bumped it yet), so asserting against the literal pins
	// that era without pretending a live admission was captured.
	var generationZero uint64 // the pre-trip era's generation
	if b.StillAllowed(1, generationZero) {
		t.Fatal("admission from before the trip reads as current after IsOpen reads")
	}
}
