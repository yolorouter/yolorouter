package gateway

// Behavioural tests for the sticky-binding registry: assignment spread,
// stickiness, invalidation on death/drop, LRU eviction, and the nil-registry
// degrade. The relay-level wiring (a balanced chain actually starting at the
// bound candidate) lives in binding_relay_test.go.

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
)

func bindingTestChain(providerIDs ...uint) []model.ModelCandidate {
	// One candidate per given provider ID, in sort_order. candidate ID =
	// provider ID so "which candidate" and "which provider" stay
	// distinguishable in assertions.
	chain := make([]model.ModelCandidate, 0, len(providerIDs))
	for i, providerID := range providerIDs {
		chain = append(chain, model.ModelCandidate{ID: providerID, ProviderID: providerID, SortOrder: i + 1})
	}
	return chain
}

func noneDead(uint) bool { return false }

func deadOnly(dead uint) func(uint) bool {
	return func(p uint) bool { return p == dead }
}

func TestBindingSpreadIsEvenAcrossKeys(t *testing.T) {
	// Three providers, six caller keys: every assignment lands on the
	// least-bound provider, so the spread must come out 2/2/2 — the number
	// the balanced mode's guarantee is stated in.
	r := NewBindingRegistry(nil)
	chain := bindingTestChain(10, 11, 12)
	firsts := map[uint]int{}
	for key := uint(1); key <= 6; key++ {
		firsts[r.Route(key, 7, chain, noneDead)]++
	}
	if len(firsts) != 3 {
		t.Fatalf("assignments concentrated on %d candidates, want all 3: %v", len(firsts), firsts)
	}
	for cand, n := range firsts {
		if n != 2 {
			t.Fatalf("candidate %d got %d of 6 keys, want exactly 2", cand, n)
		}
	}
	counts := r.BindingCounts(7)
	if len(counts) != 3 {
		t.Fatalf("binding counts = %v, want three providers", counts)
	}
	for p, n := range counts {
		if n != 2 {
			t.Fatalf("provider %d count = %d, want 2", p, n)
		}
	}
}

func TestBindingSticksAcrossRequests(t *testing.T) {
	r := NewBindingRegistry(nil)
	chain := bindingTestChain(10, 11, 12)
	first := r.Route(1, 7, chain, noneDead)
	for i := 0; i < 5; i++ {
		if got := r.Route(1, 7, chain, noneDead); got != first {
			t.Fatalf("request %d routed to candidate %d, want the sticky %d", i+2, got, first)
		}
	}
	// A different key on the same model must NOT inherit the binding.
	if second := r.Route(2, 7, chain, noneDead); second == first {
		t.Fatalf("key 2 landed on the same candidate %d as key 1; assignment is not spreading", second)
	}
}

func TestBindingInvalidatedWhenProviderDies(t *testing.T) {
	r := NewBindingRegistry(nil)
	chain := bindingTestChain(10, 11, 12)
	first := r.Route(1, 7, chain, noneDead)

	// The bound provider's breaker opens: the binding must be released and
	// the key reassigned to a healthy provider, immediately — not on some
	// later timer.
	reassigned := r.Route(1, 7, chain, deadOnly(first))
	if reassigned == first {
		t.Fatalf("key stayed on dead candidate %d", reassigned)
	}
	if counts := r.BindingCounts(7); counts[first] != 0 {
		t.Fatalf("dead candidate %d still holds %d bindings after reassignment", first, counts[first])
	}
	// And it sticks to the new home.
	if again := r.Route(1, 7, chain, deadOnly(first)); again != reassigned {
		t.Fatalf("reassigned key moved again: %d then %d", reassigned, again)
	}
}

func TestBindingReassignedEvenlyWhenSharedProviderDies(t *testing.T) {
	// 10/11/12 at 2/2/2, then one provider dies: its two keys must land on
	// the two survivors one each (3/3/0 is the failure shape — both
	// refugees piling onto the same survivor).
	r := NewBindingRegistry(nil)
	chain := bindingTestChain(10, 11, 12)
	bound := map[uint]uint{} // key → candidate
	for key := uint(1); key <= 6; key++ {
		bound[key] = r.Route(key, 7, chain, noneDead)
	}
	var dead uint
	for _, cand := range bound {
		dead = cand
		break
	}
	for key := uint(1); key <= 6; key++ {
		bound[key] = r.Route(key, 7, chain, deadOnly(dead))
	}
	counts := r.BindingCounts(7)
	if counts[dead] != 0 {
		t.Fatalf("dead provider still holds %d bindings", counts[dead])
	}
	for p, n := range counts {
		if p != dead && n != 3 {
			t.Fatalf("survivor %d holds %d bindings, want 3 (even 3/3 after the death)", p, n)
		}
	}
}

