package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// MembershipHandlers are the HTTP entry points for the session-admin members
// surface (STORY-11.5, ISSUE-0064, SPEC-02 §2). They are thin wrappers over the
// existing MembershipService — no CRUD logic is duplicated. The tenant is always
// taken from the resolved context (FR-ACC-03), never a request parameter; the
// router mounts them behind RequireSession -> RequireTenantAccess(perm).
type MembershipHandlers struct {
	Service *MembershipService
}

// NewMembershipHandlers builds handlers over a membership service.
func NewMembershipHandlers(svc *MembershipService) *MembershipHandlers {
	return &MembershipHandlers{Service: svc}
}

// memberView is one member as returned to the admin UI.
type memberView struct {
	UserID string `json:"userId"`
	Email  string `json:"email"`
	Role   string `json:"role"`
}

// List serves GET .../members: the tenant's roster (any member may read).
func (h *MembershipHandlers) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	members, err := h.Service.ListMembers(r.Context(), tid.String())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list members")
		return
	}
	out := make([]memberView, 0, len(members))
	for _, m := range members {
		out = append(out, memberView{UserID: m.UserID, Email: m.Email, Role: string(m.Role)})
	}
	writeJSONStatus(w, http.StatusOK, out)
}

// addMemberRequest is the POST .../members body: an existing user's email and the
// role to grant. There is no invite flow in this story (ISSUE-0064).
type addMemberRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// Add serves POST .../members: resolve the email to an existing user (404 if
// none), validate the role (400), then add the membership (409 on duplicate).
func (h *MembershipHandlers) Add(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	var req addMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	role, err := ParseRole(req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid role")
		return
	}
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	userID, err := h.Service.UserByEmail(r.Context(), req.Email)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "no user with that email; they must sign up first")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not add member")
		return
	}
	if err := h.Service.AddMember(r.Context(), tid.String(), userID, role); err != nil {
		if errors.Is(err, ErrAlreadyMember) {
			writeError(w, http.StatusConflict, "user is already a member")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not add member")
		return
	}
	writeJSONStatus(w, http.StatusCreated, memberView{UserID: userID, Email: req.Email, Role: string(role)})
}

// setRoleRequest is the PATCH .../members/{userId} body.
type setRoleRequest struct {
	Role string `json:"role"`
}

// SetRole serves PATCH .../members/{userId}: change an existing member's role.
// The last-owner guard (SPEC-02 §4) surfaces as 409; an unknown member is 404.
func (h *MembershipHandlers) SetRole(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	userID := r.PathValue("userId")
	var req setRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	role, err := ParseRole(req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid role")
		return
	}
	if err := h.Service.SetMemberRole(r.Context(), tid.String(), userID, role); err != nil {
		writeMembershipError(w, err, "could not change role")
		return
	}
	writeJSONStatus(w, http.StatusOK, memberView{UserID: userID, Role: string(role)})
}

// Remove serves DELETE .../members/{userId}: remove a member. The last-owner
// guard (SPEC-02 §4) surfaces as 409; an unknown member is 404.
func (h *MembershipHandlers) Remove(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	userID := r.PathValue("userId")
	if err := h.Service.RemoveMember(r.Context(), tid.String(), userID); err != nil {
		writeMembershipError(w, err, "could not remove member")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeMembershipError maps the membership sentinels to their HTTP statuses: a
// missing membership is 404, the last-owner invariant is 409, anything else 500.
func writeMembershipError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, ErrNotMember):
		writeError(w, http.StatusNotFound, "member not found")
	case errors.Is(err, ErrLastOwner):
		writeError(w, http.StatusConflict, "cannot remove or demote the last owner")
	default:
		writeError(w, http.StatusInternalServerError, fallback)
	}
}
