package audit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandlerListReturnsEntriesForTenant(t *testing.T) {
	tenant := "t-1"
	q := &fakeQuery{rows: &fakeRows{rows: [][]any{
		{int64(1), tenant, "u-9", nil, "settings.update", "tenant", tenant, []byte(`{"changed":["chunking"]}`), time.Unix(100, 0)},
	}}}
	h := NewHandlers(NewService(q))

	req := httptest.NewRequest(http.MethodGet, "/admin/audit?tenant="+tenant, nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Entries) != 1 || body.Entries[0].Action != "settings.update" {
		t.Fatalf("unexpected entries: %#v", body.Entries)
	}
	// the tenant filter must have reached the query as a bound arg
	if q.args[0].(string) != tenant {
		t.Fatalf("tenant not passed to query: %#v", q.args)
	}
}

func TestHandlerListRequiresTenantParam(t *testing.T) {
	h := NewHandlers(NewService(&fakeQuery{rows: &fakeRows{}}))
	req := httptest.NewRequest(http.MethodGet, "/admin/audit", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing tenant, got %d", rec.Code)
	}
}

func TestHandlerListHonoursLimitAndBefore(t *testing.T) {
	q := &fakeQuery{rows: &fakeRows{}}
	h := NewHandlers(NewService(q))
	req := httptest.NewRequest(http.MethodGet, "/admin/audit?tenant=t-1&limit=5&before=42", nil)
	h.List(httptest.NewRecorder(), req)

	// limit is the last arg; before is the second.
	if got := q.args[len(q.args)-1].(int); got != 5 {
		t.Fatalf("limit not parsed, got %d", got)
	}
	if got := q.args[1].(int64); got != 42 {
		t.Fatalf("before not parsed, got %d", got)
	}
}

func TestHandlerListBadLimitIsIgnored(t *testing.T) {
	q := &fakeQuery{rows: &fakeRows{}}
	h := NewHandlers(NewService(q))
	req := httptest.NewRequest(http.MethodGet, "/admin/audit?tenant=t-1&limit=abc", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bad limit should be ignored (default), got %d", rec.Code)
	}
	if got := q.args[len(q.args)-1].(int); got != defaultLimit {
		t.Fatalf("want default limit %d, got %d", defaultLimit, got)
	}
}
