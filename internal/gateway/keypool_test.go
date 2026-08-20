package gateway

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
)

// keysWith builds a sort-ordered pool from IDs, and idsOf flattens one back,
// so order assertions read as the sequence of key IDs a walk would dispatch.
func keysWith(ids ...uint) []model.ProviderKey {
	out := make([]model.ProviderKey, len(ids))
	for i, id := range ids {
		out[i] = model.ProviderKey{ID: id}
	}
	return out
}

func idsOf(keys []model.ProviderKey) []uint {
	out := make([]uint, len(keys))
	for i, k := range keys {
		out[i] = k.ID
	}
	return out
}

func assertIDs(t *testing.T, got []model.ProviderKey, want ...uint) {
	t.Helper()
	gotIDs := idsOf(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("walk order = %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("walk order = %v, want %v", gotIDs, want)
		}
	}
}

// fakePool is a pool on an advancable clock, the keypool twin of the
// NewWithClock swap the breaker tests use.
func fakePool(t *testing.T) (*keyPool, func(time.Duration)) {
	t.Helper()
	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	now := time.Unix(0, 0)
	p := newKeyPool(func() time.Time {
		<-mu
		defer func() { mu <- struct{}{} }()
		return now
	})
	return p, func(d time.Duration) {
		<-mu
		now = now.Add(d)
		mu <- struct{}{}
	}
}

// Consecutive walks must start on consecutive keys, wrapping at the end —
// the whole point of the pool: a two-key provider alternates first dispatch.
func TestWalkOrderRotatesStartAcrossCalls(t *testing.T) {
	p, _ := fakePool(t)
	keys := keysWith(1, 2, 3)
	assertIDs(t, p.walkOrder(7, keys), 1, 2, 3) // first walk starts at 0
	assertIDs(t, p.walkOrder(7, keys), 2, 3, 1)
	assertIDs(t, p.walkOrder(7, keys), 3, 1, 2)
	assertIDs(t, p.walkOrder(7, keys), 1, 2, 3) // wrapped
}

// A benched key walks behind healthy ones, keeping its relative position
// among them: demotion, never exclusion.
func TestWalkOrderBenchesCoolingKeysAtTail(t *testing.T) {
	p, _ := fakePool(t)
	keys := keysWith(1, 2, 3)
	p.coolKey(1, 0, p.stamp(), 10*time.Second)
	assertIDs(t, p.walkOrder(7, keys), 2, 3, 1)
}

// Several benched keys tail in soonest-to-recover order, so the pool
// re-admits whichever upstream will serve again first.
func TestWalkOrderTailsSoonestRecoveryFirst(t *testing.T) {
	p, _ := fakePool(t)
	keys := keysWith(1, 2, 3)
	p.coolKey(1, 0, p.stamp(), 10*time.Second)
	p.coolKey(3, 0, p.stamp(), 5*time.Second)
	assertIDs(t, p.walkOrder(7, keys), 2, 3, 1) // 3 recovers at +5s, 1 at +10s
}

// Rotation runs over the healthy subset, so the cursor cannot land on a
// benched key and hand its turn to the same healthy neighbour twice — with
// [1,2,3] and 1 cooling, whole-pool rotation would start 2,2,3 across
// consecutive walks, double-serving key 2 exactly while the pool is already
// limited.
func TestWalkOrderRotatesAcrossHealthySubset(t *testing.T) {
	p, _ := fakePool(t)
	keys := keysWith(1, 2, 3)
	p.coolKey(1, 0, p.stamp(), time.Hour)
	assertIDs(t, p.walkOrder(7, keys), 2, 3, 1)
	assertIDs(t, p.walkOrder(7, keys), 3, 2, 1) // healthy keys alternate
	assertIDs(t, p.walkOrder(7, keys), 2, 3, 1)
}

// A pool whose every key is cooling must still walk them all: the bench
// reorders, it never idles a provider out of rotation on recorded state
// alone.
func TestWalkOrderAllCoolingStillReturnsAll(t *testing.T) {
	p, _ := fakePool(t)
	keys := keysWith(1, 2)
	p.coolKey(1, 0, p.stamp(), 5*time.Second)
	p.coolKey(2, 0, p.stamp(), 10*time.Second)
	assertIDs(t, p.walkOrder(7, keys), 1, 2)
}

