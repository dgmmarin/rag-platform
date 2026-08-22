// Package ratelimit is the public-API rate limiter (STORY-03.9, NFR-SEC-07,
// SPEC-07 §1): a token bucket per API key and per tenant, both steady-rate at the
// tenant's settings.limits.qps, returning 429 with Retry-After on exceed. It is a
// control-plane concern only (C-3) — it counts requests keyed by opaque
// control-plane ids and reads only settings JSON, never tenant data — and keys
// its buckets off the authenticated credential + resolved tenant in context
// (FR-ACC-03), never a client-supplied value. Buckets are in-process (ADR-0026).
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Clock returns the current time. Injected so window boundaries are tested
// deterministically without real sleeps.
type Clock func() time.Time

// bucket is a lazily-refilled token bucket. It is refilled on demand from the
// elapsed time since the last check (no background goroutine per bucket), so a
// bucket costs nothing while idle. rate (tokens per second) and burst (bucket
// capacity) are passed on each call: the tenant's configured qps can change at
// runtime without rebuilding the bucket.
type bucket struct {
	mu     sync.Mutex
	now    Clock
	tokens float64
	last   time.Time
	// primed is false until the first allow() call so the bucket starts full
	// (a burst) regardless of the wall-clock epoch.
	primed bool
}

func newBucket(now Clock) *bucket {
	return &bucket{now: now}
}

// allow consumes one token if available, refilling from elapsed time first. It
// returns whether the request is admitted and, when refused, the duration until
// the next token becomes available (rounded up to whole seconds for the
// Retry-After header). rate<=0 refuses everything (fail closed).
func (b *bucket) allow(rate, burst float64) (bool, time.Duration) {
	if rate <= 0 || burst <= 0 {
		return false, time.Second
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.now()
	if !b.primed {
		b.tokens = burst
		b.last = t
		b.primed = true
	} else {
		elapsed := t.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(burst, b.tokens+elapsed*rate)
			b.last = t
		}
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Time until one whole token accrues, rounded up to a whole second.
	need := (1 - b.tokens) / rate
	retry := time.Duration(math.Ceil(need)) * time.Second
	if retry <= 0 {
		retry = time.Second
	}
	return false, retry
}
