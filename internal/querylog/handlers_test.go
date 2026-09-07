package querylog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/tenant"
)

func withTenant(r *http.Request, tid string) *http.Request {
	return r.WithContext(tenant.WithTenantID(r.Context(), tenant.ID(uuid.MustParse(tid))))
}

func TestFeedbackHandlerNoTenant(t *testing.T) {
	h := NewHandlers(&Service{Resolver: &fakeResolver{}, Store: &fakeStore{}})
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback",
		strings.NewReader(`{"query_id":"q_`+uuid.NewString()+`","rating":1}`))
	rec := httptest.NewRecorder()
	h.Feedback(rec, req) // no tenant in context
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestFeedbackHandlerSuccess(t *testing.T) {
	store := &fakeStore{}
	h := NewHandlers(&Service{Resolver: &fakeResolver{}, Store: store})
	qid := "q_" + uuid.NewString()
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/feedback",
		strings.NewReader(`{"query_id":"`+qid+`","rating":-1,"comment":"nope"}`)), uuid.NewString())
	rec := httptest.NewRecorder()
	h.Feedback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if len(store.feedback) != 1 || store.feedback[0].Rating != -1 || store.feedback[0].Comment != "nope" {
		t.Fatalf("feedback = %+v", store.feedback)
	}
}

func TestFeedbackHandlerBadRating(t *testing.T) {
	h := NewHandlers(&Service{Resolver: &fakeResolver{}, Store: &fakeStore{}})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/feedback",
		strings.NewReader(`{"query_id":"q_`+uuid.NewString()+`","rating":7}`)), uuid.NewString())
	rec := httptest.NewRecorder()
	h.Feedback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFeedbackHandlerUnknownQuery(t *testing.T) {
	h := NewHandlers(&Service{Resolver: &fakeResolver{}, Store: &fakeStore{feedbackErr: ErrQueryNotFound}})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/feedback",
		strings.NewReader(`{"query_id":"q_`+uuid.NewString()+`","rating":1}`)), uuid.NewString())
	rec := httptest.NewRecorder()
	h.Feedback(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestFeedbackHandlerMalformedBody(t *testing.T) {
	h := NewHandlers(&Service{Resolver: &fakeResolver{}, Store: &fakeStore{}})
	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/feedback",
		strings.NewReader(`{not json`)), uuid.NewString())
	rec := httptest.NewRecorder()
	h.Feedback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestListHandlerNoTenant(t *testing.T) {
	h := NewHandlers(&Service{Resolver: &fakeResolver{}, Store: &fakeStore{}})
	req := httptest.NewRequest(http.MethodGet, "/v1/queries", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestListHandlerReturnsPage(t *testing.T) {
	store := &fakeStore{entries: []Entry{{ID: "q_" + uuid.NewString(), Question: "hi"}}}
	h := NewHandlers(&Service{Resolver: &fakeResolver{}, Store: store})
	req := withTenant(httptest.NewRequest(http.MethodGet, "/v1/queries?limit=10", nil), uuid.NewString())
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var page struct {
		Items      []Entry `json:"items"`
		NextCursor string  `json:"next_cursor"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Question != "hi" {
		t.Fatalf("items = %+v", page.Items)
	}
}

func TestListHandlerBadLimit(t *testing.T) {
	h := NewHandlers(&Service{Resolver: &fakeResolver{}, Store: &fakeStore{}})
	req := withTenant(httptest.NewRequest(http.MethodGet, "/v1/queries?limit=-4", nil), uuid.NewString())
	rec := httptest.NewRecorder()
	h.List(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
