package auth

import (
	"encoding/json"
	"errors"
	"net/http"
)

// ImpersonationHandlers are the HTTP entry points for platform-admin impersonation
// (FR-ACC-07, SPEC-02 §4). STORY-04.1 mounts them behind RequireSession +
// RequirePlatformAdmin, so the handlers do not re-check authorization — they only
// act, reading the acting admin from the session (never a body field), so a caller
// cannot forge the actor. This mirrors how the audit handler assumes the
// middleware (ADR-0023).
type ImpersonationHandlers struct {
	Service *ImpersonationService
}

// NewImpersonationHandlers builds handlers over an impersonation service.
func NewImpersonationHandlers(svc *ImpersonationService) *ImpersonationHandlers {
	return &ImpersonationHandlers{Service: svc}
}

type startImpersonationRequest struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
}

type impersonationResponse struct {
	ID                 string `json:"id"`
	AdminUserID        string `json:"admin_user_id"`
	TenantID           string `json:"tenant_id"`
	ImpersonatedUserID string `json:"impersonated_user_id"`
	ExpiresAt          string `json:"expires_at"`
}

// Start opens an impersonation grant. The acting admin is taken from the session,
// not the body (FR-ACC-03/07): the grant and its audit event both attribute the
// real admin. A missing tenant/user is a 400; no session is a 401.
func (h *ImpersonationHandlers) Start(w http.ResponseWriter, r *http.Request) {
	sess, ok := SessionFrom(r.Context())
	if !ok || sess.UserID == "" {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req startImpersonationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TenantID == "" || req.UserID == "" {
		writeError(w, http.StatusBadRequest, "tenant_id and user_id are required")
		return
	}

	grant, err := h.Service.Start(r.Context(), sess.UserID, req.TenantID, req.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start impersonation")
		return
	}
	writeJSONStatus(w, http.StatusCreated, impersonationResponse{
		ID:                 grant.ID,
		AdminUserID:        grant.AdminUserID,
		TenantID:           grant.TenantID,
		ImpersonatedUserID: grant.ImpersonatedUserID,
		ExpiresAt:          grant.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	})
}

// End revokes an impersonation grant named by the {id} path value. Unknown id is a
// 404; success is a 204. It is idempotent at the store layer (ended_at is stamped
// only if null) and writes an admin.impersonate.end audit event.
func (h *ImpersonationHandlers) End(w http.ResponseWriter, r *http.Request) {
	sess, ok := SessionFrom(r.Context())
	if !ok || sess.UserID == "" {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "impersonation id is required")
		return
	}
	err := h.Service.End(r.Context(), id, sess.UserID)
	switch {
	case errors.Is(err, ErrNoImpersonation):
		writeError(w, http.StatusNotFound, "no such impersonation")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not end impersonation")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// writeJSONStatus writes a JSON body with the given status.
func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
