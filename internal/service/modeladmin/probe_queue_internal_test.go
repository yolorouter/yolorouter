package modeladmin

import "testing"

// An id already queued (or still being probed) must not be queued again — one
// verdict per pending mapping, however many times an import re-submits it. The
// FIFO length is the assertion that matters: the pending set deduplicates by
// construction (map keys), so only the work list can betray a double enqueue.
func TestProbeQueueEnqueueDeduplicatesPendingIDs(t *testing.T) {
	q := NewProbeQueue(nil, 4)
	q.Enqueue(1, 1, 2)
	q.Enqueue(2)
	if got := q.PendingCount(); got != 2 {
		t.Fatalf("expected 2 pending after duplicate enqueues, got %d", got)
	}
	q.mu.Lock()
	fifoLen := len(q.fifo)
	q.mu.Unlock()
	if fifoLen != 2 {
		t.Fatalf("expected 2 queued work items after duplicate enqueues, got %d", fifoLen)
	}
}
