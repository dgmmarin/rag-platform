//go:build e2e

// STORY-03.3 golden path: tenant membership, roles, and the role matrix against
// the REAL control-plane Postgres (up via `mise run up`), no mocks. It drives the
// real auth.MembershipService and auth.AuthzService through:
//   - add members with each of the four spec roles,
//   - list them (ordered, with emails joined),
//   - change a role,
//   - remove a member,
//   - reject removing/demoting the LAST owner (the SPEC-02 §4 invariant),
//
// and proves the SPEC-02 §4 role × route matrix end to end: for each role, the
// RequireRole middleware allows exactly the permitted permissions and 403s the
// rest, keyed off the authenticated session user and the resolved tenant.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestMembershipAndRoleMatrixGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)
	msvc := auth.NewMembershipService(auth.MembershipFromPool(pool))
	authz := auth.NewAuthzService(auth.MembershipFromPool(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "mem-" + suffix

	// --- Seed a tenant and one user per role plus a stranger. ---
	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region) values ($1, $2, 'active', 'eu-central') returning id::text`,
		slug, "Membership Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	users := map[string]string{} // role name -> user id
	mkUser := func(label string) string {
		email := fmt.Sprintf("mem-%s-%s@example.test", label, suffix)
		var id string
		if err := pool.QueryRow(ctx,
			`insert into users (email) values ($1) returning id::text`, email).Scan(&id); err != nil {
			t.Fatalf("seed user %s: %v", label, err)
		}
		t.Cleanup(func() {
			user := hostPort("POSTGRES_USER", "rag")
			_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM users WHERE id = '%s'", id))
		})
		return id
	}
	for _, label := range []string{"owner", "admin", "editor", "viewer", "stranger", "owner2"} {
		users[label] = mkUser(label)
	}

	// --- AddMember for each role. ---
	roleByLabel := map[string]auth.Role{
		"owner": auth.RoleOwner, "admin": auth.RoleAdmin,
		"editor": auth.RoleEditor, "viewer": auth.RoleViewer,
	}
	for label, role := range roleByLabel {
		if err := msvc.AddMember(ctx, tenantID, users[label], role); err != nil {
			t.Fatalf("add %s: %v", label, err)
		}
	}
	// A duplicate add is rejected.
	if err := msvc.AddMember(ctx, tenantID, users["owner"], auth.RoleAdmin); err == nil {
		t.Fatal("duplicate add succeeded; want ErrAlreadyMember")
	}

	// --- ListMembers: four members, joined to emails, ordered by email. ---
	members, err := msvc.ListMembers(ctx, tenantID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 4 {
		t.Fatalf("listed %d members, want 4", len(members))
	}
	for i := 1; i < len(members); i++ {
		if members[i-1].Email > members[i].Email {
			t.Fatalf("members not ordered by email: %q before %q", members[i-1].Email, members[i].Email)
		}
	}

	// --- SetMemberRole: promote the viewer to editor. ---
	if err := msvc.SetMemberRole(ctx, tenantID, users["viewer"], auth.RoleEditor); err != nil {
		t.Fatalf("set role: %v", err)
	}
	got := psqlScalar(t, fmt.Sprintf(
		"select role from tenant_members where tenant_id = '%s' and user_id = '%s'", tenantID, users["viewer"]))
	if got != "editor" {
		t.Fatalf("role after change = %q, want editor", got)
	}

	// --- RemoveMember: remove the admin. ---
	if err := msvc.RemoveMember(ctx, tenantID, users["admin"]); err != nil {
		t.Fatalf("remove admin: %v", err)
	}
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from tenant_members where tenant_id = '%s' and user_id = '%s'", tenantID, users["admin"])); n != "0" {
		t.Fatalf("admin still a member after removal (count %q)", n)
	}

	// --- Last-owner invariant: removing the sole owner is refused. ---
	if err := msvc.RemoveMember(ctx, tenantID, users["owner"]); err == nil {
		t.Fatal("removing the last owner succeeded; want ErrLastOwner")
	}
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from tenant_members where tenant_id = '%s' and role = 'owner'", tenantID)); n != "1" {
		t.Fatalf("owner count = %q after blocked removal, want 1", n)
	}
	// Demoting the sole owner is likewise refused.
	if err := msvc.SetMemberRole(ctx, tenantID, users["owner"], auth.RoleAdmin); err == nil {
		t.Fatal("demoting the last owner succeeded; want ErrLastOwner")
	}
	// With a SECOND owner present, removing one is allowed.
	if err := msvc.AddMember(ctx, tenantID, users["owner2"], auth.RoleOwner); err != nil {
		t.Fatalf("add second owner: %v", err)
	}
	if err := msvc.RemoveMember(ctx, tenantID, users["owner2"]); err != nil {
		t.Fatalf("remove one of two owners: %v", err)
	}

	// --- Role × route matrix via the real RequireRole middleware. ---
	// Restore a clean role set: owner, admin, editor, viewer.
	if err := msvc.AddMember(ctx, tenantID, users["admin"], auth.RoleAdmin); err != nil {
		t.Fatalf("re-add admin: %v", err)
	}
	if err := msvc.SetMemberRole(ctx, tenantID, users["viewer"], auth.RoleViewer); err != nil {
		t.Fatalf("reset viewer role: %v", err)
	}

	tid := tenant.ID(uuid.MustParse(tenantID))
	perms := []auth.Permission{
		auth.PermQuery, auth.PermUploadDocuments, auth.PermManageSources,
		auth.PermManageMembers, auth.PermChangeSettings, auth.PermDeleteTenant,
	}
	for label, role := range roleByLabel {
		for _, perm := range perms {
			called := false
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			guarded := authz.RequireRole(perm)(inner)

			req := httptest.NewRequest(http.MethodPost, "/x", nil)
			rctx := auth.ContextWithSession(req.Context(), auth.Session{UserID: users[label]})
			rctx = tenant.WithTenantID(rctx, tid)
			req = req.WithContext(rctx)
			rr := httptest.NewRecorder()
			guarded.ServeHTTP(rr, req)

			want := role.Can(perm)
			if want {
				if rr.Code != http.StatusOK || !called {
					t.Fatalf("%s/%s: got %d called=%v, want allowed", role, perm, rr.Code, called)
				}
			} else {
				if rr.Code != http.StatusForbidden || called {
					t.Fatalf("%s/%s: got %d called=%v, want 403", role, perm, rr.Code, called)
				}
			}
		}
	}

	// A stranger (not a member) is forbidden even for query.
	{
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		guarded := authz.RequireRole(auth.PermQuery)(inner)
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		rctx := auth.ContextWithSession(req.Context(), auth.Session{UserID: users["stranger"]})
		rctx = tenant.WithTenantID(rctx, tid)
		req = req.WithContext(rctx)
		rr := httptest.NewRecorder()
		guarded.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("stranger query: got %d, want 403", rr.Code)
		}
	}
}
