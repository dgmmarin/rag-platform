package ratelimit

import (
	"testing"
	"time"
)

// A fresh bucket admits a burst up to its capacity, then refuses.
func TestBucketAllowsBurstThenRefuses(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	b := newBucket(clock)

	// rate=2 qps, burst=2: two immediate requests allowed, the third refused.
	for i := 0; i < 2; i++ {
		if ok, _ := b.allow(2, 2); !ok {
			t.Fatalf("request %d: expected allow within burst", i)
		}
	}
	ok, retry := b.allow(2, 2)
	if ok {
		t.Fatal("third request should be refused (burst exhausted)")
	}
	// At 2 qps one token refills in 0.5s; Retry-After rounds up to 1s.
	if retry != time.Second {
		t.Fatalf("Retry-After = %v, want 1s", retry)
	}
}

// Tokens refill over time: after enough elapsed time a refused bucket admits again.
func TestBucketRefillsOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	b := newBucket(clock)

	// Exhaust a rate=1, burst=1 bucket.
	if ok, _ := b.allow(1, 1); !ok {
		t.Fatal("first request should be allowed")
	}
	if ok, _ := b.allow(1, 1); ok {
		t.Fatal("second immediate request should be refused")
	}

	// Advance one second: exactly one token refills.
	now = now.Add(time.Second)
	if ok, _ := b.allow(1, 1); !ok {
		t.Fatal("request after 1s refill should be allowed")
	}
}

// Refill is capped at burst: idle time does not accumulate unbounded tokens.
func TestBucketRefillCappedAtBurst(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	b := newBucket(clock)

	// Idle a long time on a rate=1, burst=2 bucket.
	now = now.Add(time.Hour)
	// Only 2 tokens are available (capped), not 3600.
	for i := 0; i < 2; i++ {
		if ok, _ := b.allow(1, 2); !ok {
			t.Fatalf("request %d within capped burst should be allowed", i)
		}
	}
	if ok, _ := b.allow(1, 2); ok {
		t.Fatal("third request should be refused; refill capped at burst")
	}
}