// A bench that has elapsed is gone: the key is ready again, and the expired
// entry is pruned rather than left to accumulate.
func TestWalkOrderExpiredBenchIsReadyAgain(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	p.coolKey(1, 0, p.stamp(), 5*time.Second)
	advance(6 * time.Second)
	assertIDs(t, p.walkOrder(7, keys), 1, 2)
	if p.states[1].benched() {
		t.Fatalf("expired bench not pruned: %+v", p.states[1])
	}
}

// The boundary is "until strictly after now": a bench expiring exactly at
// the current instant does not bench.
func TestWalkOrderBenchExpiresInclusive(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	p.coolKey(1, 0, p.stamp(), 5*time.Second)
	advance(5 * time.Second)
	assertIDs(t, p.walkOrder(7, keys), 1, 2)
}

// A fresh 429 replaces an older bench wholesale — including with a shorter
// window, because the newer Retry-After is the newer statement of when the
// upstream will serve again.
func TestCoolKeyLatestVerdictWins(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	p.coolKey(1, 0, p.stamp(), 10*time.Second)
	advance(5 * time.Second)
	p.coolKey(1, 0, p.stamp(), 2*time.Second) // re-benched at t=5 until t=7
	advance(1 * time.Second)
	assertIDs(t, p.walkOrder(7, keys), 2, 1) // still benched at t=6
	advance(1 * time.Second)
	// Fresh provider ID: cursor 0, so a released key 1 walks first — the
	// t=7 walk above already rotated provider 7's cursor and would mask it.
	assertIDs(t, p.walkOrder(8, keys), 1, 2) // released exactly at expiry
}

// 429 verdicts are ordered by dispatch, not arrival: an older request's
// delayed 429 must not replace — in particular not shorten — the bench a
// later-dispatched request installed.
func TestCoolKeyOlderDispatchCannotReplaceBench(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	staleDispatch := time.Unix(0, 0) // the older request goes out at t=0
	advance(5 * time.Second)
	p.coolKey(1, 0, p.stamp(), time.Hour)       // newer request's bench at t=5
	p.coolKey(1, 0, staleDispatch, time.Second) // the older 429 lands late
	advance(2 * time.Second)                    // past the short window it stated
	assertIDs(t, p.walkOrder(7, keys), 2, 1)    // the hour-long bench stands
}

// The 429 dispatch watermark outlives its bench: a short window can expire
// and be pruned before an older request's delayed 429 arrives, and that
// verdict — already superseded while the bench stood — must not become
// acceptable again just because the bench is gone.
func TestCoolKeyOlderDispatchRefusedAfterBenchExpiry(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	staleDispatch := time.Unix(0, 0) // the older request goes out at t=0
	advance(5 * time.Second)
	p.coolKey(1, 0, p.stamp(), time.Second) // newer 429: benched t=5→6
	advance(2 * time.Second)
	assertIDs(t, p.walkOrder(7, keys), 1, 2)  // expired, pruned
	p.coolKey(1, 0, staleDispatch, time.Hour) // the older 429 lands after expiry
	if p.states[1].benched() {
		t.Fatal("a 429 superseded before the bench expired re-benched the key after it")
	}
}

func TestClearKeyReleasesTheBench(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	p.coolKey(1, 0, p.stamp(), time.Hour) // bench installed at t=0
	advance(1 * time.Second)
	p.clearKey(1, 0, time.Unix(1, 0)) // dispatched at t=1, after the bench
	assertIDs(t, p.walkOrder(7, keys), 1, 2)
}

// A success only proves the key was healthy when its request was DISPATCHED.
// A long stream accepted before another request's 429 finishes after the
// bench is booked; its dispatch predates the bench, so it must not release
// it.
func TestClearKeyStaleDispatchCannotRelease(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	dispatched := time.Unix(0, 0) // the stream's dispatch instant
	advance(5 * time.Second)
	p.coolKey(1, 0, p.stamp(), time.Hour) // bench installed at t=5, after the dispatch
	p.clearKey(1, 0, dispatched)
	assertIDs(t, p.walkOrder(7, keys), 2, 1) // bench survives
}

// dropKey releases a bench whose 429 did not postdate the invalidation —
// including one earned at the very same instant, unlike a success, which
// must be strictly later than the booking. The key is leaving rotation via
// the persistent retest path, and the bench must not outlive it and demote
// the key after a successful retest.
func TestDropKeyReleasesPriorBench(t *testing.T) {
	p, _ := fakePool(t)
	keys := keysWith(1, 2)
	p.coolKey(1, 0, p.stamp(), time.Hour)
	p.dropKey(1, 0, p.stamp())
	assertIDs(t, p.walkOrder(7, keys), 1, 2)
}

