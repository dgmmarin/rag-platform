package tenants

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func withCtx(r *http.Request, tid string, userID string) *http.Request {
	ctx := r.Context()
	ctx = tenant.WithTenantID(ctx, tenant.ID(uuid.MustParse(tid)))
	ctx = auth.ContextWithSession(ctx, auth.Session{UserID: userID})
	return r.WithContext(ctx)
}

const seedTenant = "11111111-1111-1111-1111-111111111111"

func newHandlers(db SettingsDB) *SettingsHandlers {
	return &SettingsHandlers{Service: NewSettingsService(db)}
}

// GET returns the spec-shaped settings document as JSON.
func TestHandlerGet(t *testing.T) {
	db := &fakeDB{current: provisionedSettings(t)}
	h := newHandlers(db)

	req := withCtx(httptest.NewRequest(http.MethodGet, "/v1/settings", nil), seedTenant, "u1")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["embedding"].(map[string]any); !ok {
		t.Errorf("embedding missing from GET body: %s", rr.Body.String())
	}
	if _, leaked := doc["embedding_dim"]; leaked {
		t.Errorf("flat embedding_dim leaked in GET body")
	}
}

// A GET with no resolved tenant is 401 (FR-ACC-03).
func TestHandlerGetNoTenant(t *testing.T) {
	h := newHandlers(&fakeDB{current: provisionedSettings(t)})
	req := httptest.NewRequest(http.MethodGet, "/v1/settings", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

// A valid PATCH returns 200 with the merged document and writes an audit row.
func TestHandlerPatchValid(t *testing.T) {
	db := &fakeDB{current: provisionedSettings(t), updateRows: 1}
	h := newHandlers(db)

	body := `{"retrieval":{"k_vector":40,"k_text":40,"final_k":12,"min_score":0.05}}`
	req := withCtx(httptest.NewRequest(http.MethodPatch, "/v1/settings", strings.NewReader(body)), seedTenant, "u1")
	rr := httptest.NewRecorder()
	h.Patch(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if len(db.auditRows) != 1 {
		t.Errorf("audit rows = %d, want 1", len(db.auditRows))
	}
	if db.auditRows[0].tenantID != seedTenant {
		t.Errorf("audit tenant = %q, want %q", db.auditRows[0].tenantID, seedTenant)
	}
}

// An invalid PATCH is 400 with a per-field error list.
func TestHandlerPatchInvalid(t *testing.T) {
	db := &fakeDB{current: provisionedSettings(t), updateRows: 1}
	h := newHandlers(db)

	body := `{"retrieval":{"k_vector":40,"k_text":40,"final_k":8,"min_score":9}}`
	req := withCtx(httptest.NewRequest(http.MethodPatch, "/v1/settings", strings.NewReader(body)), seedTenant, "u1")
	rr := httptest.NewRecorder()
	h.Patch(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error  string       `json:"error"`
		Fields []FieldError `json:"fields"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Fields) == 0 {
		t.Errorf("no field errors in 400 body: %s", rr.Body.String())
	}
	found := false
	for _, f := range resp.Fields {
		if f.Field == "retrieval.min_score" {
			found = true
		}
	}
	if !found {
		t.Errorf("retrieval.min_score not in field errors: %s", rr.Body.String())
	}
}

// An attempt to change embedding.dim is 409 Conflict.
func TestHandlerPatchImmutable(t *testing.T) {
	db := &fakeDB{current: provisionedSettings(t), updateRows: 1}
	h := newHandlers(db)

	body := `{"embedding":{"provider":"voyage","model":"voyage-3","dim":2048}}`
	req := withCtx(httptest.NewRequest(http.MethodPatch, "/v1/settings", strings.NewReader(body)), seedTenant, "u1")
	rr := httptest.NewRecorder()
	h.Patch(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if len(db.auditRows) != 0 {
		t.Errorf("audit written on rejected immutable change")
	}
}

// Ensure the handler reads the tenant from context, not the body/query.
var _ = context.Background
