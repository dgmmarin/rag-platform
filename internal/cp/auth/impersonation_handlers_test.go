package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The handlers assume RequirePlatformAdmin ran upstream, so they read the acting
// admin from the session (never a body field) — the impersonation grant records
// that real admin as the actor.

func newImpHandlers(t *testing.T, db *impDB) (*ImpersonationHandlers, *captureAudit) {
	t.Helper()
	aud := &captureAudit{}
	svc := &ImpersonationService{DB: db, Now: fixedClock(time.Now()), Audit: aud.fn()}
	return &ImpersonationHandlers{Service: svc}, aud
}

func TestStartImpersonationUsesSessionAdminNotBody(t *testing.T) {
	db := &impDB{rows: []fakeRow{{vals: []any{"imp-1"}}}}
	h, aud := newImpHandlers(t, db)

	body := `{"tenant_id":"tenant-9","user_id":"member-3","admin_user_id":"attacker"}`
	r := httptest.NewRequest(http.MethodPost, "/admin/impersonations", strings.NewReader(body))
	r = r.WithContext(ContextWithSession(r.Context(), Session{UserID: "admin-1"}))
	rr := httptest.NewRecorder()
	h.Start(rr, r)

	if rr.Code != http.StatusCreated {
		t.Fatalf("Start = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if len(aud.events) != 1 || aud.events[0].ActorUserID == nil || *aud.events[0].ActorUserID != "admin-1" {
		t.Fatalf("actor must be the session admin, not the body; events=%+v", aud.events)
	}
	var resp struct {
		ID          string `json:"id"`
		AdminUserID string `json:"admin_user_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.AdminUserID != "admin-1" {
		t.Fatalf("response admin = %q, want admin-1", resp.AdminUserID)
	}
}

func TestStartImpersonationNoSession401(t *testing.T) {
	db := &impDB{}
	h, _ := newImpHandlers(t, db)
	r := httptest.NewRequest(http.MethodPost, "/admin/impersonations",
		strings.NewReader(`{"tenant_id":"t","user_id":"u"}`))
	rr := httptest.NewRecorder()
	h.Start(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("Start no session = %d, want 401", rr.Code)
	}
}

func TestStartImpersonationMissingFields400(t *testing.T) {
	db := &impDB{}
	h, _ := newImpHandlers(t, db)
	r := httptest.NewRequest(http.MethodPost, "/admin/impersonations",
		strings.NewReader(`{"tenant_id":"t"}`))
	r = r.WithContext(ContextWithSession(r.Context(), Session{UserID: "admin-1"}))
	rr := httptest.NewRecorder()
	h.Start(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Start missing user = %d, want 400", rr.Code)
	}
}

func TestEndImpersonationUnknown404(t *testing.T) {
	db := &impDB{rows: []fakeRow{{err: errNoRows{}}}}
	h, _ := newImpHandlers(t, db)
	r := httptest.NewRequest(http.MethodDelete, "/admin/impersonations/nope", nil)
	r = r.WithContext(ContextWithSession(r.Context(), Session{UserID: "admin-1"}))
	r.SetPathValue("id", "nope")
	rr := httptest.NewRecorder()
	h.End(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("End unknown = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestEndImpersonationOK(t *testing.T) {
	db := &impDB{rows: []fakeRow{{vals: []any{"admin-1", "tenant-9", "member-3"}}}}
	h, aud := newImpHandlers(t, db)
	r := httptest.NewRequest(http.MethodDelete, "/admin/impersonations/imp-1", nil)
	r = r.WithContext(ContextWithSession(r.Context(), Session{UserID: "admin-1"}))
	r.SetPathValue("id", "imp-1")
	rr := httptest.NewRecorder()
	h.End(rr, r)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("End = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if len(aud.events) != 1 || aud.events[0].Action != "admin.impersonate.end" {
		t.Fatalf("End must audit admin.impersonate.end; got %+v", aud.events)
	}
}
