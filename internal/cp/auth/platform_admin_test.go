package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// platAdminStore answers the tenant-less is_platform_admin lookup
// RequirePlatformAdmin issues: select is_platform_admin from users where id = $1.
type platAdminStore struct {
	admins map[string]bool
	known  map[string]bool
}

func (m platAdminStore) QueryRow(_ context.Context, _ string, args ...any) Row {
	userID := args[0].(string)
	if !m.known[userID] {
		return fakeRow{err: errNoRows{}}
	}
	return fakeRow{vals: []any{m.admins[userID]}}
}

func drivePlatformAdmin(t *testing.T, store platAdminStore, userID string) (int, bool) {
	t.Helper()
	svc := &AuthzService{DB: store}
	ran := false
	h := svc.RequirePlatformAdmin()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/admin/audit?tenant=t-1", nil)
	if userID != "" {
		req = req.WithContext(ContextWithSession(req.Context(), Session{UserID: userID}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, ran
}

func TestRequirePlatformAdminNoSession401(t *testing.T) {
	store := platAdminStore{admins: map[string]bool{}, known: map[string]bool{}}
	code, ran := drivePlatformAdmin(t, store, "")
	if code != http.StatusUnauthorized || ran {
		t.Fatalf("want 401 and inner not run, got %d ran=%v", code, ran)
	}
}

func TestRequirePlatformAdminNonAdmin403(t *testing.T) {
	store := platAdminStore{
		admins: map[string]bool{"u-1": false},
		known:  map[string]bool{"u-1": true},
	}
	code, ran := drivePlatformAdmin(t, store, "u-1")
	if code != http.StatusForbidden || ran {
		t.Fatalf("want 403 and inner not run, got %d ran=%v", code, ran)
	}
}

func TestRequirePlatformAdminAllowsAdmin(t *testing.T) {
	store := platAdminStore{
		admins: map[string]bool{"u-9": true},
		known:  map[string]bool{"u-9": true},
	}
	code, ran := drivePlatformAdmin(t, store, "u-9")
	if code != http.StatusOK || !ran {
		t.Fatalf("want 200 and inner run, got %d ran=%v", code, ran)
	}
}
