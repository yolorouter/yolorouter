package gateway

// keyPool is the rotation cursor and rate-limit bench for a provider's keys:
// a per-provider cursor that spreads load across the pool, and a per-key
// cooldown record fed by plain 429s. It decides only the ORDER tryKeys walks
// a provider's keys in — never whether a key dispatches, which stays the
// decision table's call.
//
// Like the circuit breaker, the record lives in process memory on purpose:
// this build is a single binary with nobody to share it, and losing it on
// restart merely means the rotation and benches start fresh. A cooldown is a
// DEMOTION, not an exclusion — a benched key walks behind healthy ones, and a
// pool whose every key is cooling still dispatches (soonest-to-recover
// first), so no amount of recorded state can idle a provider out of rotation
// on its own.
//
// A nil *keyPool is a pool with no memory: every method is nil-safe and
// degrades to "walk in sort_order, bench nothing", so a hand-assembled
// Service that never wired one simply runs with priority order instead of
// panicking on the hot path.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
)

// The bench a Retry-After header can buy is clamped at both ends: below the
// floor it cannot separate two requests and only adds map churn; above the
// ceiling a broken or hostile upstream could bench a healthy key for hours.
const (
	retryAfterFloor   = time.Second
	retryAfterCeiling = 10 * time.Minute
)

// keyState is everything the pool remembers about one key, and all of it is
// about ONE credential generation — the newest whose verdict the pool has
// seen. Replacing a key's secret keeps the row ID but bumps ConfigVersion,
// and evidence about the old secret says nothing about its replacement, so
// the generation rule is applied once, in stateFor, before any verdict
// touches these fields: an older-generation verdict is discarded, a
// newer-generation one resets the record wholesale.
//
// Within the generation, verdicts are ordered by WHEN THEY WERE OBSERVED,
// not when they arrived — concurrent requests on one key finish out of
// order. Each field records the moment its fact took effect, and a verdict
// may only alter state it is strictly newer than:
//
//   - a 429 (dispatched at its request's send time) benches only if that
//     dispatch postdates the standing bench's 429, the latest success, and
//     the last invalidation;
//   - a success (also dispatch-stamped) releases the bench only if
//     dispatched after the bench was BOOKED — a stream accepted before the
//     429 proves the key was healthy before the limit hit, not after;
//   - an invalidation (arrival-stamped; its CAS already proved the
//     generation current) wipes the bench unconditionally.
//
// A record whose facts have all expired or been superseded lingers in the
// map — like a stale entry for a deleted key, state nothing matches against
// is harmless and not worth a sweeper.
type keyState struct {
	// cfg is the ConfigVersion these facts are about. TestGeneration is
	// deliberately NOT part of the era: it advances when a retest is
	// CLAIMED, before the probe runs, and an inconclusive probe (rate
	// limit, timeout) proves nothing — treating the claim as a new era
	// would drop a still-valid bench. Proven recovery arrives instead as an
	// explicit clearKey from the retest-passed listener (NoteKeyRetestPassed).
	cfg int
	// The bench: zero benchUntil = healthy. benchInstalled is booking time
	// (pool clock).
	benchUntil     time.Time
	benchInstalled time.Time
	// limitedAt is the dispatch time of the newest 429 applied for this
	// generation. It deliberately OUTLIVES the bench it booked: a short
	// window can expire and be pruned before an older request's delayed
	// 429 arrives, and that verdict — already superseded while the bench
	// stood — must not become acceptable again just because it stood no
	// longer.
	limitedAt time.Time
	// invalidatedAt is when the key last left rotation through the
	// persistent retest path; retest keeps ConfigVersion, so without it a
	// straggler 429 could re-bench the key for after it recovers.
	invalidatedAt time.Time
	// recoveredAt is the dispatch time of the key's latest success.
	recoveredAt time.Time
}

func (s *keyState) benched() bool { return !s.benchUntil.IsZero() }

// dropBench releases the bench but keeps limitedAt: the 429 that earned the
// bench stays the newest rate-limit verdict on record, and older delayed
// ones stay refused.
func (s *keyState) dropBench() {
	s.benchUntil, s.benchInstalled = time.Time{}, time.Time{}
}

type keyPool struct {
	mu      sync.Mutex
	cursors map[uint]uint64    // provider ID → rotation cursor
	states  map[uint]*keyState // key ID → newest-generation record
	now     func() time.Time   // injectable clock, for tests
}

