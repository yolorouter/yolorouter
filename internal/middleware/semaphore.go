package middleware

import "golang.org/x/sync/semaphore"

// Semaphore is a non-blocking counting semaphore: TryAcquire returns false
// immediately once the capacity is exhausted, letting the caller shed the
// excess work instead of queueing it (queueing would just delay the same
// expensive work rather than actually bounding it). A thin wrapper around
// golang.org/x/sync/semaphore.Weighted (already in go.sum) with weight
// fixed at 1 per acquire — no reason to hand-roll the same non-blocking
// acquire/release logic over a raw channel.
//
// This is a plain struct the caller invokes around just the expensive part
// of its own work — deliberately NOT a gin.HandlerFunc middleware. A
// middleware wraps the entire downstream handler, including
// c.ShouldBindJSON's body read; on POST /api/admin/auth/login that would
// mean the semaphore slot is held for as long as the client takes to
// finish sending the request body, with only ReadHeaderTimeout (not a full
// body ReadTimeout) bounding that wait. A handful of connections that send
// valid headers and then stall mid-body would then permanently exhaust
// every slot at zero CPU cost — worse than the unbounded-bcrypt problem
// this was built to fix. Acquiring only around the bcrypt-triggering
// service.Login call, after the body has already been fully read and
// parsed, avoids that.
type Semaphore struct {
	weighted *semaphore.Weighted
}

// NewSemaphore creates a semaphore with the given capacity.
func NewSemaphore(capacity int) *Semaphore {
	return &Semaphore{weighted: semaphore.NewWeighted(int64(capacity))}
}

// TryAcquire reports whether a slot was acquired. Callers must call
// Release exactly once for every TryAcquire that returned true.
func (s *Semaphore) TryAcquire() bool {
	return s.weighted.TryAcquire(1)
}

// Release returns a slot acquired via a successful TryAcquire.
func (s *Semaphore) Release() {
	s.weighted.Release(1)
}