// A dropKey that runs LATE — its goroutine stalled past a retest that
// brought the key back, and a fresh 429 has already benched it — counts
// against its observation time, not its execution time: the fresh bench
// postdates the invalidation and must survive it.
func TestDropKeyLateInvalidationCannotWipeNewerBench(t *testing.T) {
	p, advance := fakePool(t)
	observed := p.stamp() // the invalidating response arrived at t=0
	advance(5 * time.Second)
	p.coolKey(1, 0, p.stamp(), time.Hour) // fresh bench, dispatched t=5
	p.dropKey(1, 0, observed)             // the delayed drop finally runs
	if !p.states[1].benched() {
		t.Fatal("late invalidation wiped a bench born after it was observed")
	}
}

// Nor may a late dropKey regress the invalidation mark a newer one already
// set: the t=5 mark must keep refusing the t=3 429 after the stale t=0
// invalidation lands.
func TestDropKeyLateMarkCannotRegressNewerMark(t *testing.T) {
	p, advance := fakePool(t)
	early := p.stamp() // t=0
	advance(5 * time.Second)
	p.dropKey(1, 0, p.stamp()) // invalidation observed at t=5
	p.dropKey(1, 0, early)     // an older invalidation lands late
	p.coolKey(1, 0, time.Unix(3, 0), time.Hour)
	if p.states[1].benched() {
		t.Fatal("regressed mark let a pre-invalidation 429 bench the key")
	}
}

// A newer-generation bench belongs to a replacement credential the
// invalidating caller never saw: dropKey must leave it alone.
func TestDropKeyOlderGenerationCannotRelease(t *testing.T) {
	p, _ := fakePool(t)
	p.coolKey(1, 1, p.stamp(), time.Hour)
	p.dropKey(1, 0, p.stamp())
	keys := []model.ProviderKey{{ID: 1, ConfigVersion: 1}, {ID: 2}}
	assertIDs(t, p.walkOrder(7, keys), 2, 1)
}

// A 429 whose request was dispatched before a success that later proved the
// key healthy must not re-bench it: request A goes out, another request's
// 429 benches the key, a success dispatched after that bench releases it —
// then A's delayed 429 lands. The recovery is the newer evidence.
func TestCoolKeyDispatchBeforeRecoveryCannotBench(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	staleDispatch := time.Unix(0, 0) // request A goes out at t=0
	advance(2 * time.Second)
	p.coolKey(1, 0, p.stamp(), time.Hour) // another request's 429 benches at t=2
	advance(2 * time.Second)
	p.clearKey(1, 0, time.Unix(4, 0))         // success dispatched at t=4 releases it
	p.coolKey(1, 0, staleDispatch, time.Hour) // A's delayed 429 lands last
	assertIDs(t, p.walkOrder(7, keys), 1, 2)  // no bench re-installed
}

// A 429 dispatched after the latest success is fresh evidence and benches
// normally — the watermark blocks only verdicts the recovery already
// outdated.
func TestCoolKeyDispatchAfterRecoveryBenches(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	p.coolKey(1, 0, p.stamp(), time.Hour) // benched at t=0
	advance(2 * time.Second)
	p.clearKey(1, 0, time.Unix(1, 0))           // success dispatched at t=1 releases it
	p.coolKey(1, 0, time.Unix(2, 0), time.Hour) // fresh 429 dispatched at t=2
	assertIDs(t, p.walkOrder(7, keys), 2, 1)
}

// The recovery watermark is generation-scoped: a late success from the OLD
// secret — dispatched after the replacement's request went out — proves
// nothing about the new credential and must not veto its 429.
func TestCoolKeyRecoveryWatermarkIsGenerationScoped(t *testing.T) {
	p, _ := fakePool(t)
	p.clearKey(1, 0, time.Unix(5, 0))           // old-secret success, dispatched t=5
	p.coolKey(1, 1, time.Unix(3, 0), time.Hour) // new credential's 429, dispatched t=3
	keys := []model.ProviderKey{{ID: 1, ConfigVersion: 1}, {ID: 2}}
	assertIDs(t, p.walkOrder(7, keys), 2, 1) // the 429 benched the new credential
}

// The invalidation mark is generation-scoped the same way: a mark recorded
// for the old secret must not swallow the replacement's 429s.
func TestCoolKeyInvalidationWatermarkIsGenerationScoped(t *testing.T) {
	p, advance := fakePool(t)
	advance(5 * time.Second)
	p.dropKey(1, 0, p.stamp())                  // old-secret invalidation at t=5
	p.coolKey(1, 1, time.Unix(3, 0), time.Hour) // new credential's 429, dispatched t=3
	keys := []model.ProviderKey{{ID: 1, ConfigVersion: 1}, {ID: 2}}
	assertIDs(t, p.walkOrder(7, keys), 2, 1)
}