// stateFor positions the key's record at generation gen — the single place
// the generation rule lives. It returns nil for an older-generation verdict
// (discard: evidence about a superseded secret) and a wholesale-reset
// record for a newer one (nothing about the old secret carries over).
// Callers hold p.mu.
func (p *keyPool) stateFor(keyID uint, cfg int) *keyState {
	s, ok := p.states[keyID]
	if !ok {
		s = &keyState{cfg: cfg}
		p.states[keyID] = s
		return s
	}
	if cfg < s.cfg {
		return nil
	}
	if cfg > s.cfg {
		*s = keyState{cfg: cfg}
	}
	return s
}

func newKeyPool(now func() time.Time) *keyPool {
	if now == nil {
		now = time.Now
	}
	return &keyPool{
		cursors: make(map[uint]uint64),
		states:  make(map[uint]*keyState),
		now:     now,
	}
}

// walkOrder returns the order to walk this provider's keys in: rotated by
// the provider's cursor so consecutive requests start on different keys, then
// partitioned so benched keys trail healthy ones, earliest-to-recover first.
// The input order (sort_order from the repository) is the tiebreaker within
// each group, and a pool with fewer than two keys is returned untouched —
// there is nothing to rotate.
func (p *keyPool) walkOrder(providerID uint, keys []model.ProviderKey) []model.ProviderKey {
	// Empty comes back untouched too — before the cursor arithmetic, whose
	// modulo would divide by zero. Today's only caller filters empties
	// first; the method's own contract must not depend on that.
	if p == nil || len(keys) == 0 {
		return keys
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// The cursor advances on every call — single-key pools included — so a
	// pool that later gains keys is already part of the rotation (mod 1 is
	// always 0; the single-key walk itself never reorders).
	cur := p.cursors[providerID]
	p.cursors[providerID]++
	if len(keys) < 2 {
		return keys
	}

	now := p.now()
	// Partition FIRST, in the caller's sort_order. Rotating before
	// partitioning would let the cursor land on a benched key, which then
	// tails and hands its turn to whichever healthy key follows it — with
	// [1,2,3] and 1 cooling, consecutive walks would start 2,2,3, serving
	// one healthy key double traffic exactly when part of the pool is
	// already limited.
	ready := make([]model.ProviderKey, 0, len(keys))
	// benched carries the expiry alongside the key so the tail can be sorted
	// soonest-first without a second lookup per comparison.
	type benched struct {
		key    model.ProviderKey
		expiry time.Time
	}
	var tail []benched
	for _, k := range keys {
		s, ok := p.states[k.ID]
		switch {
		case !ok || !s.benched():
		case s.cfg != k.ConfigVersion:
			// The bench belongs to a different credential generation than
			// the walked key. Pruned only when the walked key is the NEWER
			// truth — a stale walk, from a key list loaded before a
			// replacement, must not wipe the replacement's record. Either
			// way the bench cannot speak for this walk's key: it walks
			// healthy.
			if s.cfg < k.ConfigVersion {
				s.dropBench()
			}
		case s.benchUntil.After(now):
			tail = append(tail, benched{key: k, expiry: s.benchUntil})
			continue
		default:
			// Expired: lazily pruned rather than left to accumulate.
			s.dropBench()
		}
		ready = append(ready, k)
	}

	// The cursor rotates the healthy subset only, modulo its CURRENT size —
	// the same "survives size changes" arithmetic the whole-pool rotation
	// uses when keys come and go.
	order := make([]model.ProviderKey, 0, len(keys))
	if len(ready) > 0 {
		start := int(cur % uint64(len(ready)))
		order = append(order, ready[start:]...)
		order = append(order, ready[:start]...)
	}
	if len(tail) == 0 {
		return order
	}
	sort.SliceStable(tail, func(i, j int) bool { return tail[i].expiry.Before(tail[j].expiry) })
	for _, b := range tail {
		order = append(order, b.key)
	}
	return order
}

// coolKey benches a key for d, on behalf of a 429 from credential
// generation gen, dispatched at dispatchedAt. Ordering rules — which older
// verdicts it loses to, and why a later-dispatched 429 with a shorter
// Retry-After may still shorten the bench — live on keyState.
func (p *keyPool) coolKey(keyID uint, cfg int, dispatchedAt time.Time, d time.Duration) {
	if p == nil || d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stateFor(keyID, cfg)
	if s == nil {
		return
	}
	// The verdict benches only if its dispatch is strictly newer than every
	// fact standing against it: the last invalidation, the latest success
	// (either proves the key's standing AFTER this 429's request went out),
	// and the newest 429 already applied — an older request's delayed
	// verdict replacing a newer bench would shorten it and send traffic
	// back early, and one arriving after that bench expired was superseded
	// all the same.
	if !dispatchedAt.After(s.invalidatedAt) ||
		!dispatchedAt.After(s.recoveredAt) ||
		!dispatchedAt.After(s.limitedAt) {
		return
	}
	now := p.now()
	s.benchUntil, s.benchInstalled, s.limitedAt = now.Add(d), now, dispatchedAt
}

// dropKey books a persistent invalidation (quota 429, 401 — the retest
// path) observed at observedAt: it releases the bench and records the
// invalidation mark, both ordering-gated like every other verdict. Retest
// keeps the row's ConfigVersion, so without the release a bench booked
// before the invalidation would still match after a successful retest and
// keep the recovered key demoted until expiry; and without the mark a 429
// still in flight from before the invalidation could re-install it.
//
// observedAt is when the invalidating response arrived, stamped by the
// caller BEFORE its own slow work (the DB write): a dropKey that runs late
// — after a retest already brought the key back and fresh verdicts landed —
// must count against the moment the invalidation was seen, not the moment
// this call finally executes, or it would wipe a bench born after the
// recovery and outdate 429s it never preceded.
func (p *keyPool) dropKey(keyID uint, cfg int, observedAt time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stateFor(keyID, cfg)
	if s == nil {
		return
	}
	if observedAt.After(s.invalidatedAt) {
		s.invalidatedAt = observedAt
	}
	if s.benched() && !s.limitedAt.After(observedAt) {
		s.dropBench()
	}
}

// stamp reads the pool's clock, for callers that must later prove their
// dispatch predates or postdates a bench. Nil-safe: a pool with no memory
// hands out the zero time, which can never postdate a bench it also cannot
// hold.
func (p *keyPool) stamp() time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.now()
}

