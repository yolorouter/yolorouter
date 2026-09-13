package gateway

import (
	"context"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway/circuit"
)

// BreakerState is the circuit ledger's read-write face: everything the
// kernel and the binding router ask of the per-provider health record.
// It exists so the record's PLACEMENT can differ behind it — the
// in-process ledger this binary serves from is one implementation; a
// deployment that runs several processes and must share the ledger
// supplies its own. The face carries no process-memory assumptions:
// every method is a complete read or a complete write, values in and
// values out — no shared pointers, no handles into the caller's
// address space, and generation tokens that must stay meaningful to
// whoever stores the record.
type BreakerState interface {
	Allow(providerID uint, destination int) (bool, uint64)
	StillAllowed(providerID uint, generation uint64) bool
	IsOpen(providerID uint, destination int) bool
	RecordFailure(providerID uint, generation uint64)
	RecordSoftFailure(providerID uint, generation uint64)
	RecordSuccess(providerID uint, generation uint64)
}

// KeyPoolState is the key pool's read-write face: the rotation cursor
// that spreads dispatches across a provider's keys and the rate-limit
// bench that demotes limited keys behind healthy ones. The pool stores
// no key material — keys arrive as rows on every call — so a shared
// placement only has to agree on ordering and bench verdicts. The
// methods stay unexported because the face follows the pool's own
// package-private shape; an implementation living outside this package
// would justify exporting the surface, and that change belongs to the
// moment such an implementation exists, not before.
type KeyPoolState interface {
	walkOrder(providerID uint, keys []ProviderKey) []ProviderKey
	coolKey(keyID uint, cfg int, dispatchedAt time.Time, d time.Duration)
	lengthenKeyBench(keyID uint, cfg int, dispatchedAt time.Time, d time.Duration)
	dropKey(keyID uint, cfg int, observedAt time.Time)
	clearKey(keyID uint, cfg int, dispatchedAt time.Time)
	stamp() time.Time
}

// BindingState is the sticky-binding registry's read-write face: the
// balanced-model spread, its quarantine of dead-end providers, and the
// rebinding of a caller whose bound candidate proved unusable. Route
// hands the implementation a providerDead callback because liveness is
// the BREAKER's verdict, not the registry's — a shared registry asks
// whatever breaker it is paired with. The admin binding-count view is
// deliberately NOT on the face: nothing in the kernel reads counts
// through the seam, so an implementation behind it would owe a method
// nobody calls.
type BindingState interface {
	Route(apiKeyID, modelID uint, candidates []ModelCandidate, providerDead func(uint) bool) uint
	Quarantine(providerID uint)
	Rebind(apiKeyID, modelID, providerID, candidateID, invalidatedCandidateID uint)
}

// StateStore is the gateway's process state behind one seam: the three
// components above, handed out through their faces. The kernel's
// decision paths reach all circuit, bench, and binding state through
// this seam and nothing else. A single-process binary serves the
// field-derived view below; a build whose state lives elsewhere (one
// process of several, sharing it) assigns its own implementation to
// the service's state field and serves from that instead. This
// repository ships no such build — the seam's shape, not an install
// point, is what it carries.
type StateStore interface {
	Breaker() BreakerState
	Keys() KeyPoolState
	Bindings() BindingState
}

// Compile-time proof that the in-process components already satisfy
// their faces: the interface extraction changed no behavior because
// the concrete types ARE the implementations.
var (
	_ BreakerState = (*circuit.Breaker)(nil)
	_ KeyPoolState = (*keyPool)(nil)
	_ BindingState = (*BindingRegistry)(nil)
	_ StateStore   = svcStateView{}
)

// svcStateView serves the three faces from whatever the Service's
// concrete components hold RIGHT NOW. Reading the fields at every
// access is the point: the service can be assembled field-by-field
// rather than through the constructor, and a component replaced
// wholesale after construction must be the one the kernel serves
// from on the very next access. A nil component keeps its nil-safe
// methods — the view adds no panics of its own.
type svcStateView struct{ s *Service }

func (v svcStateView) Breaker() BreakerState  { return v.s.breaker }
func (v svcStateView) Keys() KeyPoolState     { return v.s.keyPool }
func (v svcStateView) Bindings() BindingState { return v.s.bindings }

// st is the seam every kernel state access goes through. The default
// is the field-derived view above; a build whose state lives elsewhere
// assigns its own StateStore to the state field, and once set that
// store is the truth the kernel serves from. This repository ships no
// such assignment.
func (s *Service) st() StateStore {
	if s.state != nil {
		return s.state
	}
	return svcStateView{s: s}
}

// keyRows is the kernel's key fetch — straight through the Store, the
// one place all database access lives.
func (s *Service) keyRows(ctx context.Context, providerID uint) ([]ProviderKey, error) {
	return s.store.ListProviderKeysByProvider(ctx, providerID)
}