// An old-secret success must not overwrite the replacement's own recovery
// watermark: with the watermark gone, a stale 429 about the replacement
// could re-bench it despite its newer success.
func TestClearKeyOlderGenerationCannotRegressWatermark(t *testing.T) {
	p, _ := fakePool(t)
	p.clearKey(1, 1, time.Unix(10, 0))          // replacement's success, dispatched t=10
	p.clearKey(1, 0, time.Unix(20, 0))          // late old-secret success, dispatched t=20
	p.coolKey(1, 1, time.Unix(9, 0), time.Hour) // stale replacement 429, dispatched t=9
	keys := []model.ProviderKey{{ID: 1, ConfigVersion: 1}, {ID: 2}}
	assertIDs(t, p.walkOrder(7, keys), 1, 2) // still refused by the replacement's watermark
}

// A 429 whose request was dispatched before the key's persistent
// invalidation must not re-install the bench the invalidation dropped:
// retest keeps ConfigVersion, so the resurrected bench would still match
// after a successful retest and demote the recovered key until expiry.
func TestCoolKeyDispatchBeforeInvalidationCannotBench(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	dispatched := time.Unix(0, 0) // in flight when the invalidation lands
	advance(5 * time.Second)
	p.dropKey(1, 0, p.stamp()) // persistent invalidation at t=5
	p.coolKey(1, 0, dispatched, time.Hour)
	assertIDs(t, p.walkOrder(7, keys), 1, 2) // no bench installed
}

// A 429 dispatched after the invalidation is a fresh verdict — for example
// from a request that fetched the key between the upstream verdict and the
// DB write taking it out of rotation — and benches normally.
func TestCoolKeyDispatchAfterInvalidationBenches(t *testing.T) {
	p, advance := fakePool(t)
	keys := keysWith(1, 2)
	p.dropKey(1, 0, p.stamp()) // invalidation at t=0
	advance(5 * time.Second)
	p.coolKey(1, 0, time.Unix(1, 0), time.Hour) // dispatched at t=1, after it
	assertIDs(t, p.walkOrder(7, keys), 2, 1)
}

// The generation rule itself: a newer-generation verdict replaces the key's
// record WHOLESALE. Facts about the old secret — its bench, its recovery,
// its invalidation — do not carry over to the credential that superseded
// it, in either direction: they must not veto the new credential's verdicts
// (pinned here) and must not be vetoed by them (pinned by the
// *GenerationScoped and *OlderGeneration* tests around this one).
func TestNewerGenerationResetsRecord(t *testing.T) {
	p, advance := fakePool(t)
	// The old secret accumulates every kind of fact: a bench, an
	// invalidation, and a recovery far in the future dispatch-wise.
	p.coolKey(1, 0, p.stamp(), time.Hour)
	p.dropKey(1, 0, p.stamp())
	p.clearKey(1, 0, time.Unix(100, 0))
	advance(1 * time.Second)
	// The new credential's first verdict is a 429 dispatched at t=1. Were
	// any old-generation fact carried over — the recovery mark at t=100
	// most of all — it would refuse this bench.
	p.coolKey(1, 1, p.stamp(), time.Hour)
	keys := []model.ProviderKey{{ID: 1, ConfigVersion: 1}, {ID: 2}}
	assertIDs(t, p.walkOrder(7, keys), 2, 1) // benched: nothing leaked across
}

// Replacing a key's secret keeps its row ID but bumps ConfigVersion. The
// three tests below pin the generation boundary: benches and verdicts from
// a superseded credential must not touch the replacement's record.

// A bench earned by the old secret does not demote its replacement: the walk
// sees the new generation, prunes the stale record, and the key starts
// healthy.
func TestWalkOrderStaleGenerationBenchPruned(t *testing.T) {
	p, _ := fakePool(t)
	p.coolKey(1, 0, p.stamp(), time.Hour) // benched as generation 0
	keys := []model.ProviderKey{{ID: 1, ConfigVersion: 1}, {ID: 2}}
	assertIDs(t, p.walkOrder(7, keys), 1, 2)
	if p.states[1].benched() {
		t.Fatalf("stale-generation bench not pruned: %+v", p.states[1])
	}
}

