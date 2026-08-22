package usage

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeRows is a canned multi-row result for the reader.
type fakeRows struct {
	rows [][]any
	i    int
	err  error
}

func (r *fakeRows) Next() bool { r.i++; return r.i <= len(r.rows) }
func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = row[i].(string)
		case *time.Time:
			*d = row[i].(time.Time)
		case *int64:
			*d = row[i].(int64)
		}
	}
	return nil
}
func (r *fakeRows) Err() error { return r.err }
func (r *fakeRows) Close()     {}

type fakeQuery struct {
	sql  string
	args []any
	rows *fakeRows
	err  error
}

func (q *fakeQuery) Query(_ context.Context, sql string, args ...any) (Rows, error) {
	q.sql, q.args = sql, args
	if q.err != nil {
		return nil, q.err
	}
	return q.rows, nil
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestListRequiresTenant(t *testing.T) {
	svc := NewService(&fakeQuery{rows: &fakeRows{}})
	_, err := svc.List(context.Background(), ListParams{TenantID: ""})
	if err == nil {
		t.Fatal("expected error when TenantID is empty (fail closed)")
	}
}

func TestListReturnsRowsAndBindsTenant(t *testing.T) {
	q := &fakeQuery{rows: &fakeRows{rows: [][]any{
		{tenantA, day(2026, 8, 22), int64(10), int64(2), int64(30), int64(4000), int64(500), int64(700)},
		{tenantA, day(2026, 8, 21), int64(5), int64(1), int64(15), int64(2000), int64(0), int64(0)},
	}}}
	svc := NewService(q)
	rows, err := svc.List(context.Background(), ListParams{TenantID: tenantA})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].TenantID != tenantA || rows[0].Queries != 10 || rows[0].LLMOutTokens != 700 {
		t.Fatalf("row not scanned: %#v", rows[0])
	}
	if !rows[0].Day.Equal(day(2026, 8, 22)) {
		t.Fatalf("day not scanned: %v", rows[0].Day)
	}
	// tenant must be a bound parameter, not interpolated.
	if q.args[0].(string) != tenantA {
		t.Fatalf("tenant not bound as arg: %#v", q.args)
	}
}

func TestListPassesDateRangeWhenGiven(t *testing.T) {
	q := &fakeQuery{rows: &fakeRows{}}
	svc := NewService(q)
	from := day(2026, 8, 1)
	to := day(2026, 8, 31)
	if _, err := svc.List(context.Background(), ListParams{TenantID: tenantA, From: from, To: to}); err != nil {
		t.Fatalf("List: %v", err)
	}
	// from and to must reach the query as bound args.
	var sawFrom, sawTo bool
	for _, a := range q.args {
		if tm, ok := a.(time.Time); ok {
			if tm.Equal(from) {
				sawFrom = true
			}
			if tm.Equal(to) {
				sawTo = true
			}
		}
	}
	if !sawFrom || !sawTo {
		t.Fatalf("date range not bound: %#v", q.args)
	}
	if !strings.Contains(strings.ToLower(q.sql), "day >=") || !strings.Contains(strings.ToLower(q.sql), "day <=") {
		t.Fatalf("query does not filter by day range: %s", q.sql)
	}
}

func TestListDefaultsToLast30DaysWhenNoRange(t *testing.T) {
	q := &fakeQuery{rows: &fakeRows{}}
	svc := NewService(q)
	svc.now = func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }
	if _, err := svc.List(context.Background(), ListParams{TenantID: tenantA}); err != nil {
		t.Fatalf("List: %v", err)
	}
	// With no range the reader still bounds the scan (default window), so a bound
	// "from" arg must be present and be ~30 days before today.
	var from *time.Time
	for _, a := range q.args {
		if tm, ok := a.(time.Time); ok {
			t := tm
			if from == nil || t.Before(*from) {
				from = &t
			}
		}
	}
	if from == nil {
		t.Fatalf("no default from bound: %#v", q.args)
	}
	want := day(2026, 7, 23) // 2026-08-22 minus 30 days
	if !from.Equal(want) {
		t.Fatalf("default from = %v, want %v", *from, want)
	}
}

func TestListRejectsInvertedRange(t *testing.T) {
	svc := NewService(&fakeQuery{rows: &fakeRows{}})
	_, err := svc.List(context.Background(), ListParams{
		TenantID: tenantA,
		From:     day(2026, 8, 31),
		To:       day(2026, 8, 1),
	})
	if err == nil {
		t.Fatal("expected error when from is after to")
	}
}
