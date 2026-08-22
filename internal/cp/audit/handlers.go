package audit

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Handlers is the HTTP entry point for the audit read API (FR-ADM-05, SPEC-02 §6).
// It is unit-tested with httptest here; STORY-04.1 mounts List on the public
// router behind RequireSession + RequirePlatformAdmin, so the handler itself does
// not re-check authorization — it only reads the requested tenant and returns the
// page. The tenant is a query parameter (not the resolved credential tenant)
// because a platform admin reads any tenant's history (FR-ACC-07).
type Handlers struct {
	Service *Service
}

// NewHandlers builds handlers over an audit reader.
func NewHandlers(svc *Service) *Handlers { return &Handlers{Service: svc} }

// List serves GET /admin/audit?tenant=<id>&limit=<n>&before=<id>. A missing
// tenant is a 400 (the read is always tenant-scoped); malformed limit/before are
// ignored and fall back to defaults.
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "tenant query parameter is required")
		return
	}

	p := ListParams{TenantID: tenant}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.Limit = n
		}
	}
	if v := r.URL.Query().Get("before"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			p.BeforeID = n
		}
	}

	entries, err := h.Service.List(r.Context(), p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read audit log")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the SPEC-07 §1 error envelope so the router-mounted audit
// handler speaks the one public error contract (ADR-0027).
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": errorCodeForStatus(status), "message": msg},
	})
}

func errorCodeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusBadRequest:
		return "validation"
	default:
		return "internal"
	}
}
