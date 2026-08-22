package auth

import "fmt"

// Role is a tenant-scoped role (SPEC-02 §4). It mirrors the control-plane
// tenant_role enum ('owner','admin','editor','viewer'). The zero value is an
// invalid role that grants no permission, so a mis-scanned or unset role fails
// closed.
type Role string

// The four tenant roles defined by SPEC-02 §4, in descending privilege.
const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Permission is a discrete authorised action from the SPEC-02 §4 matrix. It is
// what routes are keyed to, not the role directly, so the matrix stays the one
// place the rules live.
type Permission string

// The permissions that make up the rows of the SPEC-02 §4 matrix.
const (
	PermQuery           Permission = "query"            // query / retrieve
	PermUploadDocuments Permission = "upload_documents" // upload documents
	PermManageSources   Permission = "manage_sources"   // manage sources, trigger sync
	PermManageMembers   Permission = "manage_members"   // manage members, API keys
	PermChangeSettings  Permission = "change_settings"  // change tenant settings
	PermDeleteTenant    Permission = "delete_tenant"    // delete tenant (request)
)

// roleMatrix is the SPEC-02 §4 table encoded once. Absence of a (role,perm) key
// means "denied": every grant is explicit.
var roleMatrix = map[Role]map[Permission]bool{
	RoleOwner: {
		PermQuery:           true,
		PermUploadDocuments: true,
		PermManageSources:   true,
		PermManageMembers:   true,
		PermChangeSettings:  true,
		PermDeleteTenant:    true,
	},
	RoleAdmin: {
		PermQuery:           true,
		PermUploadDocuments: true,
		PermManageSources:   true,
		PermManageMembers:   true,
		PermChangeSettings:  true,
	},
	RoleEditor: {
		PermQuery:           true,
		PermUploadDocuments: true,
	},
	RoleViewer: {
		PermQuery: true,
	},
}

// Can reports whether the role is granted the permission per SPEC-02 §4. An
// unknown role grants nothing.
func (r Role) Can(p Permission) bool {
	return roleMatrix[r][p]
}

// Valid reports whether r is one of the four spec roles.
func (r Role) Valid() bool {
	_, ok := roleMatrix[r]
	return ok
}

// ParseRole validates untrusted role text (from an API payload or a DB scan) and
// returns the typed Role, rejecting anything outside the four spec roles so no
// invented role enters the system.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if !r.Valid() {
		return "", fmt.Errorf("auth: invalid role %q", s)
	}
	return r, nil
}
