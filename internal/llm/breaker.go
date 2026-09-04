package llm

import (
	"sync"
	"time"
)

// breaker is a per-provider circuit breaker (NFR-REL-04). It trips open after
// `threshold` consecutive failed provider calls and, while open, short-circuits
// every call with ErrCircuitOpen until `cooldown` elapses — so a provider outage
// or a spent quota fails fast instead of hammering the API. After the cooldown
// one trial call is admitted (half-open); its success closes the breaker, its
// failure re-opens it for another cooldown.
//
// ponytail: this mirrors internal/ingest/embed.breaker exactly (same three-state
// machine). It is copied rather than shared because embed's copy is coupled to
// embed's private error type (embed.ErrCircuitOpen) and extracting a shared
// package would force a change to the stable embed package for no behavioural
// gain (NFR-MNT: a new provider should not ripple outside its package). A third
// consumer justifies extracting internal/resilience.
type breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	fails     int
	state     breakerState
	openUntil time.Time
}

type breakerState int

const (
	closed breakerState = iota
	open
	halfOpen
)

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// allow reports whether a call may proceed. It returns ErrCircuitOpen while the
// breaker is open and the cooldown has not elapsed; once it has, it admits a
// single half-open trial.
func (b *breaker) allow() error {
	if b == nil || b.threshold <= 0 { // disabled
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == open {
		if b.now().Before(b.openUntil) {
			return ErrCircuitOpen
		}
		b.state = halfOpen // permit one trial
	}
	return nil
}

// record folds a completed call's outcome into the breaker: a success closes it
// and resets the failure count; a failure increments it (in half-open, any
// failure re-opens immediately) and opens the breaker at the threshold. An
// ErrCircuitOpen result is not an outcome (the call never ran) and is ignored.
func (b *breaker) record(err error) {
	if b == nil || b.threshold <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.fails = 0
		b.state = closed
		return
	}
	if b.state == halfOpen {
		b.trip()
		return
	}
	b.fails++
	if b.fails >= b.threshold {
		b.trip()
	}
}

func (b *breaker) trip() {
	b.state = open
	b.fails = 0
	b.openUntil = b.now().Add(b.cooldown)
}
