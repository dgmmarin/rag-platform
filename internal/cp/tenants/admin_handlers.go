package tenants

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/provision"
)

// AdminHandlers is the HTTP entry point for the platform-admin tenant API
// (STORY-04.6, FR-TEN-01/05/07, SPEC-07 §2). STORY-04.1's router mounts these
// under /admin behind RequireSession -> RequirePlatformAdmin, with CSRF on the
// mutating routes (POST/PATCH/DELETE) — the same platform-admin guard the audit
// and impersonation routes use. These are NOT tenant-scoped: a platform admin
// operates across tenants, so the tenant is a route/body value here, not derived
// from a per-request principal.
type AdminHandlers struct {
	Service *AdminService
}

// NewAdminHandlers builds handlers over an admin tenant service.
func NewAdminHandlers(svc *AdminService) *AdminHandlers { return &AdminHandlers{Service: svc} }

// Create serves POST /admin/tenants: enrol a tenant and return {tenant, job_id}.
func (h *AdminHandlers) Create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug         string `json:"slug"`
		Name         string `json:"name"`
		Region       string `json:"region"`
		EmbeddingDim int    `json:"embedding_dim"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := h.Service.Create(r.Context(), CreateTenantParams{
		Slug:         body.Slug,
		Name:         body.Name,
		Region:       body.Region,
		EmbeddingDim: body.EmbeddingDim,
	})
	if err != nil {
		writeServiceError(w, err, "could not create tenant")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tenant": res.Tenant, "job_id": res.JobID})
}

// List serves GET /admin/tenants?limit&cursor.
func (h *AdminHandlers) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := ListTenantsParams{Cursor: q.Get("cursor")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid 'limit'")
			return
		}
		p.Limit = n
	}
	page, err := h.Service.List(r.Context(), p)
	if err != nil {
		writeServiceError(w, err, "could not list tenants")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// Update serves PATCH /admin/tenants/{id}: status, db connection, and/or settings.
func (h *AdminHandlers) Update(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status     *string          `json:"status"`
		Connection *ConnectionPatch `json:"connection"`
		Settings   map[string]any   `json:"settings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	actor := Actor{}
	if sess, ok := auth.SessionFrom(r.Context()); ok && sess.UserID != "" {
		uid := sess.UserID
		actor.UserID = &uid
	}
	t, err := h.Service.Patch(r.Context(), PatchTenantParams{
		ID:         r.PathValue("id"),
		Status:     body.Status,
		Connection: body.Connection,
		Settings:   body.Settings,
		Actor:      actor,
	})
	if err != nil {
		writeServiceError(w, err, "could not update tenant")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// Delete serves DELETE /admin/tenants/{id}?grace: schedule deletion with grace.
// A missing/zero grace uses the lifecycle default (7 days, FR-TEN-05). It returns
// 202 (deletion scheduled) with the tenant now in `deleting` and its delete_after
// set; the irreversible teardown is the EPIC-09 River delete_tenant job.
func (h *AdminHandlers) Delete(w http.ResponseWriter, r *http.Request) {
	var grace time.Duration
	if v := r.URL.Query().Get("grace"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid 'grace' (want a Go duration like 168h)")
			return
		}
		grace = d
	}
	t, err := h.Service.Delete(r.Context(), r.PathValue("id"), grace)
	if err != nil {
		writeServiceError(w, err, "could not schedule tenant deletion")
		return
	}
	writeJSON(w, http.StatusAccepted, t)
}

// writeServiceError maps a service error to the SPEC-07 §1 envelope. Unknown
// tenants are 404; input errors (including a bad cursor, an empty patch, or a
// provisioner/lifecycle validation failure) are 400; an illegal status transition
// or an immutable-settings change is 409; anything else is a generic 500.
func writeServiceError(w http.ResponseWriter, err error, fallback string) {
	var av *adminValidationError
	var ve *ValidationErrors
	switch {
	case errors.As(err, &av):
		writeAPIError(w, http.StatusBadRequest, av.msg)
	case errors.Is(err, ErrTenantNotFound):
		writeAPIError(w, http.StatusNotFound, "tenant not found")
	case errors.As(err, &ve):
		writeAPIError(w, http.StatusBadRequest, "settings failed validation")
	case errors.Is(err, ErrImmutableField):
		writeAPIError(w, http.StatusConflict, "embedding.dim is immutable; reindex to change")
	case errors.Is(err, provision.ErrIllegalTransition):
		writeAPIError(w, http.StatusConflict, "illegal tenant status transition")
	case errors.Is(err, provision.ErrValidation):
		writeAPIError(w, http.StatusBadRequest, "invalid tenant request")
	default:
		writeAPIError(w, http.StatusInternalServerError, fallback)
	}
}

// writeAPIError writes the SPEC-07 §1 error envelope. The package's settings
// handlers use a legacy flat-error writer (writeError); the admin tenant surface
// speaks the one public error contract (ADR-0027), matching internal/cp/jobs and
// internal/cp/sources, so this is a separate writer.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
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
	case http.StatusConflict:
		return "conflict"
	default:
		return "internal"
	}
}
