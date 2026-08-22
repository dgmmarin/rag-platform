package ratelimit

import (
	"context"
	"testing"
	"time"
)

// Distinct keys have independent buckets: exhausting one does not limit another.
func TestLimiterPerKeyIndependence(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(func() time.Time { return now })

	// Exhaust key "a" (rate=1, burst=1).
	if ok, _ := l.Allow("a", 1, 1); !ok {
		t.Fatal("a: first request should be allowed")
	}
	if ok, _ := l.Allow("a", 1, 1); ok {
		t.Fatal("a: second request should be refused")
	}
	// key "b" is unaffected.
	if ok, _ := l.Allow("b", 1, 1); !ok {
		t.Fatal("b: first request should be allowed despite a being exhausted")
	}
}

// Idle buckets are evicted after the TTL so memory does not grow unbounded, and
// a re-created bucket starts full (a fresh burst).
func TestLimiterEvictsIdleBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(func() time.Time { return now })

	if ok, _ := l.Allow("a", 1, 1); !ok {
		t.Fatal("first request should be allowed")
	}
	if l.len() != 1 {
		t.Fatalf("bucket count = %d, want 1", l.len())
	}

	// Advance past the idle TTL and sweep.
	now = now.Add(idleTTL + time.Second)
	l.evictIdle()
	if l.len() != 0 {
		t.Fatalf("bucket count after eviction = %d, want 0", l.len())
	}

	// A re-created bucket is full again (fresh burst), independent of history.
	if ok, _ := l.Allow("a", 1, 1); !ok {
		t.Fatal("re-created bucket should start full")
	}
}

// Run sweeps idle buckets on its tick and returns when the context is cancelled.
func TestLimiterRunEvictsAndStops(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(func() time.Time { return now })

	if ok, _ := l.Allow("a", 1, 1); !ok {
		t.Fatal("first request should be allowed")
	}
	// Age the bucket past the TTL so the sweep will evict it.
	now = now.Add(idleTTL + time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx, time.Millisecond); close(done) }()

	deadline := time.After(2 * time.Second)
	for l.len() != 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not evict the idle bucket in time")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
