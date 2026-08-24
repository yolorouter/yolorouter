// Package circuit is the provider health record: a per-provider three-state
// breaker (closed, open, half-open) held in process memory. The kernel books
// the decision table's circuit effects against it and consults it before
// walking a candidate, so a provider that keeps falling over stops receiving
// traffic until a probe shows it recovered.
//
// The record lives in memory on purpose. This build is a single binary, so
// there is no second process to share the record with, and losing it on
// restart merely means a broken provider gets one fresh chance — the breaker
// re-opens after the same handful of failures. Persisting it would buy
// nothing and cost an external dependency.
//
// Admission and accounting travel together as a GENERATION: Allow hands out
// the record's current generation, and a result is booked only when it still
// carries it. Upstream calls can outlive a whole open window (an attempt
// timeout is minutes, the window is seconds), and without the token a result
// from a request dispatched before the trip would arrive during half-open
// and close — or re-open — the breaker on evidence from the wrong era.
//
// The package is a leaf: standard library only, no opinion about where the
// verdicts it books come from.
package circuit

import (
	"sync"
	"time"
)

// Config sizes the breaker. All fields must be positive; the caller
// normalises before constructing.
type Config struct {
	// FailureThreshold is how many consecutive failures open the breaker.
	FailureThreshold int
	// SuccessThreshold is how many successes in half-open close it again.
	SuccessThreshold int
	// OpenTimeout is how long an open breaker refuses traffic before letting
	// probes through. It also sets the half-open probe rate: one admission
	// per OpenTimeout/SuccessThreshold, so a healthy provider can be back in
	// rotation after roughly one more window.
	OpenTimeout time.Duration
}

type stateKind uint8

const (
	closed stateKind = iota
	open
	halfOpen
)

// state is one provider's record. Zero value is a closed breaker with a
// clean slate, so a provider never seen before needs no initialisation.
type state struct {
	kind      stateKind
	failures  int
	successes int
	// soft accumulates half-weight faults: every second one converts into a
	// full failure, so soft faults open the breaker at twice the threshold.
	soft     int
	openedAt time.Time
	// generation increments on every state transition. Results carrying an
	// older generation describe dispatches from before the transition and
	// are not booked.
	generation uint64
	// lastProbeAt rate-limits half-open admissions. The bound is on TIME, not
	// on outstanding probes, deliberately: many admissions end with no health
	// verdict at all (a 4xx judged terminal, a rate limit, a caller that
	// hung up, a candidate skipped before dispatch), and a slot-counting
	// scheme would need every such path to hand its slot back — one missed
	// release and the breaker wedges. Time needs no release.
	lastProbeAt time.Time
	// destination is the version of the provider destination this record
	// describes. A repaired base URL is a different backend: the record
	// starts clean when the destination changes, instead of holding the old
	// backend's failures against the new one for a whole open window.
	destination int
}

// Breaker holds every provider's record behind one lock. The critical
// sections are a few field updates, and the relay consults it at most a few
// times per request, so one mutex is simpler than striping and nowhere near
// contention.
//
// A nil *Breaker is a breaker that never opens: every method is nil-safe, so
// a hand-assembled service (tests, embedders) that never wired one simply
// runs without circuit protection instead of panicking on the hot path.
type Breaker struct {
	cfg Config

	mu     sync.Mutex
	states map[uint]*state
	// now is the clock, injectable so tests can move time instead of
	// sleeping through open windows.
	now func() time.Time
}

// New builds a breaker. cfg must already be normalised (positive fields).
func New(cfg Config) *Breaker {
	return &Breaker{cfg: cfg, states: make(map[uint]*state), now: time.Now}
}

// NewWithClock is New with an injected clock, for tests.
func NewWithClock(cfg Config, now func() time.Time) *Breaker {
	b := New(cfg)
	b.now = now
	return b
}

// Allow reports whether the provider may receive a request, together with
// the generation the caller must hand back when booking the result. An open
// breaker whose timeout has elapsed transitions to half-open and admits
// rate-limited probes: the requests themselves are the probes, and what they
// book decides whether the breaker closes or re-opens.
//
// destination is the provider's destination version. A record made against
// one destination says nothing about another, so a change — an admin
// repairing the base URL — resets the record: the fresh generation also
// stamps every in-flight result from the old destination stale.
func (b *Breaker) Allow(providerID uint, destination int) (bool, uint64) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[providerID]
	if !ok {
		// First contact stamps the destination, so a later change is
		// distinguishable from it.
		b.states[providerID] = &state{destination: destination}
		return true, 0
	}
	if s.destination != destination {
		*s = state{generation: s.generation + 1, destination: destination}
		return true, s.generation
	}
	switch s.kind {
	case open:
		if b.now().Sub(s.openedAt) >= b.cfg.OpenTimeout {
			s.kind = halfOpen
			s.successes = 0
			s.lastProbeAt = b.now()
			return true, s.generation
		}
		return false, 0
	case halfOpen:
		if b.now().Sub(s.lastProbeAt) >= b.probeInterval() {
			s.lastProbeAt = b.now()
			return true, s.generation
		}
		return false, 0
	default: // closed
		return true, s.generation
	}
}

