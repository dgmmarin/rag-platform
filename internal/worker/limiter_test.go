package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func isSnooze(err error) bool {
	var s *river.JobSnoozeError
	return errors.As(err, &s)
}

// The cap admits up to N slots per tenant; a different tenant is unaffected (fairness),
// and releasing frees a slot.
func TestTenantLimiterAcquireRelease(t *testing.T) {
	l := newTenantLimiter(2, discardLog())
	if !l.acquire("A") {
		t.Fatal("first acquisition for A should succeed")
	}
	if !l.acquire("A") {
		t.Fatal("second acquisition for A should succeed")
	}
	if l.acquire("A") {
		t.Fatal("third acquisition for A should be capped")
	}
	if !l.acquire("B") {
		t.Fatal("another tenant must not be blocked by A's cap")
	}
	l.release("A")
	if !l.acquire("A") {
		t.Fatal("a released slot should free capacity")
	}
}

// A non-ingest job is never capped.
func TestTenantLimiterPassesThroughNonIngest(t *testing.T) {
	l := newTenantLimiter(1, discardLog())
	job := &rivertype.JobRow{Queue: QueueMaintenance, EncodedArgs: []byte(`{"tenant_id":"A"}`)}
	// Occupy A's ingest slot, then a maintenance job for A must still run.
	l.acquire("A")
	ran := false
	if err := l.Work(context.Background(), job, func(context.Context) error { ran = true; return nil }); err != nil {
		t.Fatalf("maintenance job returned %v", err)
	}
	if !ran {
		t.Fatal("maintenance job should not be capped by the ingest limiter")
	}
}

// Synthetic load: many concurrent ingest jobs for one tenant must never run more than
// `cap` at once (the excess snooze), while another tenant's job still proceeds.
func TestTenantLimiterUnderLoadCapsOneTenantAndServesOthers(t *testing.T) {
	const capN, load = 2, 10
	l := newTenantLimiter(capN, discardLog())
	jobA := &rivertype.JobRow{Queue: QueueIngest, EncodedArgs: []byte(`{"tenant_id":"A"}`)}

	var concurrent, maxConcurrent, ran, snoozed int32
	release := make(chan struct{})
	inner := func(context.Context) error {
		c := atomic.AddInt32(&concurrent, 1)
		for {
			m := atomic.LoadInt32(&maxConcurrent)
			if c <= m || atomic.CompareAndSwapInt32(&maxConcurrent, m, c) {
				break
			}
		}
		atomic.AddInt32(&ran, 1)
		<-release
		atomic.AddInt32(&concurrent, -1)
		return nil
	}

	var wg sync.WaitGroup
	for i := 0; i < load; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Work(context.Background(), jobA, inner); isSnooze(err) {
				atomic.AddInt32(&snoozed, 1)
			}
		}()
	}

	// Wait until the cap is saturated and the rest have snoozed.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&snoozed) < load-capN {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d snoozes, saw %d", load-capN, atomic.LoadInt32(&snoozed))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Tenant A is saturated at its cap; tenant B must still be served immediately.
	jobB := &rivertype.JobRow{Queue: QueueIngest, EncodedArgs: []byte(`{"tenant_id":"B"}`)}
	bDone := make(chan error, 1)
	go func() { bDone <- l.Work(context.Background(), jobB, func(context.Context) error { return nil }) }()
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("tenant B job returned %v, want it served despite A saturating", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tenant B was starved by tenant A's load")
	}

	close(release)
	wg.Wait()
	if maxConcurrent > capN {
		t.Fatalf("tenant A ran %d jobs concurrently, want <= %d", maxConcurrent, capN)
	}
	if atomic.LoadInt32(&ran) != capN {
		t.Fatalf("tenant A ran %d jobs, want exactly %d (the rest snoozed)", ran, capN)
	}
}
