package audit

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
		case *int64:
			*d = row[i].(int64)
		case **string:
			if row[i] == nil {
				*d = nil
			} else {
				s := row[i].(string)
				*d = &s
			}
		case *string:
			*d = row[i].(string)
		case *[]byte:
			*d = row[i].([]byte)
		case *time.Time:
			*d = row[i].(time.Time)
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

func TestListRequiresTenant(t *testing.T) {
	svc := &Service{DB: &fakeQuery{rows: &fakeRows{}}}
	if _, err := svc.List(context.Background(), ListParams{TenantID: ""}); err == nil {
		t.Fatal("expected error when TenantID is empty")
	}
}

func TestListReturnsEntriesNewestFirst(t *testing.T) {
	tenant := "t-1"
	q := &fakeQuery{rows: &fakeRows{rows: [][]any{
		{int64(2), tenant, "u-9", nil, "member.add", "member", "u-x", []byte(`{"role":"editor"}`), time.Unix(200, 0)},
		{int64(1), tenant, nil, nil, "tenant.create", "tenant", tenant, []byte(`{}`), time.Unix(100, 0)},
	}}}
	svc := &Service{DB: q}
	entries, err := svc.List(context.Background(), ListParams{TenantID: tenant})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].ID != 2 || entries[0].Action != "member.add" {
		t.Fatalf("newest-first ordering broken: %#v", entries[0])
	}
	if entries[0].ActorUserID == nil || *entries[0].ActorUserID != "u-9" {
		t.Fatalf("actor not scanned: %#v", entries[0].ActorUserID)
	}
	if entries[1].ActorUserID != nil {
		t.Fatalf("nil actor should stay nil: %#v", entries[1].ActorUserID)
	}
	if entries[0].Details["role"] != "editor" {
		t.Fatalf("details not decoded: %#v", entries[0].Details)
	}
	// tenant must be a bound parameter, not interpolated
	if !strings.Contains(q.args[0].(string), tenant) {
		t.Fatalf("tenant not passed as arg: %#v", q.args)
	}
}

func TestListClampsLimit(t *testing.T) {
	q := &fakeQuery{rows: &fakeRows{}}
	svc := &Service{DB: q}
	if _, err := svc.List(context.Background(), ListParams{TenantID: "t", Limit: 10000}); err != nil {
		t.Fatalf("List: %v", err)
	}
	// the last bound arg is the limit; it must be capped at maxLimit
	limit, ok := q.args[len(q.args)-1].(int)
	if !ok {
		t.Fatalf("limit arg not an int: %#v", q.args)
	}
	if limit > maxLimit {
		t.Fatalf("limit %d exceeds cap %d", limit, maxLimit)
	}
}

func TestListDefaultsLimitWhenUnset(t *testing.T) {
	q := &fakeQuery{rows: &fakeRows{}}
	svc := &Service{DB: q}
	if _, err := svc.List(context.Background(), ListParams{TenantID: "t"}); err != nil {
		t.Fatalf("List: %v", err)
	}
	limit := q.args[len(q.args)-1].(int)
	if limit != defaultLimit {
		t.Fatalf("want default limit %d, got %d", defaultLimit, limit)
	}
}
