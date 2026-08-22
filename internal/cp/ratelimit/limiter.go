package ratelimit

import (
	"sync"
	"time"
)

// idleTTL is how long a bucket may sit unused before it is swept. A re-created
// bucket starts full, so eviction only reclaims memory (it never tightens a
// limit). Kept comfortably longer than any refill window.
const idleTTL = 10 * time.Minute

// Limiter holds one token bucket per key (an API key id or a tenant id) and is
// safe for concurrent use. It is an in-process limiter (ADR-0026): each API/
// worker replica keeps its own buckets, so the effective ceiling scales with
// replica count — acceptable for abuse-protection rate limiting and documented
// in the ADR; a shared store is deferred until a hard global cap is required.
//
// Limiter is a control-plane concern only (C-3): it holds counters keyed by
// opaque ids and never touches tenant data.
type Limiter struct {
	now Clock

	mu      sync.Mutex
	buckets map[string]*bucket
}

// New builds an empty limiter using the given clock (injected for deterministic
// tests). A nil clock defaults to time.Now.
func New(now Clock) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, buckets: make(map[string]*bucket)}
}

// Allow admits one request against the bucket for key at the given rate (qps)
// and burst (capacity), creating the bucket on first use. When refused it
// returns the Retry-After duration. A non-positive rate or burst refuses (fail
// closed).
func (l *Limiter) Allow(key string, rate, burst float64) (bool, time.Duration) {
	l.mu.Lock()
	b := l.buckets[key]
	if b == nil {
		b = newBucket(l.now)
		l.buckets[key] = b
	}
	l.mu.Unlock()

	return b.allow(rate, burst)
}

// evictIdle removes buckets whose last activity is older than idleTTL. It is
// called periodically by Run.
func (l *Limiter) evictIdle() {
	cutoff := l.now().Add(-idleTTL)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		b.mu.Lock()
		idle := b.primed && b.last.Before(cutoff)
		b.mu.Unlock()
		if idle {
			delete(l.buckets, k)
		}
	}
}

// len reports the number of live buckets (test/observability helper).
func (l *Limiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