func TestBindingDroppedCandidateReassigns(t *testing.T) {
	// The admin shrinks the chain: the bound mapping is gone even though its
	// provider never tripped a breaker. The binding must reassign.
	r := NewBindingRegistry(nil)
	full := bindingTestChain(10, 11, 12)
	first := r.Route(1, 7, full, noneDead)
	shrunk := []model.ModelCandidate{full[0], full[1]}
	if first == 10 || first == 11 {
		shrunk = []model.ModelCandidate{full[1], full[2]}
	}
	// Reassign against a chain that no longer contains the bound candidate.
	reassigned := r.Route(1, 7, shrunk, noneDead)
	if reassigned == first {
		t.Fatalf("binding survived its candidate leaving the chain (had %d, routed %d)", first, reassigned)
	}
}

func TestBindingAllDeadKeepsCallersOrder(t *testing.T) {
	r := NewBindingRegistry(nil)
	chain := bindingTestChain(10, 11, 12)
	r.Route(1, 7, chain, noneDead)
	if got := r.Route(1, 7, chain, func(uint) bool { return true }); got != 0 {
		t.Fatalf("all-dead chain returned candidate %d, want 0 (keep caller's order, bind nothing)", got)
	}
	// The earlier binding survives untouched — the outage may pass, and the
	// key should not lose its spot to a transient total blackout.
	if counts := r.BindingCounts(7); len(counts) == 0 {
		t.Fatalf("total blackout wiped all bindings; they should be kept for the recovery")
	}
}

// Quarantine keeps new assignments off a dead-end provider, drops bindings
// stuck to it out of the sticky check, and expires by time alone — after
// the window the provider (still at low counts) earns its retry, and a
// provider that is still dead gets re-marked by the walk that rediscovers
// it. A nil registry stays safe.
func TestBindingQuarantineLifecycle(t *testing.T) {
	cur := time.Unix(0, 0)
	r := NewBindingRegistry(func() time.Time { return cur })
	chain := bindingTestChain(10, 11, 12)
	first := r.Route(1, 7, chain, noneDead)
	r.Quarantine(first)
	if got := r.Route(1, 7, chain, noneDead); got == first {
		t.Fatalf("sticky hit survived quarantine (still %d)", got)
	}
	if got := r.Route(2, 7, chain, noneDead); got == first {
		t.Fatal("new key was assigned to a quarantined provider")
	}
	cur = cur.Add(deadEndCooldown + time.Second)
	if got := r.Route(3, 7, chain, noneDead); got != first {
		t.Fatalf("expired quarantine (zero bindings) did not attract the next key: got %d, want %d", got, first)
	}
	var nilReg *BindingRegistry
	nilReg.Quarantine(1) // must not panic
}

// Rebind replaces the caller's binding when it still points at the candidate
// the walk proved dead (the deferred-replacement norm), yields to a binding
// that moved to a different target (a concurrent routing decision), and
// fills the hole when no binding remains.
func TestBindingRebindReplacesYieldsAndFills(t *testing.T) {
	r := NewBindingRegistry(nil)
	chain := bindingTestChain(10, 11)
	first := r.Route(1, 7, chain, noneDead)
	r.Rebind(1, 7, 11, 11, first)
	counts := r.BindingCounts(7)
	if counts[11] != 1 || counts[10] != 0 {
		t.Fatalf("rebind counts = %v, want provider 11 holding the binding", counts)
	}
	if got := r.Route(1, 7, chain, noneDead); got != 11 {
		t.Fatalf("route after rebind = %d, want the rebound 11", got)
	}
	// A binding that moved to a different target stands: rebinding toward 10
	// with a stale invalidated id must be a no-op.
	r.Rebind(1, 7, 10, 10, 99)
	if got := r.Route(1, 7, chain, noneDead); got != 11 {
		t.Fatalf("rebind overwrote a live binding (route = %d, want 11)", got)
	}
	// A key with no binding at all: Rebind fills the hole.
	r.Rebind(2, 7, 10, 10, 99)
	if got := r.Route(2, 7, chain, noneDead); got != 10 {
		t.Fatalf("rebind did not fill the empty slot (route = %d, want 10)", got)
	}
	var nilReg *BindingRegistry
	nilReg.Rebind(1, 7, 10, 10, 99) // must not panic
}

