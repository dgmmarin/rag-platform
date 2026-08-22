package tenant

import (
	"context"
	"sync"
	"time"
)

// Registry resolves a tenant ID to its Record (status + connection details),
// reading the control-plane tenants and tenant_databases tables (SPEC-01 §3).
// Lookups are cached briefly; Invalidate drops a cached entry so a status change
// published via LISTEN/NOTIFY tenant_changed takes effect within ~1s.
type Registry interface {
	Lookup(ctx context.Context, id ID) (Record, error)
	Invalidate(id ID)
	Close()
}

// loaderFunc loads a single tenant's Record from the control plane.
type loaderFunc func(ctx context.Context, id ID) (Record, error)

// cacheEntry is a Record cached until fetchedAt+ttl.
type cacheEntry struct {
	rec       Record
	fetchedAt time.Time
}

// cachingRegistry wraps a loader with a short TTL cache (SPEC-01 §3: 30s). The
// clock is injectable so tests can advance time without sleeping.
type cachingRegistry struct {
	load loaderFunc
	ttl  time.Duration
	now  func() time.Time

	mu    sync.Mutex
	cache map[ID]cacheEntry
}

// newCachingRegistry builds a TTL-cached registry over load.
func newCachingRegistry(load loaderFunc, ttl time.Duration, now func() time.Time) *cachingRegistry {
	if now == nil {
		now = time.Now
	}
	return &cachingRegistry{
		load:  load,
		ttl:   ttl,
		now:   now,
		cache: make(map[ID]cacheEntry),
	}
}

// Lookup returns the cached Record if fresh, otherwise loads and caches it.
func (r *cachingRegistry) Lookup(ctx context.Context, id ID) (Record, error) {
	r.mu.Lock()
	if e, ok := r.cache[id]; ok && r.now().Sub(e.fetchedAt) < r.ttl {
		r.mu.Unlock()
		return e.rec, nil
	}
	r.mu.Unlock()

	rec, err := r.load(ctx, id)
	if err != nil {
		return Record{}, err
	}

	r.mu.Lock()
	r.cache[id] = cacheEntry{rec: rec, fetchedAt: r.now()}
	r.mu.Unlock()
	return rec, nil
}

// Invalidate drops a cached entry so the next Lookup reloads it. Called from the
// LISTEN/NOTIFY tenant_changed handler and on tenant move/delete.
func (r *cachingRegistry) Invalidate(id ID) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

// Close releases registry resources. The in-memory cache holds none; the
// control-plane-backed loader owns the NOTIFY listener lifecycle separately.
func (r *cachingRegistry) Close() {}
