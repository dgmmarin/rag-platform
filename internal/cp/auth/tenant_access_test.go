package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rag-platform/ragctl/internal/cp/audit"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// tenantAccessHarness drives RequireTenantAccess over the same in-memory
// MembershipDB fake authz_test.go uses for RequireRole (authzMemStore), plus an
// audit capture so a test can assert the impersonation event RequireTenantAccess
// writes for a cross-tenant platform admin.
type tenantAccessHarness struct {
	store *authzMemStore
	svc   *AuthzService
	audit []audit.Event
}

func newTenantAccessHarness() *tenantAccessHarness {
	store := newAuthzMemStore()
	h := &tenantAccessHarness{store: store}
	h.svc = &AuthzService{DB: store, Audit: func(_ context.Context, e audit.Event) error {
		h.audit = append(h.audit, e)
		return nil
	}}
	return h
}

// do drives one request as userID against the {tenantId} path segment
// pathTenantID, requiring perm, and reports the response code, whether the
// inner handler ran, and (when it ran) the tenant id the inner handler observed
// via tenant.TenantIDFromCtx.
func (h *tenantAccessHarness) do(t *testing.T, userID, pathTenantID string, perm Permission) (code int, called bool, ctxTenant string) {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if tid, ok := tenant.TenantIDFromCtx(r.Context()); ok {
			ctxTenant = tid.String()
		}
		w.WriteHeader(http.StatusOK)
	})
	guarded := h.svc.RequireTenantAccess(perm)(inner)

	req := httptest.NewRequest(http.MethodGet, "/admin/tenants/"+pathTenantID+"/sources", nil)
	if userID != "" {
		req = req.WithContext(ContextWithSession(req.Context(), Session{UserID: userID}))
	}
	req.SetPathValue("tenantId", pathTenantID)

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	return rec.Code, called, ctxTenant
}

// TestRequireTenantAccessNoSession proves a request without a session is 401
// and the inner handler never runs.
func TestRequireTenantAccessNoSession(t *testing.T) {
	h := newTenantAccessHarness()
	code, called, _ := h.do(t, "", tnt, PermQuery)
	if code != http.StatusUnauthorized || called {
		t.Fatalf("no session: got %d called=%v, want 401 false", code, called)
	}
}

// TestRequireTenantAccessInvalidTenantID404 proves an unparseable tenant id is
// 404, not 400 — the surface never distinguishes a malformed id from an
// unknown one (ADR-0075).
func TestRequireTenantAccessInvalidTenantID404(t *testing.T) {
	h := newTenantAccessHarness()
	code, called, _ := h.do(t, "u-1", "not-a-uuid", PermQuery)
	if code != http.StatusNotFound || called {
		t.Fatalf("invalid tenant id: got %d called=%v, want 404 false", code, called)
	}
}

// TestRequireTenantAccessUnknownTenant404 proves a session user unknown to the
// store (no membership row, not a platform admin) is 404 for an otherwise
// well-formed tenant id.
func TestRequireTenantAccessUnknownTenant404(t *testing.T) {
	h := newTenantAccessHarness()
	code, called, _ := h.do(t, "stranger", tnt, PermQuery)
	if code != http.StatusNotFound || called {
		t.Fatalf("unknown tenant/non-member: got %d called=%v, want 404 false", code, called)
	}
}

// TestRequireTenantAccessNonMemberNonAdmin404 proves a known user who is simply
// not a member of this tenant (and not a platform admin) is 404, matching the
// unknown-tenant case exactly (no existence leak).
func TestRequireTenantAccessNonMemberNonAdmin404(t *testing.T) {
	h := newTenantAccessHarness()
	h.store.roles[key("22222222-2222-2222-2222-222222222222", "member-elsewhere")] = RoleOwner
	code, called, _ := h.do(t, "member-elsewhere", tnt, PermQuery)
	if code != http.StatusNotFound || called {
		t.Fatalf("non-member of this tenant: got %d called=%v, want 404 false", code, called)
	}
}

