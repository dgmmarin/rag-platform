package sources

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Handlers is the HTTP entry point for the sources API (FR-SRC-01/14, SPEC-07 §2).
// The tenant is always taken from the resolved context (FR-ACC-03), never a
// request parameter. STORY-04.1's router mounts these behind
// RequireScopeAdmin -> RateLimit (all sources routes are the `admin` scope).
type Handlers struct {
	Service *Service
}

// NewHandlers builds handlers over a sources service.
func NewHandlers(svc *Service) *Handlers { return &Handlers{Service: svc} }

// createRequest is the POST /v1/sources body. `credentials` is accepted only to
// reject it: credential handling is STORY-06.2, so accepting it here would risk
// storing plaintext (C-4). Failing closed is safer than silently dropping it.
type createRequest struct {
	Kind         string          `json:"kind"`
	Name         string          `json:"name"`
	Config       json.RawMessage `json:"config,omitempty"`
	ScheduleCron *string         `json:"schedule_cron,omitempty"`
	Credentials  json.RawMessage `json:"credentials,omitempty"`
}

// updateRequest is the PATCH /v1/sources/{id} body. Every field is optional; a
// field present with a null value for schedule_cron clears it (manual-only).
type updateRequest struct {
	Name         *string          `json:"name,omitempty"`
	Config       *json.RawMessage `json:"config,omitempty"`
	Status       *string          `json:"status,omitempty"`
	ScheduleCron *json.RawMessage `json:"schedule_cron,omitempty"`
	Credentials  json.RawMessage  `json:"credentials,omitempty"`
}

// syncRequest is the POST /v1/sources/{id}/sync body.
type syncRequest struct {
	Full bool `json:"full"`
}

// List serves GET /v1/sources?limit&cursor.
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	p := ListParams{TenantID: tid.String(), Cursor: r.URL.Query().Get("cursor")}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid 'limit'")
			return
		}
		p.Limit = n
	}
	page, err := h.Service.List(r.Context(), p)
	if err != nil {
		writeServiceError(w, err, "could not list sources")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// Create serves POST /v1/sources.
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	var req createRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Credentials) > 0 {
		writeError(w, http.StatusBadRequest, "credential handling is not available yet (STORY-06.2); omit 'credentials'")
		return
	}
	src, err := h.Service.Create(r.Context(), CreateParams{
		TenantID:     tid.String(),
		Kind:         req.Kind,
		Name:         req.Name,
		Config:       req.Config,
		ScheduleCron: req.ScheduleCron,
	})
	if err != nil {
		writeServiceError(w, err, "could not create source")
		return
	}
	writeJSON(w, http.StatusCreated, src)
}

// Get serves GET /v1/sources/{id}.
func (h *Handlers) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	src, err := h.Service.Get(r.Context(), tid.String(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err, "could not read source")
		return
	}
	writeJSON(w, http.StatusOK, src)
}

// Update serves PATCH /v1/sources/{id} (includes pause/resume via `status`).
func (h *Handlers) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	var req updateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Credentials) > 0 {
		writeError(w, http.StatusBadRequest, "credential handling is not available yet (STORY-06.2); omit 'credentials'")
		return
	}
	patch := UpdatePatch{Name: req.Name, Config: req.Config, Status: req.Status}
	if req.ScheduleCron != nil {
		// An explicit JSON null clears the schedule; a string sets it.
		if string(*req.ScheduleCron) == "null" {
			patch.ClearSchedule = true
		} else {
			var cron string
			if err := json.Unmarshal(*req.ScheduleCron, &cron); err != nil {
				writeError(w, http.StatusBadRequest, "schedule_cron must be a string or null")
				return
			}
			patch.ScheduleCron = &cron
		}
	}
	src, err := h.Service.Update(r.Context(), UpdateParams{TenantID: tid.String(), ID: r.PathValue("id"), Patch: patch})
	if err != nil {
		writeServiceError(w, err, "could not update source")
		return
	}
	writeJSON(w, http.StatusOK, src)
}

// Delete serves DELETE /v1/sources/{id}: mark deleting + enqueue delete_source.
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	job, err := h.Service.Delete(r.Context(), tid.String(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err, "could not delete source")
		return
	}
	// 202 Accepted: deletion is asynchronous (the delete_source worker performs
	// the FR-SRC-12 cascade). An already-deleting source returns an empty job.
	if job.ID == "" {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "deleting"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "deleting", "job": job})
}

// Sync serves POST /v1/sources/{id}/sync (manual sync). Honors Idempotency-Key.
func (h *Handlers) Sync(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	var req syncRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	job, err := h.Service.Sync(r.Context(), SyncParams{
		TenantID:       tid.String(),
		SourceID:       r.PathValue("id"),
		Full:           req.Full,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeServiceError(w, err, "could not start sync")
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// Test serves POST /v1/sources/{id}/test (runs Connector.Test). Until the
// connector framework is wired (EPIC-06) this returns the not_found seam envelope.
func (h *Handlers) Test(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	if err := h.Service.Test(r.Context(), tid.String(), r.PathValue("id")); err != nil {
		writeServiceError(w, err, "could not test source")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// writeServiceError maps a service error to the SPEC-07 §1 envelope. A
// ValidationError is 400; the sentinels map to their documented statuses; an
// ErrConnectorUnavailable becomes the not_found seam (mirroring STORY-04.1);
// anything else is a generic 500 with the safe fallback message.
func writeServiceError(w http.ResponseWriter, err error, fallback string) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Msg)
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "source not found")
	case errors.Is(err, ErrConnectorUnavailable):
		writeError(w, http.StatusNotFound, "connector framework not available yet (EPIC-06)")
	case errors.Is(err, ErrDuplicateName):
		writeError(w, http.StatusConflict, "a source with that name already exists")
	case errors.Is(err, ErrActiveSyncExists):
		writeError(w, http.StatusConflict, "a sync is already queued or running for this source")
	default:
		writeError(w, http.StatusInternalServerError, fallback)
	}
}

// decodeJSON strictly decodes a request body, rejecting unknown fields so a
// misnamed field (e.g. a fat-fingered secret) is a 400 rather than silently
// ignored.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the SPEC-07 §1 error envelope so the router-mounted sources
// handlers speak the one public error contract (ADR-0027). The request-id is
// stamped by the obs middleware onto the response header, not the body here
// (consistent with the other cp handlers).
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
	case http.StatusConflict:
		return "conflict"
	default:
		return "internal"
	}
}
