package gateway

import (
	"sync"
	"time"
)

// minuteWindow is a per-key fixed-minute counter used for RPM. A fixed
// window (not sliding) is deliberately coarse — RPM's job is to protect the
// upstream from a single hot key, not to bill precisely, and the boundary
// jitter is acceptable. The minute key is unix epoch / 60 so it's
// timezone-independent (the "today by system timezone" rule is about
// the dashboard, not the rate counter).
type minuteWindow struct {
	minute int64
	count  int
}

// allow increments the counter for the current minute if the limit has not
// been hit yet. Returns false without incrementing when over the limit.
func (w *minuteWindow) allow(limit int, now time.Time) bool {
	m := now.Unix() / 60
	if w.minute != m {
		w.minute = m
		w.count = 0
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}

// keySlot is one API key's in-memory rate/concurrency state. Both fields are
// guarded by the slot's own mutex, not a global one, so two keys never
// contend on each other's accounting.
type keySlot struct {
	mu   sync.Mutex
	conc int
	rpm  minuteWindow
}

// Limiter enforces per-API-key concurrency + RPM in memory. v0.1 is
// process-local: a single-binary deployment has one gateway, so per-process
// counters match per-instance state. TPM pre-enforcement needs prompt-token
// estimation and is deferred.
//
// Slots are stored in a sync.Map so a hot key's accounting never serializes
// against an unrelated key's slot lookup (a single shared mutex + map was
// the previous shape — three global-lock acquisitions per request).
type Limiter struct {
	keys sync.Map // map[uint]*keySlot
}

func NewLimiter() *Limiter {
	return &Limiter{}
}

func (l *Limiter) slot(apiKeyID uint) *keySlot {
	actual, _ := l.keys.LoadOrStore(apiKeyID, &keySlot{})
	return actual.(*keySlot)
}

// AcquireConcurrency tries to take one in-flight slot for this key. limit
// <= 0 means unlimited (always succeeds, caller still MUST pair with
// ReleaseConcurrency — Release is a no-op when nothing was counted).
func (l *Limiter) AcquireConcurrency(apiKeyID uint, limit int) bool {
	if limit <= 0 {
		return true
	}
	s := l.slot(apiKeyID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conc >= limit {
		return false
	}
	s.conc++
	return true
}

// ReleaseConcurrency returns one in-flight slot. Safe to call when the key
// had no limit (AcquireConcurrency returned true via the unlimited path) —
// the counter stays at 0 and the decrement is a guarded no-op.
func (l *Limiter) ReleaseConcurrency(apiKeyID uint) {
	s := l.slot(apiKeyID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conc > 0 {
		s.conc--
	}
}

// CheckRPM increments the key's per-minute counter. limit <= 0 = unlimited.
// Returns false without incrementing when the limit is already hit this
// minute.
func (l *Limiter) CheckRPM(apiKeyID uint, limit int, now time.Time) bool {
	if limit <= 0 {
		return true
	}
	s := l.slot(apiKeyID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rpm.allow(limit, now)
}