// StillAllowed reports whether an admission handed out earlier is still
// current. Every transition bumps the generation, so a mismatch means the
// breaker tripped (or recovered) since the admission — a key-rotation loop
// mid-candidate uses this to stop dispatching after its own faults opened
// the breaker, without consuming a probe the way a fresh Allow would.
func (b *Breaker) StillAllowed(providerID uint, generation uint64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[providerID]
	if !ok {
		return true
	}
	return s.generation == generation
}

// IsOpen reports whether the provider's breaker is refusing traffic right
// now, WITHOUT any of Allow's side effects: no open→half-open transition, no
// generation handed out, no half-open probe slot consumed. Callers that are
// about to dispatch must still go through Allow — this read exists for
// decisions that only need the health snapshot, such as picking which
// provider a sticky binding should target.
//
// An open breaker whose open window has already elapsed reports false: the
// provider deserves recovery traffic at that point, and the dispatches that
// traffic produces are what transition it through half-open via the normal
// Allow path. Half-open and closed also report false — half-open is already
// letting probes through, so it is not "refusing traffic" in any sense a
// binding decision should act on.
//
// destination is the provider's current destination version. A record made
// against a different destination describes a backend that no longer exists
// — Allow resets it on first contact — and reporting it open here would
// demote a freshly repaired provider for the remainder of a stale window.
// The snapshot stays read-only: the reset itself remains Allow's job.
func (b *Breaker) IsOpen(providerID uint, destination int) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[providerID]
	if !ok || s.kind != open || s.destination != destination {
		return false
	}
	return b.now().Sub(s.openedAt) < b.cfg.OpenTimeout
}

// probeInterval spaces half-open admissions so a healthy provider closes the
// breaker after roughly one more open window, while an unhealthy or
// verdict-less stream of admissions costs at most one probe per interval.
// Floored at one millisecond: an integer division that truncated to zero
// would turn the rate limit into an always-pass and flood the provider under
// probe traffic — config validation keeps sane deployments far above this,
// the floor keeps a hand-built config from disabling the limit entirely.
func (b *Breaker) probeInterval() time.Duration {
	iv := b.cfg.OpenTimeout / time.Duration(b.cfg.SuccessThreshold)
	if iv < time.Millisecond {
		return time.Millisecond
	}
	return iv
}

// RecordFailure books a fault against the provider. Consecutive faults open
// the breaker; a fault during half-open re-opens it immediately — the probe
// answered, and the answer was no. A result carrying a stale generation is
// dropped: it describes a dispatch from before the last transition.
func (b *Breaker) RecordFailure(providerID uint, generation uint64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stateFor(providerID)
	if generation != s.generation {
		return
	}
	switch s.kind {
	case closed:
		s.failures++
		if s.failures >= b.cfg.FailureThreshold {
			b.trip(s)
		}
	case halfOpen:
		b.trip(s)
	case open:
		// Late report from a request admitted in this same era but resolved
		// after the trip cannot happen — the trip changed the generation.
	}
}

// RecordSoftFailure books half a fault: a signal that says more about load
// than about health — a rate limit, a stream cut short after commitment.
// Every second one converts into a full failure, so persistent soft faults
// open the breaker at twice the threshold while intermittent ones (with
// successes clearing the streak in between) never accumulate. Zero booking
// was tried instead and rejected: a provider that truncates every stream
// would stay in rotation forever, invisible to a record that only counts
// hard failures.
func (b *Breaker) RecordSoftFailure(providerID uint, generation uint64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stateFor(providerID)
	if generation != s.generation {
		return
	}
	if s.kind == open {
		return
	}
	s.soft++
	if s.soft < 2 {
		return
	}
	s.soft = 0
	switch s.kind {
	case closed:
		s.failures++
		if s.failures >= b.cfg.FailureThreshold {
			b.trip(s)
		}
	case halfOpen:
		b.trip(s)
	}
}

// RecordSuccess books a healthy interaction. In closed state it wipes the
// consecutive-failure count — the threshold counts a streak, not a lifetime.
// In half-open, enough successes close the breaker. Stale generations are
// dropped, exactly as for failures.
func (b *Breaker) RecordSuccess(providerID uint, generation uint64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[providerID]
	if !ok {
		return
	}
	if generation != s.generation {
		return
	}
	switch s.kind {
	case closed:
		s.failures = 0
		s.soft = 0
	case halfOpen:
		// The soft accumulation clears here too: the streak semantics are
		// consecutive, and a healthy probe between two soft faults means they
		// were not consecutive. Soft faults deliberately do NOT clear
		// accumulated successes — half a fault is not evidence the recovery
		// failed, and only a full failure re-opens.
		s.soft = 0
		s.successes++
		if s.successes >= b.cfg.SuccessThreshold {
			*s = state{generation: s.generation + 1, destination: s.destination}
		}
	case open:
		// See RecordFailure: unreachable for a live generation.
	}
}

func (b *Breaker) stateFor(providerID uint) *state {
	s, ok := b.states[providerID]
	if !ok {
		s = &state{}
		b.states[providerID] = s
	}
	return s
}

func (b *Breaker) trip(s *state) {
	s.kind = open
	s.openedAt = b.now()
	s.failures = 0
	s.successes = 0
	s.soft = 0
	s.generation++
}
