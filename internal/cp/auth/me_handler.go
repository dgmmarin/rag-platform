package auth

import (
	"encoding/json"
	"net/http"
)

// MeHandlers serves GET /v1/auth/me: the admin UI's session-hydration endpoint
// (SPEC-11 §2.1). Session required, no platform-admin gate.
type MeHandlers struct {
	Service *MeService
}

// Me returns the caller's identity, platform-admin flag, tenant memberships,
// and CSRF token (mirroring the login response contract). The session token
// itself is never logged or returned.
func (h *MeHandlers) Me(w http.ResponseWriter, r *http.Request) {
	sess, ok := SessionFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no session")
		return
	}
	v, err := h.Service.Me(r.Context(), sess.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session user")
		return
	}
	type membershipJSON struct {
		TenantID string `json:"tenant_id"`
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Role     string `json:"role"`
	}
	out := struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
		IsPlatformAdmin bool             `json:"is_platform_admin"`
		Memberships     []membershipJSON `json:"memberships"`
		CSRFToken       string           `json:"csrf_token"`
	}{CSRFToken: sess.CSRFToken, IsPlatformAdmin: v.IsPlatformAdmin}
	out.User.ID = v.User.ID
	out.User.Email = v.User.Email
	for _, m := range v.Memberships {
		out.Memberships = append(out.Memberships, membershipJSON{m.TenantID, m.Slug, m.Name, string(m.Role)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
