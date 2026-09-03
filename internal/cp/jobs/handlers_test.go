package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/tenant"
)

const tenantA = "11111111-1111-1111-1111-111111111111"

func ctxWithTenant(ctx context.Context) context.Context {
	return tenant.WithTenantID(ctx, tenant.ID(uuid.MustParse(tenantA)))
}

func decodeEnvelope(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, body)
	}
	return env.Error.Code
}

func TestHandlerListRequiresTenant(t *testing.T) {
	h := NewHandlers(NewService(newFakeStore()))
	r := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	rr := httptest.NewRecorder()
	h.List(rr, r) // no tenant in context
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "unauthorized" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerListInvalidStatusFilter(t *testing.T) {
	h := NewHandlers(NewService(newFakeStore()))
	r := httptest.NewRequest(http.MethodGet, "/v1/jobs?status=bogus", nil)
	rr := httptest.NewRecorder()
	h.List(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "validation" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerGetNotFound(t *testing.T) {
	h := NewHandlers(NewService(newFakeStore()))
	r := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+tenantA, nil)
	r.SetPathValue("id", tenantA) // a valid-shaped id that does not exist
	rr := httptest.NewRecorder()
	h.Get(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "not_found" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerCancelQueued200(t *testing.T) {
	fs := newFakeStore()
	j := fs.add(Job{ID: tenantA, TenantID: tenantA, Kind: "sync_source", Status: StatusQueued})
	h := NewHandlers(NewService(fs))
	r := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+j.ID+"/cancel", nil)
	r.SetPathValue("id", j.ID)
	rr := httptest.NewRecorder()
	h.Cancel(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	var got Job
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
}

func TestHandlerCancelRunningSeam404(t *testing.T) {
	fs := newFakeStore()
	j := fs.add(Job{ID: tenantA, TenantID: tenantA, Kind: "sync_source", Status: StatusRunning})
	h := NewHandlers(NewService(fs)) // Canceller nil
	r := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+j.ID+"/cancel", nil)
	r.SetPathValue("id", j.ID)
	rr := httptest.NewRecorder()
	h.Cancel(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404 seam, got %d", rr.Code)
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "not_found" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerCancelRunningWithWorker202(t *testing.T) {
	fs := newFakeStore()
	j := fs.add(Job{ID: tenantA, TenantID: tenantA, Kind: "sync_source", Status: StatusRunning})
	svc := NewService(fs)
	svc.Canceller = &fakeCanceller{}
	h := NewHandlers(svc)
	r := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+j.ID+"/cancel", nil)
	r.SetPathValue("id", j.ID)
	rr := httptest.NewRecorder()
	h.Cancel(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestHandlerCancelTerminal409(t *testing.T) {
	fs := newFakeStore()
	j := fs.add(Job{ID: tenantA, TenantID: tenantA, Kind: "sync_source", Status: StatusSucceeded})
	h := NewHandlers(NewService(fs))
	r := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+j.ID+"/cancel", nil)
	r.SetPathValue("id", j.ID)
	rr := httptest.NewRecorder()
	h.Cancel(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "conflict" {
		t.Fatalf("code=%q", code)
	}
}
