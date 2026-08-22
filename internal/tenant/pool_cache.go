package tenant

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// poolBuilder constructs a live pgxpool for a tenant from its Record. It is a
// seam: production dials Postgres (decrypting the password first, SPEC-01 §4);
// tests inject a builder that never dials.
type poolBuilder func(ctx context.Context, rec Record) (*pgxpool.Pool, error)

// poolCacheOptions configures a poolCache. maxPools is the hard cap before LRU
// eviction (SPEC-01 §4: default 200); idleTTL is how long an unused pool lives
// (default 10 min). now is injectable for deterministic tests.
type poolCacheOptions struct {
	build    poolBuilder
	maxPools int
	idleTTL  time.Duration
	now      func() time.Time
}

// pooledEntry is one cached pool with the metadata the LRU and idle sweeper need.
type pooledEntry struct {
	id       ID
	pool     *pgxpool.Pool
	lastUsed time.Time
}

// poolCache holds lazily-created per-tenant pools. It reuses a pool across
// Opens, evicts idle pools, and caps the total number open with LRU eviction
// (SPEC-01 §4). Building a pool is expensive (a network dial), so it happens at
// most once per tenant until eviction.
type poolCache struct {
	build    poolBuilder
	maxPools int
	idleTTL  time.Duration
	now      func() time.Time

	mu    sync.Mutex
	ll    *list.List           // front = most recently used
	items map[ID]*list.Element // ID -> element holding *pooledEntry
}

func newPoolCache(opts poolCacheOptions) *poolCache {
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.maxPools <= 0 {
		opts.maxPools = 200
	}
	if opts.idleTTL <= 0 {
		opts.idleTTL = 10 * time.Minute
	}
	return &poolCache{
		build:    opts.build,
		maxPools: opts.maxPools,
		idleTTL:  opts.idleTTL,
		now:      opts.now,
		ll:       list.New(),
		items:    make(map[ID]*list.Element),
	}
}

// get returns the tenant's pool, building it on first use. It marks the entry
// most-recently-used and enforces the hard cap by evicting the LRU pool.
func (c *poolCache) get(ctx context.Context, rec Record) (*pgxpool.Pool, error) {
	c.mu.Lock()
	if el, ok := c.items[rec.ID]; ok {
		ent := el.Value.(*pooledEntry)
		ent.lastUsed = c.now()
		c.ll.MoveToFront(el)
		c.mu.Unlock()
		return ent.pool, nil
	}
	c.mu.Unlock()

	// Build outside the lock so a slow dial does not block other tenants.
	pool, err := c.build(ctx, rec)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Another goroutine may have built it while we dialed; keep theirs, close ours.
	if el, ok := c.items[rec.ID]; ok {
		ent := el.Value.(*pooledEntry)
		ent.lastUsed = c.now()
		c.ll.MoveToFront(el)
		closePool(pool)
		return ent.pool, nil
	}
	ent := &pooledEntry{id: rec.ID, pool: pool, lastUsed: c.now()}
	c.items[rec.ID] = c.ll.PushFront(ent)
	c.evictOverCapLocked()
	return pool, nil
}

// close evicts and closes a tenant's pool (after delete or move; SPEC-01 §4).
func (c *poolCache) close(id ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		c.removeLocked(el)
	}
}

// sweepIdle closes pools unused for longer than idleTTL.
func (c *poolCache) sweepIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := c.now().Add(-c.idleTTL)
	for el := c.ll.Back(); el != nil; {
		prev := el.Prev()
		if el.Value.(*pooledEntry).lastUsed.Before(cutoff) {
			c.removeLocked(el)
		}
		el = prev
	}
}

// len reports the number of cached pools.
func (c *poolCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// closeAll evicts every pool (resolver shutdown).
func (c *poolCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.ll.Front(); el != nil; el = el.Next() {
		ent := el.Value.(*pooledEntry)
		delete(c.items, ent.id)
		closePool(ent.pool)
	}
	c.ll.Init()
}

// evictOverCapLocked drops least-recently-used pools until under the hard cap.
func (c *poolCache) evictOverCapLocked() {
	for c.ll.Len() > c.maxPools {
		if el := c.ll.Back(); el != nil {
			c.removeLocked(el)
		}
	}
}

// removeLocked unlinks and closes the pool held by el. Caller holds c.mu.
func (c *poolCache) removeLocked(el *list.Element) {
	ent := el.Value.(*pooledEntry)
	c.ll.Remove(el)
	delete(c.items, ent.id)
	closePool(ent.pool)
}

// closePool closes a pool, tolerating the nil pool the test builder returns.
func closePool(p *pgxpool.Pool) {
	if p != nil {
		p.Close()
	}
}