// A successful retest opens a new credential era even though ConfigVersion
// is unchanged: a 429 from a request dispatched before the retest carries
// the old TestGeneration, and NoteKeyRetestPassed — the proven-recovery
// signal from the provider service's commit path — must both release the
// bench and refuse that straggler: a key that just re-proved itself must
// not stay demoted by evidence older than the proof.
func TestRetestProofReleasesBenchAndRefusesStale429(t *testing.T) {
	p, advance := fakePool(t)
	svc := &Service{keyPool: p}
	keys := keysWith(1, 2)
	staleDispatch := time.Unix(0, 0)      // in flight when the retest runs
	p.coolKey(1, 0, p.stamp(), time.Hour) // benched at t=0
	advance(5 * time.Second)
	svc.NoteKeyRetestPassed(1, 0, time.Unix(5, 0)) // proof observed at t=5
	assertIDs(t, p.walkOrder(7, keys), 1, 2)
	p.coolKey(1, 0, staleDispatch, time.Hour) // the straggler 429 lands last
	if p.states[1].benched() {
		t.Fatal("a 429 from before the proven retest re-benched the recovered key")
	}
}

// The proof carries its own observation time, stamped before the probe ran
// — NOT the callback's execution time. A 429 that benches the key in the
// gap between the probe and the delayed callback is newer evidence, and
// the late proof must not release it.
func TestRetestProofObservedBeforeFreshBenchCannotRelease(t *testing.T) {
	p, advance := fakePool(t)
	svc := &Service{keyPool: p}
	observed := p.stamp() // the probe is observed at t=0
	advance(5 * time.Second)
	p.coolKey(1, 0, p.stamp(), time.Hour) // fresh 429 benches at t=5
	advance(2 * time.Second)
	svc.NoteKeyRetestPassed(1, 0, observed) // the callback lands at t=7
	if !p.states[1].benched() {
		t.Fatal("a delayed retest proof released a bench newer than itself")
	}
}

// The pruning right belongs to the walk only when ITS key is the newer
// truth: a walk from a key list loaded before a replacement must not wipe
// the bench the replacement credential earned for itself.
func TestWalkOrderStaleWalkCannotPruneNewerBench(t *testing.T) {
	p, _ := fakePool(t)
	p.coolKey(1, 1, p.stamp(), time.Hour) // the replacement's own bench
	stale := []model.ProviderKey{{ID: 1, ConfigVersion: 0}, {ID: 2}}
	assertIDs(t, p.walkOrder(7, stale), 1, 2) // the bench cannot speak for the old key
	if !p.states[1].benched() {
		t.Fatal("a stale-generation walk pruned the newer credential's bench")
	}
}

// A late 429 from a request sent with the old secret must not bench the
// replacement — not even by overwriting a bench the replacement earned
// itself.
func TestCoolKeyOlderGenerationCannotOverwrite(t *testing.T) {
	p, _ := fakePool(t)
	p.coolKey(1, 1, p.stamp(), time.Hour)   // the replacement's own bench
	p.coolKey(1, 0, p.stamp(), time.Minute) // late verdict about the old secret
	keys := []model.ProviderKey{{ID: 1, ConfigVersion: 1}, {ID: 2}}
	assertIDs(t, p.walkOrder(7, keys), 2, 1) // replacement's bench intact
}

// A late success from the old secret proves nothing about the replacement:
// it must not release the replacement's bench.
func TestClearKeyOlderGenerationCannotRelease(t *testing.T) {
	p, _ := fakePool(t)
	p.coolKey(1, 1, p.stamp(), time.Hour)
	p.clearKey(1, 0, time.Unix(10, 0)) // late success from the old secret
	keys := []model.ProviderKey{{ID: 1, ConfigVersion: 1}, {ID: 2}}
	assertIDs(t, p.walkOrder(7, keys), 2, 1)
}

// A nil pool is the unwired-pool case: priority order, no rotation, no
// benching, and above all no panic.
func TestNilPoolWalksPriorityOrder(t *testing.T) {
	var p *keyPool
	keys := keysWith(1, 2, 3)
	assertIDs(t, p.walkOrder(7, keys), 1, 2, 3)
	assertIDs(t, p.walkOrder(7, keys), 1, 2, 3)
	p.coolKey(1, 0, p.stamp(), time.Hour) // must not panic
	p.clearKey(1, 0, p.stamp())           // must not panic; nil stamp is zero time
	p.dropKey(1, 0, p.stamp())            // must not panic
}

