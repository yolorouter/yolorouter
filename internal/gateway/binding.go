package gateway

// binding.go is the sticky-binding registry behind a balanced model's
// candidate ordering: caller API keys are assigned to one provider each and
// keep hitting it, so upstream prompt caches stay warm per key while the
// assignment itself spreads keys evenly across providers.
//
// Like the key pool and the circuit breaker, the registry lives in process
// memory on purpose: this build is a single binary with nobody to share it,
// and losing it on restart merely means every key is assigned again — which
// reproduces the same even spread within a handful of requests. It decides
// only WHICH candidate a balanced chain starts from; whether that candidate
// dispatches, and what happens when it fails, is unchanged gateway machinery.

import (
	"sync"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
)

// bindingCapacity bounds the registry. Each entry is tens of bytes and one
// row per (caller key, model) pair actually seen, so even a busy gateway
// sits far below this; the bound exists so a pathological pattern (millions
// of one-shot keys against one model) cannot grow the map without limit.
// At capacity the least-recently-used binding is evicted — its key simply
// gets reassigned on its next request, which is also how the spread heals
// after a restart.
const bindingCapacity = 4096

// deadEndCooldown is how long a provider stays out of NEW assignments after
// a walk proved it a pre-dispatch dead end (no usable key, undecryptable
// key, provider row gone). Moving the discovering caller's own binding is
// not enough: the dead end then sits at zero bindings, and the least-bound
// choice would feed it every subsequent new key, each burning a probe to
// rediscover the same fact. A plain time bound, deliberately, on both ends:
// no recovery event lifts it early — the states that end a dead end (a key
// enabled, a retest passed, a destination repaired) have many write paths,
// and missing one would quarantine the provider until restart — and no
// admission token meters the retries after expiry: requests that assign to
// the provider in the instant after the window lapses each burn one probe
// and re-mark it within milliseconds if it is still dead, a bounded,
// self-limiting cost that no bookkeeping has to get right. While the window
// holds, a still-routable chain sends the provider no assignment traffic at
// all.
const deadEndCooldown = 30 * time.Second

// bindingKey identifies one caller's stake in one model's chain. Bindings
// are per (key, model) by construction: the candidate chain belongs to the
// model, so the same key calling two models has two independent assignments.
type bindingKey struct {
	apiKeyID uint
	modelID  uint
}

// bindingCountKey aggregates bindings for the least-bound-first choice.
// Counting per (model, provider) rather than globally keeps the numbers
// meaningful when one provider serves many models with very different
// fan-out.
type bindingCountKey struct {
	modelID    uint
	providerID uint
}

// bindingTarget is the provider a key is stuck to, plus the bookkeeping the
// LRU needs. candidateID is stored alongside providerID so a candidate that
// was deleted and recreated for the same provider is treated as a DIFFERENT
// target — the mapping the binding was made against no longer exists, and
// silently re-pointing it would hide that from the admin's view of the
// distribution.
type bindingTarget struct {
	providerID  uint
	candidateID uint
	lastUsed    time.Time
}

// BindingRegistry maps caller keys to providers for balanced models and
// nothing else. All methods are nil-safe: a registry that was never wired
// (hand-assembled services, embedders) reads as "no bindings", which makes
// balanced ordering degrade to plain sort_order rather than panic on the
// hot path.
type BindingRegistry struct {
	mu       sync.Mutex
	bindings map[bindingKey]*bindingTarget
	counts   map[bindingCountKey]int
	// deadEndUntil quarantines providers a walk proved to be pre-dispatch
	// dead ends: no NEW assignment lands on them until the window expires.
	deadEndUntil map[uint]time.Time
	now          func() time.Time // injectable clock, for tests
}

// NewBindingRegistry builds an empty registry. now may be nil, in which case
// time.Now is used.
func NewBindingRegistry(now func() time.Time) *BindingRegistry {
	if now == nil {
		now = time.Now
	}
	return &BindingRegistry{
		bindings:     make(map[bindingKey]*bindingTarget),
		counts:       make(map[bindingCountKey]int),
		deadEndUntil: make(map[uint]time.Time),
		now:          now,
	}
}

