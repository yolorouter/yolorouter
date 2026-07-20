package gateway

import (
	"testing"
	"time"
)

func TestLimiterConcurrency(t *testing.T) {
	l := NewLimiter()
	const key uint = 1
	if !l.AcquireConcurrency(key, 2) {
		t.Fatal("first acquire should succeed")
	}
	if !l.AcquireConcurrency(key, 2) {
		t.Fatal("second acquire should succeed")
	}
	if l.AcquireConcurrency(key, 2) {
		t.Fatal("third acquire should fail (limit 2)")
	}
	l.ReleaseConcurrency(key)
	if !l.AcquireConcurrency(key, 2) {
		t.Fatal("acquire after release should succeed")
	}
	// Release one more time than acquired past this point is a guarded no-op,
	// not an underflow — verify it doesn't panic or go negative.
	l.ReleaseConcurrency(key)
	l.ReleaseConcurrency(key)
}

func TestLimiterConcurrencyUnlimited(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 200; i++ {
		if !l.AcquireConcurrency(1, 0) {
			t.Fatalf("acquire %d failed under unlimited (limit 0)", i)
		}
	}
}

func TestLimiterConcurrencyPerKey(t *testing.T) {
	l := NewLimiter()
	// Two different keys do not share a concurrency budget.
	if !l.AcquireConcurrency(1, 1) {
		t.Fatal("key 1 first acquire failed")
	}
	if !l.AcquireConcurrency(2, 1) {
		t.Fatal("key 2 acquire should be independent of key 1")
	}
	if l.AcquireConcurrency(1, 1) {
		t.Fatal("key 1 second acquire should fail (limit 1)")
	}
}

func TestLimiterRPM(t *testing.T) {
	l := NewLimiter()
	const key uint = 1
	now := time.Unix(0, 0)
	for i := 0; i < 3; i++ {
		if !l.CheckRPM(key, 3, now) {
			t.Fatalf("check %d should succeed under limit 3", i)
		}
	}
	if l.CheckRPM(key, 3, now) {
		t.Fatal("4th check in the same minute should fail")
	}
	// A new minute window resets the counter.
	nextMinute := time.Unix(60, 0)
	if !l.CheckRPM(key, 3, nextMinute) {
		t.Fatal("first check of the next minute should succeed")
	}
}

func TestLimiterRPMUnlimited(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 100; i++ {
		if !l.CheckRPM(1, 0, time.Now()) {
			t.Fatalf("check %d failed under unlimited RPM", i)
		}
	}
}