// At capacity the eviction happens BEFORE the least-bound choice: the freed
// count must be visible to the assignment, or a tie would resolve toward the
// provider the eviction is about to make the more-bound one.
func TestBindingEvictionPrecedesLeastBoundChoice(t *testing.T) {
	cur := time.Unix(0, 0)
	r := NewBindingRegistry(func() time.Time { cur = cur.Add(time.Second); return cur })
	chain := bindingTestChain(10, 11)
	for k := uint(1); k <= bindingCapacity; k++ {
		r.Route(k, 7, chain, noneDead)
	}
	// Alternating assignment left key 1 (provider 10) as the oldest entry;
	// refreshing it makes key 2's binding — provider 11 — the LRU victim.
	r.Route(1, 7, chain, noneDead)
	if got := r.Route(bindingCapacity+1, 7, chain, noneDead); got != 11 {
		t.Fatalf("assignment at capacity = %d, want 11 (the provider the eviction freed)", got)
	}
	counts := r.BindingCounts(7)
	if counts[10] != bindingCapacity/2 || counts[11] != bindingCapacity/2 {
		t.Fatalf("counts after eviction+assignment = %v, want an even %d/%d split", counts, bindingCapacity/2, bindingCapacity/2)
	}
}

func TestBindingTieBreaksByChainPosition(t *testing.T) {
	// All providers at zero: every fresh model's first keys must land on the
	// head of sort_order, then the next, and so on — deterministic rather
	// than map-order.
	r := NewBindingRegistry(nil)
	chain := bindingTestChain(10, 11, 12)
	if got := r.Route(1, 7, chain, noneDead); got != 10 {
		t.Fatalf("first assignment = %d, want chain head 10", got)
	}
	if got := r.Route(2, 7, chain, noneDead); got != 11 {
		t.Fatalf("second assignment = %d, want next in chain 11", got)
	}
	if got := r.Route(3, 7, chain, noneDead); got != 12 {
		t.Fatalf("third assignment = %d, want last in chain 12", got)
	}
	if got := r.Route(4, 7, chain, noneDead); got != 10 {
		t.Fatalf("fourth assignment = %d, want back to the least-bound head 10", got)
	}
}

func TestBindingLRUEvictsOldestUsed(t *testing.T) {
	base := time.Now().UTC()
	now := base
	r := NewBindingRegistry(func() time.Time { return now })
	chain := bindingTestChain(10, 11)

	// Fill to capacity on one model.
	for key := uint(1); key <= bindingCapacity; key++ {
		now = base.Add(time.Duration(key) * time.Second)
		if got := r.Route(key, 7, chain, noneDead); got == 0 {
			t.Fatalf("assignment %d returned 0", key)
		}
	}
	if n := len(r.bindings); n != bindingCapacity {
		t.Fatalf("registry holds %d bindings, want exactly the capacity %d", n, bindingCapacity)
	}

	// Touch key 1 so key 2 becomes the least recently used, then admit one
	// more key: the eviction must take key 2, not key 1.
	now = base.Add(time.Duration(bindingCapacity+1) * time.Second)
	r.Route(1, 7, chain, noneDead)
	now = base.Add(time.Duration(bindingCapacity+2) * time.Second)
	r.Route(bindingCapacity+1, 7, chain, noneDead)
	if _, ok := r.bindings[bindingKey{apiKeyID: 2, modelID: 7}]; ok {
		t.Fatalf("LRU kept the stale key 2; the touched key 1 should have survived instead")
	}
	if _, ok := r.bindings[bindingKey{apiKeyID: 1, modelID: 7}]; !ok {
		t.Fatalf("LRU evicted the recently-used key 1")
	}
	if n := len(r.bindings); n != bindingCapacity {
		t.Fatalf("registry holds %d bindings after eviction, want capacity %d", n, bindingCapacity)
	}
}

func TestBindingNilRegistryDegradesToCallersOrder(t *testing.T) {
	var r *BindingRegistry
	chain := bindingTestChain(10, 11, 12)
	if got := r.Route(1, 7, chain, noneDead); got != 0 {
		t.Fatalf("nil registry returned candidate %d, want 0", got)
	}
	if counts := r.BindingCounts(7); counts != nil {
		t.Fatalf("nil registry reported counts %v, want nil", counts)
	}
}

func TestBindingCountsArePerModel(t *testing.T) {
	// The same provider serving two models: counts must not leak across
	// models, or the least-bound choice on one model would be skewed by the
	// other's traffic.
	r := NewBindingRegistry(nil)
	chainA := bindingTestChain(10, 11)
	chainB := bindingTestChain(10, 11)
	r.Route(1, 100, chainA, noneDead)
	r.Route(2, 100, chainA, noneDead)
	r.Route(1, 200, chainB, noneDead)

	if got := r.BindingCounts(100); got[10] != 1 || got[11] != 1 {
		t.Fatalf("model 100 counts = %v, want 1/1", got)
	}
	if got := r.BindingCounts(200); len(got) != 1 {
		t.Fatalf("model 200 counts = %v, want a single provider", got)
	}
}
