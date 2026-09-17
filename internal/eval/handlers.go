package eval

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Handlers is the HTTP entry point for the eval REPORT surface (STORY-12.4,
// FR-ADM-04): read-only access to stored runs and their per-case results. The
// tenant is always taken from the resolved context (tenant.TenantIDFromCtx, set by
// the session tenant-access middleware) — never a request parameter (FR-ACC-03).
// The mutating eval surface (cases CRUD, run) stays on the CLI (STORY-12.1/12.2).
type Handlers struct {
	Service *Service
}

// NewHandlers builds handlers over an eval service.
func NewHandlers(svc *Service) *Handlers { return &Handlers{Service: svc} }

// runListResponse is the runs-list envelope. It carries no cursor yet — the store
// returns a capped newest-first page (defaultRunListLimit).
type runListResponse struct {
	Items []RunView `json:"items"`
}

// RunList serves GET .../eval/runs?limit — the tenant's stored runs, newest first.
func (h *Handlers) RunList(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'limit'")
		return
	}
	runs, err := h.Service.ListRuns(r.Context(), tid, limit)
	if err != nil {
		writeServiceError(w, err, "could not list eval runs")
		return
	}
	writeJSON(w, http.StatusOK, runListResponse{Items: runs})
}

// Report serves GET .../eval/runs/{id} — one run plus its per-case results.
func (h *Handlers) Report(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	report, err := h.Service.Report(r.Context(), tid, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err, "could not read eval run")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// parseLimit parses the optional ?limit; a negative or non-numeric value is an
// error, an empty value is 0 (the store applies the default).
func parseLimit(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, errors.New("invalid limit")
	}
	return n, nil
}

// writeServiceError maps a service error to the SPEC-07 §1 envelope: ErrNotFound is
// 404, ErrTenantUnavailable is 503, anything else is a generic 500.
func writeServiceError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "eval run not found")
	case errors.Is(err, ErrTenantUnavailable):
		writeError(w, http.StatusServiceUnavailable, "tenant is not available")
	default:
		writeError(w, http.StatusInternalServerError, fallback)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the SPEC-07 §1 error envelope so the eval handlers speak the
// one public error contract (ADR-0027).
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
	case http.StatusServiceUnavailable:
		return "tenant_unavailable"
	default:
		return "internal"
	}
}