// Route answers which candidate a balanced chain for (apiKeyID, modelID)
// should start with, creating, refreshing, or reassigning the binding as the
// answer requires. candidates is the model's routable chain in sort_order
// (the caller's filtering already excluded disabled/unverified mappings);
// providerDead reports whether a provider's breaker is currently refusing
// traffic.
//
// The rules, in order:
//
//   - A binding whose candidate is still in the eligible set (present in
//     candidates AND its provider not dead) is kept and refreshed — the
//     sticky hit that keeps upstream prompt caches warm.
//   - A binding whose candidate dropped out of the chain, or whose provider
//     is dead, is released and reassigned. "Dropped out" covers every admin
//     action (disable, un-verify, delete) without the registry knowing any
//     of those states; "dead" is the breaker's open window, the one signal
//     that says this provider just kept failing.
//   - A key with no binding gets the provider with the FEWEST bindings among
//     the eligible candidates, ties broken by chain position (sort_order).
//     That is the whole balancing algorithm: new bindings always land on the
//     least-loaded provider, so the spread stays even no matter what order
//     keys arrive in, and a provider that returns from an outage — whose
//     bindings were all reassigned away — is at zero and attracts the next
//     arrivals, healing the distribution without a background rebalancer.
//
// Returns the candidateID to put first, or 0 when the caller should keep its
// own order (empty chain, or every provider dead — in which case the chain
// walk will skip them all anyway and no binding should pin a loser).
func (r *BindingRegistry) Route(apiKeyID, modelID uint, candidates []model.ModelCandidate, providerDead func(uint) bool) uint {
	if r == nil || len(candidates) == 0 || providerDead == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	eligible := make([]model.ModelCandidate, 0, len(candidates))
	eligibleIDs := make(map[uint]struct{}, len(candidates))
	for _, c := range candidates {
		if providerDead(c.ProviderID) || r.deadEndLocked(c.ProviderID) {
			continue
		}
		eligible = append(eligible, c)
		eligibleIDs[c.ID] = struct{}{}
	}
	if len(eligible) == 0 {
		// Every provider is dead. The key's binding — if any — is deliberately
		// KEPT: a transient total blackout must not cost every caller its spot
		// (and the upstream cache affinity that spot carries) when there is no
		// live provider to reassign to anyway. The admin-facing counts keep
		// showing the held bindings through the outage, which is what the
		// registry will actually resume from when providers return.
		return 0
	}

	key := bindingKey{apiKeyID: apiKeyID, modelID: modelID}
	if target, ok := r.bindings[key]; ok {
		if _, stillEligible := eligibleIDs[target.candidateID]; stillEligible {
			target.lastUsed = r.now()
			return target.candidateID
		}
		// The bound mapping is gone or its provider is dead. Release the
		// stale binding so the reassignment below starts from a clean count.
		r.releaseLocked(key)
	}

	// Evict BEFORE choosing: at capacity the eviction changes the counts,
	// and choosing first would let a tie resolve toward a provider the
	// eviction is about to make the more-bound one — quietly breaking the
	// least-bound invariant exactly when the registry is fullest.
	r.evictIfNeededLocked()

	// Least-bound-first among eligible candidates, ties by chain position.
	// The chain arrives in sort_order, so a strict < keeps the earliest
	// candidate when counts tie — the deterministic tiebreak the spread's
	// convergence guarantees lean on.
	best := eligible[0]
	bestCount := r.counts[bindingCountKey{modelID: modelID, providerID: best.ProviderID}]
	for _, c := range eligible[1:] {
		if count := r.counts[bindingCountKey{modelID: modelID, providerID: c.ProviderID}]; count < bestCount {
			best, bestCount = c, count
		}
	}
	r.bindings[key] = &bindingTarget{providerID: best.ProviderID, candidateID: best.ID, lastUsed: r.now()}
	r.counts[bindingCountKey{modelID: modelID, providerID: best.ProviderID}]++
	return best.ID
}

