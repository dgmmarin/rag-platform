package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// authzMemStore is an in-memory store for the authorization middleware: it maps
// (tenant,user) -> role and a set of platform-admin users, answering the exact
// role-lookup statement RequireRole issues.
type authzMemStore struct {
	roles     map[string]Role // key(tenant,user) -> role
	platAdmin map[string]bool // userID -> is_platform_admin
}

func newAuthzMemStore() *authzMemStore {
	return &authzMemStore{roles: map[string]Role{}, platAdmin: map[string]bool{}}
}

func (m *authzMemStore) Exec(context.Context, string, ...any) (pgconnTag, error) {
	return fakeTag{n: 0}, nil
}

func (m *authzMemStore) Query(context.Context, string, ...any) (Rows, error) {
	return nil, errNoRows{}
}

func (m *authzMemStore) QueryRow(_ context.Context, _ string, args ...any) Row {
	// The role lookup joins membership and the platform-admin flag in one query.
	tenantID, userID := args[0].(string), args[1].(string)
	role, isMember := m.roles[key(tenantID, userID)]
	plat := m.platAdmin[userID]
	if !isMember && !plat {
		// User row not found at all (mirrors the LEFT JOIN returning no user row).
		return fakeRow{err: errNoRows{}}
	}
	var roleVal any // nil role when the user is not a member (LEFT JOIN null)
	if isMember {
		roleVal = string(role)
	}
	return fakeRow{vals: []any{roleVal, plat}}
}

// authzHarness builds a RequireRole middleware backed by the in-memory store and
// returns a helper to drive a request as a given user against a given tenant with
// a required permission, reporting the status code and whether the inner handler
// ran.
type authzHarness struct {
	store *authzMemStore
	mw    *AuthzService
}

func newAuthzHarness() *authzHarness {
	store := newAuthzMemStore()
	return &authzHarness{store: store, mw: &AuthzService{DB: store}}
}

func (h *authzHarness) do(t *testing.T, userID, tenantID string, perm Permission) (int, bool) {
	t.Helper()
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	guarded := h.mw.RequireRole(perm)(inner)

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	ctx := req.Context()
	if userID != "" {
		ctx = ContextWithSession(ctx, Session{UserID: userID})
	}
	if tenantID != "" {
		ctx = tenant.WithTenantID(ctx, tenant.ID(uuid.MustParse(tenantID)))
	}
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	guarded.ServeHTTP(rr, req)
	return rr.Code, called
}

const (
	tnt = "11111111-1111-1111-1111-111111111111"
)

// TestRequireRoleNoSession proves a request without a session is 401 and the
// inner handler never runs.
func TestRequireRoleNoSession(t *testing.T) {
	h := newAuthzHarness()
	code, called := h.do(t, "", tnt, PermQuery)
	if code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", code)
	}
	if called {
		t.Fatal("inner handler ran without a session")
	}
}

// TestRequireRoleNoTenant proves a request without a resolved tenant is refused
// (there is no tenant to authorise against).
func TestRequireRoleNoTenant(t *testing.T) {
	h := newAuthzHarness()
	h.store.roles[key(tnt, "user-a")] = RoleOwner
	code, called := h.do(t, "user-a", "", PermQuery)
	if code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("no tenant: got %d, want 401/403", code)
	}
	if called {
		t.Fatal("inner handler ran without a resolved tenant")
	}
}

// TestRequireRoleNonMember proves a user who is not a member of the tenant is
// forbidden even for the lowest permission.
func TestRequireRoleNonMember(t *testing.T) {
	h := newAuthzHarness()
	code, called := h.do(t, "stranger", tnt, PermQuery)
	if code != http.StatusForbidden {
		t.Fatalf("non-member: got %d, want 403", code)
	}
	if called {
		t.Fatal("inner handler ran for a non-member")
	}
}

// TestRequireRolePlatformAdminBypass proves a platform admin is authorised for
// any permission on any tenant even without a membership (FR-ACC-07).
func TestRequireRolePlatformAdminBypass(t *testing.T) {
	h := newAuthzHarness()
	h.store.platAdmin["root"] = true
	code, called := h.do(t, "root", tnt, PermDeleteTenant)
	if code != http.StatusOK || !called {
		t.Fatalf("platform admin bypass: got %d called=%v, want 200 true", code, called)
	}
}

// TestRequireRoleMatrixPerRolePerPermission is the table-driven role × route
// matrix test: for every (role, permission) cell of SPEC-02 §4, a member with
// that role is allowed (200, inner ran) iff the matrix grants it, else 403.
func TestRequireRoleMatrixPerRolePerPermission(t *testing.T) {
	perms := []Permission{
		PermQuery, PermUploadDocuments, PermManageSources,
		PermManageMembers, PermChangeSettings, PermDeleteTenant,
	}
	roles := []Role{RoleOwner, RoleAdmin, RoleEditor, RoleViewer}

	for _, role := range roles {
		for _, perm := range perms {
			role, perm := role, perm
			t.Run(string(role)+"/"+string(perm), func(t *testing.T) {
				h := newAuthzHarness()
				userID := "u-" + string(role)
				h.store.roles[key(tnt, userID)] = role

				code, called := h.do(t, userID, tnt, perm)
				wantAllowed := role.Can(perm)
				if wantAllowed {
					if code != http.StatusOK || !called {
						t.Fatalf("role %q perm %q: got %d called=%v, want allowed", role, perm, code, called)
					}
				} else {
					if code != http.StatusForbidden || called {
						t.Fatalf("role %q perm %q: got %d called=%v, want 403 denied", role, perm, code, called)
					}
				}
			})
		}
	}
}
