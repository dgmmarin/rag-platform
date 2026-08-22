package tenants

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// SettingsHandlers are the HTTP entry points for GET/PATCH /v1/settings
// (FR-TEN-08, SPEC-02 §5). They are unit-tested with httptest here; STORY-04.1
// mounts them on the public router behind RequireSession + RequireRole
// (PermChangeSettings for PATCH). The tenant is always taken from the resolved
// context (FR-ACC-03), never from the request body or query.
type SettingsHandlers struct {
	Service *SettingsService
}

// NewSettingsHandlers builds handlers over a settings service.
func NewSettingsHandlers(svc *SettingsService) *SettingsHandlers {
	return &SettingsHandlers{Service: svc}
}

// Get returns the tenant's current settings document.
func (h *SettingsHandlers) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	doc, err := h.Service.Get(r.Context(), tid.String())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load settings")
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// Patch applies a partial update to the tenant's settings.
func (h *SettingsHandlers) Patch(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}

	var patch map[string]any
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	actor := Actor{}
	if sess, ok := auth.SessionFrom(r.Context()); ok && sess.UserID != "" {
		uid := sess.UserID
		actor.UserID = &uid
	}

	doc, err := h.Service.Patch(r.Context(), PatchParams{
		TenantID: tid.String(),
		Actor:    actor,
		Patch:    patch,
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, doc)
	case errors.Is(err, ErrImmutableField):
		writeErrorFields(w, http.StatusConflict, "embedding.dim is immutable; reindex to change",
			[]FieldError{{Field: "embedding.dim", Message: "immutable after provisioning"}})
	case errors.Is(err, ErrTenantNotFound):
		writeError(w, http.StatusNotFound, "tenant not found")
	default:
		var ve *ValidationErrors
		if errors.As(err, &ve) {
			writeErrorFields(w, http.StatusBadRequest, "settings failed validation", ve.Fields)
			return
		}
		writeError(w, http.StatusInternalServerError, "could not update settings")
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

func writeErrorFields(w http.ResponseWriter, code int, msg string, fields []FieldError) {
	writeJSON(w, code, map[string]any{"error": msg, "fields": fields})
}
