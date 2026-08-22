package usage

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// dateLayout is the from/to query-parameter format for GET /v1/usage (SPEC-07).
const dateLayout = "2006-01-02"

// Handlers is the HTTP entry point for the usage read API (FR-ADM-06, SPEC-07).
// It is unit-tested with httptest here; STORY-04.1 mounts List on the public
// router behind RequireSession + RequireRole (admin scope, SPEC-07 §GET /v1/usage).
// The tenant is always taken from the resolved context (FR-ACC-03), never from a
// request parameter.
type Handlers struct {
	Service *Service
}

// NewHandlers builds handlers over a usage reader.
func NewHandlers(svc *Service) *Handlers { return &Handlers{Service: svc} }

// List serves GET /v1/usage?from=YYYY-MM-DD&to=YYYY-MM-DD. The tenant is the
// resolved-credential tenant (401 if none). A malformed or inverted range is a
// 400; an absent range falls back to the reader's default window.
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}

	p := ListParams{TenantID: tid.String()}
	if v := r.URL.Query().Get("from"); v != "" {
		d, err := time.Parse(dateLayout, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'from' date; use YYYY-MM-DD")
			return
		}
		p.From = d
	}
	if v := r.URL.Query().Get("to"); v != "" {
		d, err := time.Parse(dateLayout, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'to' date; use YYYY-MM-DD")
			return
		}
		p.To = d
	}

	rows, err := h.Service.List(r.Context(), p)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"usage": rows})
	case errors.Is(err, ErrInvalidRange):
		writeError(w, http.StatusBadRequest, "'from' must not be after 'to'")
	default:
		writeError(w, http.StatusInternalServerError, "could not read usage")
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
