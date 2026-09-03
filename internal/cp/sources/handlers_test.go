package sources

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/tenant"
)

func ctxWithTenant(ctx context.Context) context.Context {
	return tenant.WithTenantID(ctx, tenant.ID(uuid.MustParse(tenantA)))
}

func newTestHandlers(st *fakeStore) *Handlers {
	svc := NewService(st)
	svc.now = st.nowFunc
	return NewHandlers(svc)
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

func TestHandlerCreateRequiresTenant(t *testing.T) {
	h := newTestHandlers(newFakeStore())
	r := httptest.NewRequest(http.MethodPost, "/v1/sources", strings.NewReader(`{"kind":"web_crawl","name":"n"}`))
	rr := httptest.NewRecorder()
	h.Create(rr, r) // no tenant in context
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "unauthorized" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerCreateRejectsCredentials(t *testing.T) {
	// Credential handling is STORY-06.2; the create body must reject credentials
	// rather than silently drop or store plaintext (fail closed, C-4).
	h := newTestHandlers(newFakeStore())
	body := `{"kind":"api","name":"n","credentials":{"token":"secret"}}`
	r := httptest.NewRequest(http.MethodPost, "/v1/sources", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.Create(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "validation" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerCreateSuccess(t *testing.T) {
	st := newFakeStore()
	h := newTestHandlers(st)
	body := `{"kind":"web_crawl","name":"docs","config":{"start_urls":["https://x/"]}}`
	r := httptest.NewRequest(http.MethodPost, "/v1/sources", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.Create(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	var s Source
	if err := json.Unmarshal(rr.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.ID == "" || s.Name != "docs" {
		t.Fatalf("unexpected source: %+v", s)
	}
	// Credentials must never appear in a response body (FR-SRC-10).
	if strings.Contains(rr.Body.String(), "credential") {
		t.Fatalf("response leaks credentials: %s", rr.Body.String())
	}
}

func TestHandlerCreateDuplicate409(t *testing.T) {
	st := newFakeStore()
	h := newTestHandlers(st)
	mk := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/sources", strings.NewReader(`{"kind":"web_crawl","name":"dup"}`))
		rr := httptest.NewRecorder()
		h.Create(rr, r.WithContext(ctxWithTenant(r.Context())))
		return rr
	}
	if rr := mk(); rr.Code != http.StatusCreated {
		t.Fatalf("first create: %d", rr.Code)
	}
	rr := mk()
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "conflict" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerGetNotFound(t *testing.T) {
	h := newTestHandlers(newFakeStore())
	r := httptest.NewRequest(http.MethodGet, "/v1/sources/"+uuid.NewString(), nil)
	r.SetPathValue("id", uuid.NewString())
	rr := httptest.NewRecorder()
	h.Get(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "not_found" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerListReturnsItemsEnvelope(t *testing.T) {
	st := newFakeStore()
	h := newTestHandlers(st)
	svc := h.Service
	if _, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/sources", nil)
	rr := httptest.NewRecorder()
	h.List(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var page Page
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(page.Items))
	}
}

func TestHandlerSyncConflict(t *testing.T) {
	st := newFakeStore()
	h := newTestHandlers(st)
	s, _ := h.Service.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})

	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/sources/"+s.ID+"/sync", strings.NewReader(`{"full":true}`))
		r.SetPathValue("id", s.ID)
		rr := httptest.NewRecorder()
		h.Sync(rr, r.WithContext(ctxWithTenant(r.Context())))
		return rr
	}
	if rr := do(); rr.Code != http.StatusAccepted {
		t.Fatalf("first sync: want 202, got %d (%s)", rr.Code, rr.Body.String())
	}
	rr := do()
	if rr.Code != http.StatusConflict {
		t.Fatalf("second sync: want 409, got %d", rr.Code)
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "conflict" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerSyncIdempotencyKey(t *testing.T) {
	st := newFakeStore()
	h := newTestHandlers(st)
	s, _ := h.Service.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})

	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/sources/"+s.ID+"/sync", strings.NewReader(`{}`))
		r.SetPathValue("id", s.ID)
		r.Header.Set("Idempotency-Key", "key-1")
		rr := httptest.NewRecorder()
		h.Sync(rr, r.WithContext(ctxWithTenant(r.Context())))
		return rr
	}
	rr1 := do()
	rr2 := do()
	if rr1.Code != http.StatusAccepted || rr2.Code != http.StatusAccepted {
		t.Fatalf("idempotent sync codes: %d, %d", rr1.Code, rr2.Code)
	}
	var j1, j2 Job
	_ = json.Unmarshal(rr1.Body.Bytes(), &j1)
	_ = json.Unmarshal(rr2.Body.Bytes(), &j2)
	if j1.ID != j2.ID {
		t.Fatalf("idempotency-key did not replay: %s vs %s", j1.ID, j2.ID)
	}
}

func TestHandlerTestConnectionSeam(t *testing.T) {
	// No validator wired: /test returns the not_found seam envelope (EPIC-06).
	st := newFakeStore()
	h := newTestHandlers(st)
	s, _ := h.Service.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	r := httptest.NewRequest(http.MethodPost, "/v1/sources/"+s.ID+"/test", nil)
	r.SetPathValue("id", s.ID)
	rr := httptest.NewRecorder()
	h.Test(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404 seam, got %d (%s)", rr.Code, rr.Body.String())
	}
	if code := decodeEnvelope(t, rr.Body.Bytes()); code != "not_found" {
		t.Fatalf("code=%q", code)
	}
}

func TestHandlerDeleteAccepted(t *testing.T) {
	st := newFakeStore()
	h := newTestHandlers(st)
	s, _ := h.Service.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	r := httptest.NewRequest(http.MethodDelete, "/v1/sources/"+s.ID, nil)
	r.SetPathValue("id", s.ID)
	rr := httptest.NewRecorder()
	h.Delete(rr, r.WithContext(ctxWithTenant(r.Context())))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d (%s)", rr.Code, rr.Body.String())
	}
}