// clearKey releases a key's bench, on behalf of a success from credential
// generation gen, dispatched at dispatchedAt. Booked on the
// healthy-interaction ledger (the CircuitReset seam): most visibly the key
// an all-cooling pool dispatched anyway — leaving its bench booked would
// keep demoting the very key that just served. Which stale successes
// release nothing is keyState's ordering contract.
func (p *keyPool) clearKey(keyID uint, cfg int, dispatchedAt time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stateFor(keyID, cfg)
	if s == nil {
		return
	}
	// Every success advances the recovery mark, bench or no bench: coolKey
	// compares 429 dispatch times against it so a verdict older than the
	// latest proven-healthy dispatch cannot re-bench the key.
	if dispatchedAt.After(s.recoveredAt) {
		s.recoveredAt = dispatchedAt
	}
	if s.benched() && dispatchedAt.After(s.benchInstalled) {
		s.dropBench()
	}
}

// cooldownFromRetryAfter reads a Retry-After header in either standard form
// — delta-seconds or an HTTP-date, measured against now — floored at
// retryAfterFloor and capped at retryAfterCeiling. A zero, negative, or
// already-elapsed window buys the floor rather than the fallback, because
// the upstream DID state a window, however nonsensically. Anything
// unparsable falls back to the configured default rather than guessing.
func cooldownFromRetryAfter(header string, now time.Time, fallback time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if n, err := strconv.Atoi(header); err == nil {
		// Clamp in integer seconds before multiplying: a huge Atoi-valid
		// value in either direction would overflow time.Duration and slip
		// past both bounds.
		if n < 1 {
			return retryAfterFloor
		}
		if n > int(retryAfterCeiling/time.Second) {
			return retryAfterCeiling
		}
		return time.Duration(n) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		// Time.Sub saturates instead of overflowing, so clamping the result
		// is safe even for absurd dates.
		d := at.Sub(now)
		if d < retryAfterFloor {
			return retryAfterFloor
		}
		if d > retryAfterCeiling {
			return retryAfterCeiling
		}
		return d
	}
	return fallback
}
