package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func ctxWithTenant(id string) context.Context {
	tid := tenant.ID(uuid.MustParse(id))
	return tenant.WithTenantID(context.Background(), tid)
}

func TestHandlerListReturnsResolvedTenantUsage(t *testing.T) {
	q := &fakeQuery{rows: &fakeRows{rows: [][]any{
		{tenantA, day(2026, 8, 22), int64(10), int64(2), int64(30), int64(4000), int64(500), int64(700)},
	}}}
	h := NewHandlers(NewService(q))

	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil).
		WithContext(ctxWithTenant(tenantA))
	rec := httptest.NewRecorder()
	h.List(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Usage []Row `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Usage) != 1 || body.Usage[0].Queries != 10 {
		t.Fatalf("unexpected usage: %#v", body.Usage)
	}
	// The tenant must come from the resolved context, bound as a query arg
	// (FR-ACC-03) — never from a request parameter.
	if q.args[0].(string) != tenantA {
		t.Fatalf("resolved tenant not used: %#v", q.args)
	}
}

func TestHandlerListRequiresResolvedTenant(t *testing.T) {
	h := NewHandlers(NewService(&fakeQuery{rows: &fakeRows{}}))
	// No tenant in context.
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with no resolved tenant, got %d", rec.Code)
	}
}

func TestHandlerListParsesFromAndTo(t *testing.T) {
	q := &fakeQuery{rows: &fakeRows{}}
	h := NewHandlers(NewService(q))
	req := httptest.NewRequest(http.MethodGet, "/v1/usage?from=2026-08-01&to=2026-08-31", nil).
		WithContext(ctxWithTenant(tenantA))
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sawFrom, sawTo bool
	for _, a := range q.args {
		if tm, ok := a.(time.Time); ok {
			if tm.Equal(day(2026, 8, 1)) {
				sawFrom = true
			}
			if tm.Equal(day(2026, 8, 31)) {
				sawTo = true
			}
		}
	}
	if !sawFrom || !sawTo {
		t.Fatalf("from/to not parsed into query args: %#v", q.args)
	}
}

func TestHandlerListRejectsMalformedDate(t *testing.T) {
	h := NewHandlers(NewService(&fakeQuery{rows: &fakeRows{}}))
	req := httptest.NewRequest(http.MethodGet, "/v1/usage?from=not-a-date", nil).
		WithContext(ctxWithTenant(tenantA))
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for malformed from, got %d", rec.Code)
	}
}

func TestHandlerListRejectsInvertedRange(t *testing.T) {
	h := NewHandlers(NewService(&fakeQuery{rows: &fakeRows{}}))
	req := httptest.NewRequest(http.MethodGet, "/v1/usage?from=2026-08-31&to=2026-08-01", nil).
		WithContext(ctxWithTenant(tenantA))
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for inverted range, got %d", rec.Code)
	}
}
