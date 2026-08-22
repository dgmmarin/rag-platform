package tenant

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// countingLoader tracks how many times the control plane was queried per tenant.
type countingLoader struct {
	mu   sync.Mutex
	rec  Record
	hits int
}

func (c *countingLoader) load(_ context.Context, _ ID) (Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hits++
	return c.rec, nil
}

func (c *countingLoader) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

func TestRegistryCachesWithinTTL(t *testing.T) {
	id := ID(uuid.New())
	ld := &countingLoader{rec: Record{ID: id, Status: StatusActive}}
	now := time.Unix(0, 0)
	reg := newCachingRegistry(ld.load, 30*time.Second, func() time.Time { return now })

	for i := 0; i < 5; i++ {
		if _, err := reg.Lookup(context.Background(), id); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	}
	if got := ld.count(); got != 1 {
		t.Fatalf("loader called %d times within TTL, want 1", got)
	}
}

func TestRegistryReloadsAfterTTL(t *testing.T) {
	id := ID(uuid.New())
	ld := &countingLoader{rec: Record{ID: id, Status: StatusActive}}
	now := time.Unix(0, 0)
	reg := newCachingRegistry(ld.load, 30*time.Second, func() time.Time { return now })

	if _, err := reg.Lookup(context.Background(), id); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	now = now.Add(31 * time.Second)
	if _, err := reg.Lookup(context.Background(), id); err != nil {
		t.Fatalf("lookup after TTL: %v", err)
	}
	if got := ld.count(); got != 2 {
		t.Fatalf("loader called %d times, want 2 (reload after TTL)", got)
	}
}

func TestRegistryInvalidateForcesReload(t *testing.T) {
	id := ID(uuid.New())
	ld := &countingLoader{rec: Record{ID: id, Status: StatusActive}}
	now := time.Unix(0, 0)
	reg := newCachingRegistry(ld.load, 30*time.Second, func() time.Time { return now })

	if _, err := reg.Lookup(context.Background(), id); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	reg.Invalidate(id) // NOTIFY tenant_changed path
	if _, err := reg.Lookup(context.Background(), id); err != nil {
		t.Fatalf("lookup after invalidate: %v", err)
	}
	if got := ld.count(); got != 2 {
		t.Fatalf("loader called %d times, want 2 (reload after invalidate)", got)
	}
}
