package auth

import "testing"

// TestRoleMatrixMatchesSpec pins the SPEC-02 §4 role → permission matrix. Each
// (role, permission) cell must match the spec exactly; this is the single source
// of truth the RequireRole middleware and the membership service consult.
func TestRoleMatrixMatchesSpec(t *testing.T) {
	// want[perm][role] mirrors the table in SPEC-02 §4 verbatim.
	want := map[Permission]map[Role]bool{
		PermQuery: {
			RoleOwner: true, RoleAdmin: true, RoleEditor: true, RoleViewer: true,
		},
		PermUploadDocuments: {
			RoleOwner: true, RoleAdmin: true, RoleEditor: true, RoleViewer: false,
		},
		PermManageSources: {
			RoleOwner: true, RoleAdmin: true, RoleEditor: false, RoleViewer: false,
		},
		PermManageMembers: {
			RoleOwner: true, RoleAdmin: true, RoleEditor: false, RoleViewer: false,
		},
		PermChangeSettings: {
			RoleOwner: true, RoleAdmin: true, RoleEditor: false, RoleViewer: false,
		},
		PermDeleteTenant: {
			RoleOwner: true, RoleAdmin: false, RoleEditor: false, RoleViewer: false,
		},
	}

	for perm, byRole := range want {
		for role, allowed := range byRole {
			if got := role.Can(perm); got != allowed {
				t.Errorf("Role(%q).Can(%q) = %v, want %v", role, perm, got, allowed)
			}
		}
	}
}

// TestParseRoleRejectsUnknown proves only the four spec roles are accepted; no
// invented roles slip in via the database or an API payload.
func TestParseRoleRejectsUnknown(t *testing.T) {
	for _, ok := range []string{"owner", "admin", "editor", "viewer"} {
		if _, err := ParseRole(ok); err != nil {
			t.Errorf("ParseRole(%q) errored: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "OWNER", "superadmin", "root", "guest"} {
		if _, err := ParseRole(bad); err == nil {
			t.Errorf("ParseRole(%q) accepted an unknown role", bad)
		}
	}
}

// TestUnknownRoleCanNothing proves a zero/garbage role grants no permission, so
// a mis-scanned role never fails open.
func TestUnknownRoleCanNothing(t *testing.T) {
	var zero Role
	for _, p := range []Permission{PermQuery, PermUploadDocuments, PermManageSources, PermManageMembers, PermChangeSettings, PermDeleteTenant} {
		if zero.Can(p) {
			t.Errorf("zero Role granted %q", p)
		}
	}
}