// Quarantine marks a provider a walk just proved to be a pre-dispatch dead
// end — no usable key, provider row missing, undecryptable key: states the
// circuit breaker never sees. For the next deadEndCooldown, Route hands the
// provider no new assignments and treats bindings stuck to it as dropped
// out; without this, a dead end sits at zero bindings and the least-bound
// choice would feed every subsequent new key into it, each burning a probe
// to rediscover the same fact. Marked for EVERY dead-end skip, not just the
// caller's bound candidate — the observation is about the provider,
// whoever happened to walk past it.
func (r *BindingRegistry) Quarantine(providerID uint) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadEndUntil[providerID] = r.now().Add(deadEndCooldown)
}

// deadEndLocked reports whether the provider is inside its dead-end window,
// forgetting expired entries as it reads. A pure read otherwise: expiry
// alone restores eligibility, and a provider that is still dead earns its
// next window from the first walk that rediscovers it. Callers hold r.mu.
func (r *BindingRegistry) deadEndLocked(providerID uint) bool {
	until, ok := r.deadEndUntil[providerID]
	if !ok {
		return false
	}
	if r.now().Before(until) {
		return true
	}
	delete(r.deadEndUntil, providerID)
	return false
}

// Rebind records the provider that actually served a request whose binding
// (to invalidatedCandidateID) was released mid-walk. Without it the released
// binding leaves the counts tied and the very next request tie-breaks
// straight back onto the dead end — one burned probe per request, forever,
// while all traffic really flows to the fallback. A concurrent request may
// have re-bound the key in the release-to-rebind window; whether that
// assignment stands depends on WHERE it landed: a different target is a
// live routing decision and wins, but the very dead end this request just
// proved unusable — re-acquired on a tie-break the release itself made
// possible — is replaced with the candidate that served.
func (r *BindingRegistry) Rebind(apiKeyID, modelID, providerID, candidateID, invalidatedCandidateID uint) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := bindingKey{apiKeyID: apiKeyID, modelID: modelID}
	if target, ok := r.bindings[key]; ok {
		if target.candidateID != invalidatedCandidateID {
			return
		}
		r.releaseLocked(key)
	}
	r.evictIfNeededLocked()
	r.bindings[key] = &bindingTarget{providerID: providerID, candidateID: candidateID, lastUsed: r.now()}
	r.counts[bindingCountKey{modelID: modelID, providerID: providerID}]++
}

// releaseLocked removes a binding and its count contribution. Callers hold
// r.mu.
func (r *BindingRegistry) releaseLocked(key bindingKey) {
	target, ok := r.bindings[key]
	if !ok {
		return
	}
	delete(r.bindings, key)
	ck := bindingCountKey{modelID: key.modelID, providerID: target.providerID}
	if r.counts[ck] <= 1 {
		delete(r.counts, ck)
	} else {
		r.counts[ck]--
	}
}

// evictIfNeededLocked drops the least-recently-used binding at capacity, so
// a flood of one-shot keys cannot grow the registry without bound. The
// evicted key is simply reassigned on its next request. A linear scan is
// deliberate: this runs once per NEW binding (sticky hits return earlier),
// the map is bounded, and anything cleverer (heap, ordered list) would spend
// more complexity than 4096 comparisons warrant.
func (r *BindingRegistry) evictIfNeededLocked() {
	if len(r.bindings) < bindingCapacity {
		return
	}
	var oldestKey bindingKey
	var oldest time.Time
	first := true
	for k, t := range r.bindings {
		if first || t.lastUsed.Before(oldest) {
			oldestKey, oldest, first = k, t.lastUsed, false
		}
	}
	if !first {
		r.releaseLocked(oldestKey)
	}
}

// BindingCounts snapshots how many caller keys are currently bound to each
// provider for one model. It is the admin-facing answer to "is the spread
// even?" — a momentary in-memory number that a restart resets, which is why
// nothing stronger than display should depend on it.
func (r *BindingRegistry) BindingCounts(modelID uint) map[uint]int {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uint]int)
	for k, n := range r.counts {
		if k.modelID == modelID {
			out[k.providerID] = n
		}
	}
	return out
}
