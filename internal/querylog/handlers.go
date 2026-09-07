package querylog

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Handlers is the HTTP entry point for the feedback endpoint (POST /v1/feedback,
// query scope) and the admin query-log listing (GET /v1/queries, admin scope) of
// SPEC-07 §2/§2g. The tenant is always taken from the resolved context
// (tenant.TenantIDFromCtx, set by the API-key scope middleware) — never a request
// parameter (FR-ACC-03).
type Handlers struct {
	Service *Service
}

// NewHandlers builds handlers over a querylog service.
func NewHandlers(svc *Service) *Handlers { return &Handlers{Service: svc} }

// feedbackRequest is the POST /v1/feedback body (SPEC-07 §2): {query_id, rating,
// comment}. rating is the thumbs value (1 up, -1 down).
type feedbackRequest struct {
	QueryID string `json:"query_id"`
	Rating  int    `json:"rating"`
	Comment string `json:"comment,omitempty"`
}

// Feedback serves POST /v1/feedback (FR-RET-10). It resolves the tenant from
// context, decodes the body, and upserts the rating. Validating the query id's
// ownership is structural: the write targets the tenant's own database.
func (h *Handlers) Feedback(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	var body feedbackRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := h.Service.Feedback(r.Context(), tid, body.QueryID, body.Rating, body.Comment); err != nil {
		writeServiceError(w, err, "could not record feedback")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "recorded"})
}

// List serves GET /v1/queries?limit&cursor (FR-RET-09 admin visibility): a keyset
// page of the tenant's query log with joined feedback.
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	q := r.URL.Query()
	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'limit'")
		return
	}
	page, err := h.Service.List(r.Context(), tid, limit, q.Get("cursor"))
	if err != nil {
		writeServiceError(w, err, "could not list queries")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// parseLimit parses the optional ?limit; a negative or non-numeric value is an
// error, an empty value is 0 (the service applies the default).
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

// writeServiceError maps a service error to the SPEC-07 §1 envelope. A
// ValidationError is 400; ErrQueryNotFound is 404; ErrTenantUnavailable (and a
// suspended tenant's read-only refusal) is 503; anything else is a generic 500.
func writeServiceError(w http.ResponseWriter, err error, fallback string) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Msg)
	case errors.Is(err, ErrQueryNotFound):
		writeError(w, http.StatusNotFound, "query not found")
	case errors.Is(err, ErrTenantUnavailable), errors.Is(err, tenant.ErrReadOnly):
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

// writeError writes the SPEC-07 §1 error envelope so these handlers speak the one
// public error contract; the request-id is stamped on the response header by the
// obs middleware, consistent with the other tenant-scoped handlers.
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
	case http.StatusServiceUnavailable:
		return "tenant_unavailable"
	default:
		return "internal"
	}
}