// Single-key pools have nothing to rotate and come back untouched — and an
// empty pool must come back untouched without reaching the cursor's modulo,
// which would divide by zero.
func TestWalkOrderSingleKeyAndEmptyUnchanged(t *testing.T) {
	p, _ := fakePool(t)
	keys := keysWith(1)
	assertIDs(t, p.walkOrder(7, keys), 1)
	if got := p.walkOrder(7, nil); len(got) != 0 {
		t.Errorf("empty walk = %v, want empty", idsOf(got))
	}
}

// walkOrder must not reorder the caller's slice: the repository's
// sort_order lives in it, and later rotations read it again.
func TestWalkOrderDoesNotMutateInput(t *testing.T) {
	p, _ := fakePool(t)
	keys := keysWith(1, 2, 3)
	p.coolKey(2, 0, p.stamp(), time.Hour)
	_ = p.walkOrder(7, keys)
	assertIDs(t, keys, 1, 2, 3)
}

// The cursor survives pool-size changes by construction — it is taken
// modulo the CURRENT length — so a pool that grows or shrinks between walks
// keeps rotating without resetting, and single-key walks still advance it:
// a pool that later gains keys is already part of the rotation.
func TestWalkOrderModuloSurvivesPoolSizeChanges(t *testing.T) {
	p, _ := fakePool(t)
	assertIDs(t, p.walkOrder(7, keysWith(1, 2)), 1, 2)       // cursor 0→1
	assertIDs(t, p.walkOrder(7, keysWith(1, 2)), 2, 1)       // 1→2
	assertIDs(t, p.walkOrder(7, keysWith(1)), 1)             // 2→3, single-key walk still advances
	assertIDs(t, p.walkOrder(7, keysWith(1, 2, 3)), 1, 2, 3) // grew: start = 3 % 3 = 0
	assertIDs(t, p.walkOrder(7, keysWith(1, 2, 3)), 2, 3, 1) // 3→4: start = 4 % 3 = 1
	assertIDs(t, p.walkOrder(7, keysWith(1, 2)), 2, 1)       // shrunk: start = 5 % 2 = 1
}

// The clamp contract: both standard Retry-After forms honoured within
// [1s, 10min] — a zero, negative, or already-elapsed window buys the floor,
// since the upstream did state a window — while anything unparsable falls
// back.
func TestCooldownFromRetryAfter(t *testing.T) {
	const fallback = 30 * time.Second
	now := time.Date(2015, 10, 21, 7, 28, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"5", 5 * time.Second},
		{"1", 1 * time.Second},
		{" 7 ", 7 * time.Second},          // surrounding whitespace tolerated
		{"0", time.Second},                // "retry immediately" still buys the floor
		{"-3", time.Second},               // nonsense, but a stated window: floored
		{"-10000000000", time.Second},     // would overflow negative if multiplied first
		{"1200", 10 * time.Minute},        // ceiling
		{"99999", 10 * time.Minute},       // absurd demands are capped
		{"10000000000", 10 * time.Minute}, // would overflow time.Duration if multiplied first
		{"", fallback},
		{"abc", fallback},
		{"Wed, 21 Oct 2015 07:29:30 GMT", 90 * time.Second}, // HTTP-date, 90s ahead of now
		{"Wed, 21 Oct 2015 08:28:00 GMT", 10 * time.Minute}, // HTTP-date an hour out: capped
		{"Wed, 21 Oct 2015 07:28:00 GMT", time.Second},      // HTTP-date exactly now: floored
		{"Tue, 20 Oct 2015 07:28:00 GMT", time.Second},      // HTTP-date in the past: floored
		{"Wed, 21 Oct 20155 07:28:00 GMT", fallback},        // malformed date falls back
	}
	for _, tc := range cases {
		if got := cooldownFromRetryAfter(tc.header, now, fallback); got != tc.want {
			t.Errorf("cooldownFromRetryAfter(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// The pool map must stay bounded by the keys that actually exist: entries
// for deleted keys are pruned the next time their slot in a walk is reached
// only if present in the walked slice — a stale entry for a removed key
// lingers harmlessly and is not worth a sweeper. This pins that decision:
// an entry whose key no longer appears in any walk is simply never read.
func TestStaleEntryForRemovedKeyIsIgnored(t *testing.T) {
	p, _ := fakePool(t)
	p.coolKey(42, 0, p.stamp(), time.Hour) // key 42 is in no walked pool
	assertIDs(t, p.walkOrder(7, keysWith(1, 2)), 1, 2)
}
