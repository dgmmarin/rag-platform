package usage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeUpsert records every upsert the flusher performs so a test can assert what
// was written (and how deltas were merged before the flush).
type fakeUpsert struct {
	mu   sync.Mutex
	rows []upsertRow
	err  error
}

type upsertRow struct {
	tenantID string
	day      time.Time
	delta    Delta
}

func (f *fakeUpsert) UpsertUsage(_ context.Context, tenantID string, day time.Time, d Delta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, upsertRow{tenantID: tenantID, day: day, delta: d})
	return nil
}

func (f *fakeUpsert) snapshot() []upsertRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]upsertRow, len(f.rows))
	copy(out, f.rows)
	return out
}

const tenantA = "11111111-1111-1111-1111-111111111111"

func TestAddBuffersAndFlushMergesDeltasPerTenantDay(t *testing.T) {
	db := &fakeUpsert{}
	c := NewCounter(db)

	day := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	// Two adds for the same tenant/day must merge into one upsert row.
	c.AddAt(tenantA, Delta{Queries: 1}, day)
	c.AddAt(tenantA, Delta{Queries: 2, EmbedTokens: 100, ChunksEmbedded: 5}, day.Add(3*time.Hour))

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := db.snapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 merged upsert row, got %d: %#v", len(rows), rows)
	}
	got := rows[0]
	if got.tenantID != tenantA {
		t.Fatalf("tenant = %q, want %q", got.tenantID, tenantA)
	}
	// The day must be truncated to the UTC calendar day (both adds land on 2026-08-22).
	wantDay := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	if !got.day.Equal(wantDay) {
		t.Fatalf("day = %v, want %v", got.day, wantDay)
	}
	if got.delta.Queries != 3 || got.delta.EmbedTokens != 100 || got.delta.ChunksEmbedded != 5 {
		t.Fatalf("deltas not merged: %#v", got.delta)
	}
}

func TestAddSeparatesDistinctDays(t *testing.T) {
	db := &fakeUpsert{}
	c := NewCounter(db)

	d1 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	c.AddAt(tenantA, Delta{Queries: 1}, d1)
	c.AddAt(tenantA, Delta{Queries: 1}, d2)

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(db.snapshot()); got != 2 {
		t.Fatalf("want 2 rows for 2 distinct days, got %d", got)
	}
}

func TestZeroDeltaIsNotBuffered(t *testing.T) {
	db := &fakeUpsert{}
	c := NewCounter(db)
	c.Add(tenantA, Delta{})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(db.snapshot()); got != 0 {
		t.Fatalf("a zero delta must not write a row, got %d", got)
	}
}

func TestAddRejectsEmptyTenant(t *testing.T) {
	db := &fakeUpsert{}
	c := NewCounter(db)
	// Fail closed: an unscoped counter increment is dropped, never written to some
	// arbitrary/blank tenant row.
	c.Add("", Delta{Queries: 1})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(db.snapshot()); got != 0 {
		t.Fatalf("empty tenant must be dropped, got %d rows", got)
	}
}

func TestFlushIsEmptyWhenNothingBuffered(t *testing.T) {
	db := &fakeUpsert{}
	c := NewCounter(db)
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush on empty buffer: %v", err)
	}
	if got := len(db.snapshot()); got != 0 {
		t.Fatalf("nothing buffered should write nothing, got %d", got)
	}
}

func TestFlushClearsBufferSoNextFlushIsEmpty(t *testing.T) {
	db := &fakeUpsert{}
	c := NewCounter(db)
	c.Add(tenantA, Delta{Queries: 1})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush 1: %v", err)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush 2: %v", err)
	}
	// Only the first flush should have produced a row; the buffer clears on flush.
	if got := len(db.snapshot()); got != 1 {
		t.Fatalf("buffer not cleared after flush, got %d rows", got)
	}
}

func TestFlushErrorRetainsBuffer(t *testing.T) {
	db := &fakeUpsert{err: errors.New("boom")}
	c := NewCounter(db)
	c.Add(tenantA, Delta{Queries: 1})
	if err := c.Flush(context.Background()); err == nil {
		t.Fatal("expected flush error to propagate")
	}
	// The counts must not be lost on a failed flush: a subsequent successful flush
	// still writes them.
	db.err = nil
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	rows := db.snapshot()
	if len(rows) != 1 || rows[0].delta.Queries != 1 {
		t.Fatalf("counts lost after a failed flush: %#v", rows)
	}
}

func TestConcurrentAddsAreCountedExactlyOnce(t *testing.T) {
	db := &fakeUpsert{}
	c := NewCounter(db)
	day := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			c.AddAt(tenantA, Delta{Queries: 1}, day)
		}()
	}
	wg.Wait()

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows := db.snapshot()
	if len(rows) != 1 || rows[0].delta.Queries != n {
		t.Fatalf("want a single row summing to %d, got %#v", n, rows)
	}
}
