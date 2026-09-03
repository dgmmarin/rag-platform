package tenants

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/provision"
)

func newAdminHandlers(store *fakeAdminStore, prov *fakeProvisioner, life *fakeLifecycle) *AdminHandlers {
	return NewAdminHandlers(&AdminService{Store: store, Prov: prov, Life: life})
}

func TestAdminHandlerCreateReturns201WithTenantAndJob(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{
		"t-new": {ID: "t-new", Slug: "acme", Name: "Acme", Status: "active"},
	}}
	h := newAdminHandlers(store, &fakeProvisioner{res: provision.Result{TenantID: "t-new"}}, &fakeLifecycle{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants",
		strings.NewReader(`{"slug":"acme","name":"Acme","region":"eu-central","embedding_dim":768}`))
	h.Create(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("Create = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Tenant Tenant `json:"tenant"`
		JobID  string `json:"job_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Tenant.ID != "t-new" || out.JobID == "" {
		t.Fatalf("body = %+v", out)
	}
}

func TestAdminHandlerCreateInvalidBodyIs400(t *testing.T) {
	h := newAdminHandlers(&fakeAdminStore{}, &fakeProvisioner{}, &fakeLifecycle{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(`{bad`))
	h.Create(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Create bad json = %d, want 400", rr.Code)
	}
	assertEnvelope(t, rr, "validation")
}

func TestAdminHandlerCreateMissingSlugIs400(t *testing.T) {
	h := newAdminHandlers(&fakeAdminStore{}, &fakeProvisioner{}, &fakeLifecycle{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(`{"name":"Acme"}`))
	h.Create(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Create missing slug = %d, want 400", rr.Code)
	}
}

func TestAdminHandlerListReturns200Envelope(t *testing.T) {
	store := &fakeAdminStore{list: []Tenant{{ID: "a", CreatedAt: time.Now()}}}
	h := newAdminHandlers(store, &fakeProvisioner{}, &fakeLifecycle{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/tenants?limit=10", nil)
	h.List(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("List = %d, want 200", rr.Code)
	}
	var page TenantPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
}

func TestAdminHandlerListBadLimitIs400(t *testing.T) {
	h := newAdminHandlers(&fakeAdminStore{}, &fakeProvisioner{}, &fakeLifecycle{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/tenants?limit=nope", nil)
	h.List(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("List bad limit = %d, want 400", rr.Code)
	}
}

func TestAdminHandlerPatchStatus200(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{"t1": {ID: "t1", Slug: "acme", Status: "active"}}}
	life := &fakeLifecycle{}
	h := newAdminHandlers(store, &fakeProvisioner{}, life)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/admin/tenants/t1", strings.NewReader(`{"status":"suspended"}`))
	req.SetPathValue("id", "t1")
	h.Update(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Update = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if len(life.calls) != 1 || life.calls[0].op != "suspend" {
		t.Fatalf("lifecycle calls = %+v", life.calls)
	}
}

func TestAdminHandlerPatchUnknownTenant404(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{}}
	h := newAdminHandlers(store, &fakeProvisioner{}, &fakeLifecycle{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/admin/tenants/missing", strings.NewReader(`{"status":"active"}`))
	req.SetPathValue("id", "missing")
	h.Update(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("Update missing = %d, want 404", rr.Code)
	}
	assertEnvelope(t, rr, "not_found")
}

func TestAdminHandlerPatchIllegalTransitionIs409(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{"t1": {ID: "t1", Slug: "acme", Status: "suspended"}}}
	life := &fakeLifecycle{err: provision.ErrIllegalTransition}
	h := newAdminHandlers(store, &fakeProvisioner{}, life)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/admin/tenants/t1", strings.NewReader(`{"status":"suspended"}`))
	req.SetPathValue("id", "t1")
	h.Update(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("Update illegal transition = %d, want 409", rr.Code)
	}
	assertEnvelope(t, rr, "conflict")
}

func TestAdminHandlerDelete202(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{"t1": {ID: "t1", Slug: "acme", Status: "active"}}}
	life := &fakeLifecycle{}
	h := newAdminHandlers(store, &fakeProvisioner{}, life)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/admin/tenants/t1?grace=48h", nil)
	req.SetPathValue("id", "t1")
	h.Delete(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("Delete = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if len(life.calls) != 1 || life.calls[0].op != "schedule_delete" || life.calls[0].grace != 48*time.Hour {
		t.Fatalf("lifecycle calls = %+v", life.calls)
	}
}

func TestAdminHandlerDeleteBadGraceIs400(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{"t1": {ID: "t1", Slug: "acme"}}}
	h := newAdminHandlers(store, &fakeProvisioner{}, &fakeLifecycle{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/admin/tenants/t1?grace=nope", nil)
	req.SetPathValue("id", "t1")
	h.Delete(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Delete bad grace = %d, want 400", rr.Code)
	}
}

func TestAdminHandlerDeleteUnknown404(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{}}
	h := newAdminHandlers(store, &fakeProvisioner{}, &fakeLifecycle{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/admin/tenants/missing", nil)
	req.SetPathValue("id", "missing")
	h.Delete(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("Delete missing = %d, want 404", rr.Code)
	}
}

// assertEnvelope checks the SPEC-07 §1 error envelope shape and code.
func assertEnvelope(t *testing.T, rr *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q", env.Error.Code, wantCode)
	}
	if env.Error.Message == "" {
		t.Fatal("error message is empty")
	}
}
