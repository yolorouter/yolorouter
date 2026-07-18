package middleware

import (
	"sync"
	"testing"
)

func TestSemaphoreAllowsAcquiresWithinCapacity(t *testing.T) {
	sem := NewSemaphore(2)
	if !sem.TryAcquire() {
		t.Fatalf("expected first TryAcquire to succeed")
	}
	if !sem.TryAcquire() {
		t.Fatalf("expected second TryAcquire to succeed")
	}
}

func TestSemaphoreRejectsAcquireBeyondCapacity(t *testing.T) {
	sem := NewSemaphore(1)
	if !sem.TryAcquire() {
		t.Fatalf("expected first TryAcquire to succeed")
	}
	if sem.TryAcquire() {
		t.Fatalf("expected second TryAcquire to fail while the first slot is still held")
	}
}

func TestSemaphoreReleaseFreesASlotForReuse(t *testing.T) {
	sem := NewSemaphore(1)
	if !sem.TryAcquire() {
		t.Fatalf("expected first TryAcquire to succeed")
	}
	sem.Release()
	if !sem.TryAcquire() {
		t.Fatalf("expected TryAcquire to succeed again after Release")
	}
}

// TestSemaphoreConcurrentAcquiresNeverExceedCapacity fires many goroutines
// at a small-capacity semaphore simultaneously and confirms the number of
// successful acquires never exceeds the configured capacity, holding each
// successful acquire open until every goroutine has attempted its acquire
// — a race here would show up as more than `capacity` successes.
func TestSemaphoreConcurrentAcquiresNeverExceedCapacity(t *testing.T) {
	const capacity = 3
	const attempts = 20
	sem := NewSemaphore(capacity)

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	for range attempts {
		wg.Go(func() {
			start.Wait()
			if sem.TryAcquire() {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		})
	}
	start.Done()
	wg.Wait()

	if successCount != capacity {
		t.Fatalf("expected exactly %d successful acquires out of %d concurrent attempts, got %d", capacity, attempts, successCount)
	}
}
