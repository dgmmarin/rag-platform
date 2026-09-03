package jobs

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Handlers is the HTTP entry point for the jobs API (FR-ADM-02, SPEC-07 §2). The
// tenant is always taken from the resolved context (FR-ACC-03), never a request
// parameter. STORY-04.1's router mounts these behind RequireScopeAdmin ->
// RateLimit (all jobs routes are the `admin` scope, SPEC-07 §2).
type Handlers struct {
	Service *Service
}

// NewHandlers builds handlers over a jobs service.
func NewHandlers(svc *Service) *Handlers { return &Handlers{Service: svc} }

// List serves GET /v1/jobs?status&kind&source&limit&cursor.
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	q := r.URL.Query()
	p := ListParams{
		TenantID: tid.String(),
		Cursor:   q.Get("cursor"),
		Filter: ListFilter{
			Status:   q.Get("status"),
			Kind:     q.Get("kind"),
			SourceID: q.Get("source"),
		},
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid 'limit'")
			return
		}
		p.Limit = n
	}
	page, err := h.Service.List(r.Context(), p)
	if err != nil {
		writeServiceError(w, err, "could not list jobs")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// Get serves GET /v1/jobs/{id}.
func (h *Handlers) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	job, err := h.Service.Get(r.Context(), tid.String(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err, "could not read job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// Cancel serves POST /v1/jobs/{id}/cancel. A queued job is cancelled now (200
// with the cancelled job); a running job with the worker wired returns 202
// (cancellation requested, the worker finalises it); a running job without the
// worker (today) returns the not_found seam; a terminal job is 409.
func (h *Handlers) Cancel(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	job, err := h.Service.Cancel(r.Context(), tid.String(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err, "could not cancel job")
		return
	}
	// A queued/already-cancelled job is terminal now (200); a running job whose
	// cancel was only signalled is still running until the worker exits (202).
	if job.Status == StatusCancelled {
		writeJSON(w, http.StatusOK, job)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// writeServiceError maps a service error to the SPEC-07 §1 envelope. A
// ValidationError is 400; the sentinels map to their documented statuses; an
// ErrCancelUnavailable becomes the not_found seam (mirroring STORY-04.1/04.3);
// anything else is a generic 500 with the safe fallback message.
func writeServiceError(w http.ResponseWriter, err error, fallback string) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Msg)
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "job not found")
	case errors.Is(err, ErrCancelUnavailable):
		writeError(w, http.StatusNotFound, "cancelling a running job requires the job worker (EPIC-09)")
	case errors.Is(err, ErrNotCancellable):
		writeError(w, http.StatusConflict, "job is not in a cancellable state")
	default:
		writeError(w, http.StatusInternalServerError, fallback)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the SPEC-07 §1 error envelope so the router-mounted jobs
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