// TestRequireTenantAccessMemberRoleSatisfiesWrite proves a member whose role
// grants perm is allowed, without an impersonation audit event, and the inner
// handler observes the path tenant id via tenant.TenantIDFromCtx.
func TestRequireTenantAccessMemberRoleSatisfiesWrite(t *testing.T) {
	h := newTenantAccessHarness()
	h.store.roles[key(tnt, "admin-user")] = RoleAdmin
	code, called, ctxTenant := h.do(t, "admin-user", tnt, PermManageSources)
	if code != http.StatusOK || !called {
		t.Fatalf("member admin write: got %d called=%v, want 200 true", code, called)
	}
	if ctxTenant != tnt {
		t.Fatalf("inner handler saw tenant %q, want %q", ctxTenant, tnt)
	}
	if len(h.audit) != 0 {
		t.Fatalf("member access audited as impersonation: %+v", h.audit)
	}
}

// TestRequireTenantAccessMemberRoleLacksWrite403 proves a member (viewer) whose
// role does not grant perm (write) is 403.
func TestRequireTenantAccessMemberRoleLacksWrite403(t *testing.T) {
	h := newTenantAccessHarness()
	h.store.roles[key(tnt, "viewer-user")] = RoleViewer
	code, called, _ := h.do(t, "viewer-user", tnt, PermManageSources)
	if code != http.StatusForbidden || called {
		t.Fatalf("viewer write: got %d called=%v, want 403 false", code, called)
	}
}

// TestRequireTenantAccessPlatformAdminNonMemberAllowedAndAudited proves a
// platform admin who is NOT a member of the tenant is allowed for any perm, the
// inner handler observes the path tenant id, and the access is audited with
// details.impersonation=true (SPEC-02 §4, FR-ADM-05).
func TestRequireTenantAccessPlatformAdminNonMemberAllowedAndAudited(t *testing.T) {
	h := newTenantAccessHarness()
	h.store.platAdmin["root"] = true
	code, called, ctxTenant := h.do(t, "root", tnt, PermManageSources)
	if code != http.StatusOK || !called {
		t.Fatalf("platform admin non-member: got %d called=%v, want 200 true", code, called)
	}
	if ctxTenant != tnt {
		t.Fatalf("inner handler saw tenant %q, want %q", ctxTenant, tnt)
	}
	if len(h.audit) != 1 {
		t.Fatalf("got %d audit events, want 1", len(h.audit))
	}
	e := h.audit[0]
	if e.Action != "admin.tenant_access" || e.Details["impersonation"] != true {
		t.Fatalf("audit event = %+v, want action admin.tenant_access details.impersonation=true", e)
	}
	if e.TenantID == nil || *e.TenantID != tnt {
		t.Fatalf("audit event tenant = %v, want %q", e.TenantID, tnt)
	}
	if e.ActorUserID == nil || *e.ActorUserID != "root" {
		t.Fatalf("audit event actor = %v, want root", e.ActorUserID)
	}
}

// TestRequireTenantAccessPlatformAdminMemberNoImpersonation proves a platform
// admin who IS also a member of the tenant is allowed without an impersonation
// audit event (they are not acting across tenants in the FR-ADM-05 sense).
func TestRequireTenantAccessPlatformAdminMemberNoImpersonation(t *testing.T) {
	h := newTenantAccessHarness()
	h.store.platAdmin["root"] = true
	h.store.roles[key(tnt, "root")] = RoleViewer
	code, called, _ := h.do(t, "root", tnt, PermManageSources)
	if code != http.StatusOK || !called {
		t.Fatalf("platform admin + member: got %d called=%v, want 200 true", code, called)
	}
	if len(h.audit) != 0 {
		t.Fatalf("platform-admin member access audited: %+v", h.audit)
	}
}

// TestRequireTenantAccessAuditFailureFailsClosed proves that when the audit
// sink is not wired, a cross-tenant platform-admin access is refused rather than
// granted silently and unauditably.
func TestRequireTenantAccessAuditFailureFailsClosed(t *testing.T) {
	h := newTenantAccessHarness()
	h.svc.Audit = nil
	h.store.platAdmin["root"] = true
	code, called, _ := h.do(t, "root", tnt, PermManageSources)
	if code != http.StatusInternalServerError || called {
		t.Fatalf("no audit sink: got %d called=%v, want 500 false", code, called)
	}
}
