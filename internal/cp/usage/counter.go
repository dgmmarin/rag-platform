// Package usage implements control-plane usage accounting (FR-ADM-06, SPEC-10 §6,
// SPEC-02 §2): a sanctioned in-memory counter that API and worker paths increment,
// flushed periodically into usage_daily, and a tenant-scoped read API for
// billing/dashboards. It holds counters only — never tenant content (C-3).
package usage

import (
	"context"
	"sync"
	"time"
)

// Delta is a set of usage increments to attribute to a tenant on a given day. Its
// fields map one-to-one to the usage_daily columns (SPEC-02 §2); zero fields are
// no-ops. Producers construct a Delta describing what a single request/job did —
// e.g. a query is Delta{Queries: 1}; an embedding batch is
// Delta{ChunksEmbedded: n, EmbedTokens: t} — and hand it to Counter.Add.
type Delta struct {
	Queries        int64
	DocsIngested   int64
	ChunksEmbedded int64
	EmbedTokens    int64
	LLMInTokens    int64
	LLMOutTokens   int64
}

// isZero reports whether the delta carries no increments.
func (d Delta) isZero() bool {
	return d == Delta{}
}

// add merges another delta into this one.
func (d *Delta) add(o Delta) {
	d.Queries += o.Queries
	d.DocsIngested += o.DocsIngested
	d.ChunksEmbedded += o.ChunksEmbedded
	d.EmbedTokens += o.EmbedTokens
	d.LLMInTokens += o.LLMInTokens
	d.LLMOutTokens += o.LLMOutTokens
}

// UpsertDB is the minimal write surface the flusher needs: one accumulating
// upsert per (tenant, day). A pgx pool adapter satisfies it.
type UpsertDB interface {
	UpsertUsage(ctx context.Context, tenantID string, day time.Time, d Delta) error
}

// bucketKey groups buffered deltas by tenant and UTC calendar day.
type bucketKey struct {
	tenantID string
	day      time.Time
}

// Counter is the sanctioned in-memory usage aggregator (SPEC-10 §6). Producers
// call Add on the hot path (a cheap map merge under a mutex, no I/O); a Flush loop
// drains the buffer into usage_daily every 30 s via an accumulating upsert. It is
// safe for concurrent use.
type Counter struct {
	db  UpsertDB
	now func() time.Time

	mu      sync.Mutex
	buckets map[bucketKey]Delta
}

// NewCounter builds a counter writing through db.
func NewCounter(db UpsertDB) *Counter {
	return &Counter{
		db:      db,
		now:     time.Now,
		buckets: make(map[bucketKey]Delta),
	}
}

// Add attributes a delta to the tenant on the current UTC day. It is the write
// entry point other subsystems adopt (mirroring audit.Record): non-blocking, no
// I/O, safe to call from a request/job hot path. An empty tenant or zero delta is
// dropped — failing closed rather than writing an unscoped or empty row.
func (c *Counter) Add(tenantID string, d Delta) {
	c.AddAt(tenantID, d, c.now())
}

// AddAt is Add with an explicit timestamp (the day is derived from it in UTC). It
// exists so producers processing a batch with a known time — and tests — can
// attribute usage deterministically.
func (c *Counter) AddAt(tenantID string, d Delta, at time.Time) {
	if tenantID == "" || d.isZero() {
		return
	}
	key := bucketKey{tenantID: tenantID, day: dayOf(at)}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.buckets[key]
	cur.add(d)
	c.buckets[key] = cur
}

// Flush drains the buffered counts into usage_daily. Each (tenant, day) bucket is
// written with one accumulating upsert (SPEC-10 §6). On the first upsert error the
// remaining buckets are put back so no counts are lost; the next Flush retries
// them. Buckets that were written successfully are not re-sent.
func (c *Counter) Flush(ctx context.Context) error {
	c.mu.Lock()
	drained := c.buckets
	c.buckets = make(map[bucketKey]Delta)
	c.mu.Unlock()

	for key, d := range drained {
		if err := c.db.UpsertUsage(ctx, key.tenantID, key.day, d); err != nil {
			c.restore(key, d, drained)
			return err
		}
		delete(drained, key)
	}
	return nil
}

// restore merges the not-yet-flushed buckets (the failed one plus any still in
// drained) back into the live buffer so a later Flush retries them.
func (c *Counter) restore(failed bucketKey, failedDelta Delta, remaining map[bucketKey]Delta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining[failed] = failedDelta
	for key, d := range remaining {
		cur := c.buckets[key]
		cur.add(d)
		c.buckets[key] = cur
	}
}

// DefaultFlushInterval is the flush cadence SPEC-10 §6 mandates.
const DefaultFlushInterval = 30 * time.Second

// Run flushes the buffer every interval until ctx is cancelled, then performs one
// final drain so shutdown does not lose the last window of counts. A flush error
// is non-fatal to the loop: the counts are retained (Flush restores them) and the
// next tick retries. Callers wanting the error surfaced should log it via the
// returned err of a manual Flush; Run intentionally keeps counting through a
// transient control-plane outage. Start it once at boot for the process's counter.
func (c *Counter) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Best-effort final drain with a fresh context (ctx is already done).
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = c.Flush(flushCtx)
			cancel()
			return
		case <-t.C:
			_ = c.Flush(ctx)
		}
	}
}

// dayOf returns at's UTC calendar day at midnight, matching the usage_daily.day
// DATE column.
func dayOf(at time.Time) time.Time {
	u := at.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
