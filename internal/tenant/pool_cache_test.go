package tenant

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// countingBuilder records how many pools it built and per-tenant build counts.
// It returns nil pools (never dialed) so cache mechanics are tested in isolation.
type countingBuilder struct {
	mu   sync.Mutex
	byID map[ID]int
}

func newCountingBuilder() *countingBuilder { return &countingBuilder{byID: map[ID]int{}} }

func (c *countingBuilder) build(_ context.Context, rec Record) (*pgxpool.Pool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID[rec.ID]++
	return nil, nil
}

func (c *countingBuilder) count(id ID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byID[id]
}

func recFor(id ID) Record { return Record{ID: id, Status: StatusActive, MaxConns: 5} }

func TestPoolCacheLazyCreateAndReuse(t *testing.T) {
	b := newCountingBuilder()
	pc := newPoolCache(poolCacheOptions{build: b.build, maxPools: 200, idleTTL: time.Hour})
	id := ID(uuid.New())

	for i := 0; i < 3; i++ {
		if _, err := pc.get(context.Background(), recFor(id)); err != nil {
			t.Fatalf("get: %v", err)
		}
	}
	if got := b.count(id); got != 1 {
		t.Fatalf("builder called %d times, want 1 (lazy create + reuse)", got)
	}
}

func TestPoolCacheEvictsAfterIdleTTL(t *testing.T) {
	b := newCountingBuilder()
	now := time.Unix(0, 0)
	pc := newPoolCache(poolCacheOptions{
		build:    b.build,
		maxPools: 200,
		idleTTL:  10 * time.Minute,
		now:      func() time.Time { return now },
	})
	id := ID(uuid.New())

	if _, err := pc.get(context.Background(), recFor(id)); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Advance past the idle TTL and sweep; the entry should be evicted and the
	// next get must rebuild.
	now = now.Add(11 * time.Minute)
	pc.sweepIdle()
	if _, err := pc.get(context.Background(), recFor(id)); err != nil {
		t.Fatalf("get after sweep: %v", err)
	}
	if got := b.count(id); got != 2 {
		t.Fatalf("builder called %d times, want 2 (rebuild after idle eviction)", got)
	}
}

func TestPoolCacheHardCapEvictsLRU(t *testing.T) {
	b := newCountingBuilder()
	tick := time.Unix(0, 0)
	pc := newPoolCache(poolCacheOptions{
		build:    b.build,
		maxPools: 2,
		idleTTL:  time.Hour,
		now:      func() time.Time { tick = tick.Add(time.Second); return tick },
	})
	a, bb, c := ID(uuid.New()), ID(uuid.New()), ID(uuid.New())

	mustGet := func(id ID) {
		if _, err := pc.get(context.Background(), recFor(id)); err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
	}
	mustGet(a)  // cache: [a]
	mustGet(bb) // cache: [a, bb]
	mustGet(a)  // touch a so bb is now LRU: [bb, a]
	mustGet(c)  // over cap -> evict LRU (bb): [a, c]

	if pc.len() != 2 {
		t.Fatalf("cache size = %d, want 2 (hard cap)", pc.len())
	}
	// bb was evicted, so a re-get rebuilds it; a and c were retained.
	mustGet(bb)
	if got := b.count(bb); got != 2 {
		t.Fatalf("bb build count = %d, want 2 (LRU-evicted then rebuilt)", got)
	}
	if got := b.count(a); got != 1 {
		t.Fatalf("a build count = %d, want 1 (retained across cap eviction)", got)
	}
}

func TestPoolCacheCloseEvicts(t *testing.T) {
	b := newCountingBuilder()
	pc := newPoolCache(poolCacheOptions{build: b.build, maxPools: 200, idleTTL: time.Hour})
	id := ID(uuid.New())

	if _, err := pc.get(context.Background(), recFor(id)); err != nil {
		t.Fatalf("get: %v", err)
	}
	pc.close(id)
	if _, err := pc.get(context.Background(), recFor(id)); err != nil {
		t.Fatalf("get after close: %v", err)
	}
	if got := b.count(id); got != 2 {
		t.Fatalf("builder called %d times, want 2 (rebuild after Close)", got)
	}
}
