package usage

import (
	"context"
	"testing"
	"time"
)

func TestRunFlushesPeriodicallyThenDrainsOnStop(t *testing.T) {
	db := &fakeUpsert{}
	c := NewCounter(db)
	c.Add(tenantA, Delta{Queries: 1})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx, 20*time.Millisecond)
		close(done)
	}()

	// Wait for at least one periodic flush to land.
	deadline := time.Now().Add(2 * time.Second)
	for len(db.snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("periodic flush never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Buffer more, then stop: Run must drain a final time before returning so no
	// counts are lost on shutdown.
	c.Add(tenantA, Delta{Queries: 5})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	var total int64
	for _, r := range db.snapshot() {
		total += r.delta.Queries
	}
	if total != 6 {
		t.Fatalf("want all 6 queries flushed (periodic + final drain), got %d", total)
	}
}
