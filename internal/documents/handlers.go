package documents

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Handlers is the HTTP entry point for the documents API (FR-SRC-02, FR-ADM-03,
// SPEC-07 §2). The tenant is always taken from the resolved context
// (tenant.TenantIDFromCtx, set by the API-key scope middleware) — never a request
// parameter (FR-ACC-03). The router mounts ingest/delete behind RequireScopeIngest,
// list/get behind RequireScopeQuery, and chunks behind RequireScopeAdmin.
type Handlers struct {
	Service *Service
}

// NewHandlers builds handlers over a documents service.
func NewHandlers(svc *Service) *Handlers { return &Handlers{Service: svc} }

// List serves GET /v1/documents?source&status&q&limit&cursor.
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	q := r.URL.Query()
	f := ListFilter{SourceID: q.Get("source"), Status: q.Get("status"), Q: q.Get("q")}
	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'limit'")
		return
	}
	page, err := h.Service.List(r.Context(), tid, f, limit, q.Get("cursor"))
	if err != nil {
		writeServiceError(w, err, "could not list documents")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// Get serves GET /v1/documents/{id} (optional ?content=true).
func (h *Handlers) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	withContent := r.URL.Query().Get("content") == "true"
	doc, err := h.Service.Get(r.Context(), tid, r.PathValue("id"), withContent)
	if err != nil {
		writeServiceError(w, err, "could not read document")
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// Chunks serves GET /v1/documents/{id}/chunks (admin debugging, FR-ADM-03).
func (h *Handlers) Chunks(w http.ResponseWriter, r *http.Request) {
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
	page, err := h.Service.Chunks(r.Context(), tid, r.PathValue("id"), limit, q.Get("cursor"))
	if err != nil {
		writeServiceError(w, err, "could not read chunks")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// Delete serves DELETE /v1/documents/{id} (soft delete, FR-SRC-02).
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	if err := h.Service.Delete(r.Context(), tid, r.PathValue("id")); err != nil {
		writeServiceError(w, err, "could not delete document")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// Ingest serves POST /v1/documents: a multipart upload (form field "file") that
// is written to object storage and enqueues an ingest_document job (SPEC-07 §2).
// The FR-SRC-02 type allowlist and size ceiling are enforced here before the
// bytes are read; the tenant's upload source may be named in the "source" field.
func (h *Handlers) Ingest(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	maxBytes := h.Service.maxBytes()
	// Hard-cap the whole request body so an oversize upload cannot exhaust memory;
	// +1 KB tolerance for the multipart envelope around the file part itself.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1<<10)
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusBadRequest, "upload exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "expected a multipart/form-data upload with a 'file' field")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "a 'file' part is required")
		return
	}
	defer func() { _ = file.Close() }()

	if header.Size > maxBytes {
		writeError(w, http.StatusBadRequest, "upload exceeds the maximum size")
		return
	}
	contentType, allowed := uploadContentType(header.Filename)
	if !allowed {
		writeError(w, http.StatusBadRequest, "unsupported file type; allowed: pdf, docx, md, html, txt, csv")
		return
	}

	var sourceID *string
	if v := r.FormValue("source"); v != "" {
		if !validUUID(v) {
			writeError(w, http.StatusBadRequest, "source must be a UUID")
			return
		}
		sourceID = &v
	}

	job, err := h.Service.Ingest(r.Context(), IngestParams{
		TenantID:       tid.String(),
		SourceID:       sourceID,
		Filename:       header.Filename,
		ContentType:    contentType,
		Size:           header.Size,
		Reader:         file,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeServiceError(w, err, "could not ingest document")
		return
	}
	// 202 Accepted: ingestion is asynchronous. The document row is built by the
	// worker; the queued job is the client's handle to track it.
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
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
// ValidationError is 400; ErrNotFound is 404; ErrStorageUnavailable is the
// not_found seam (mirroring STORY-04.1/04.3 — object storage lands in EPIC-06);
// ErrTenantUnavailable is 503; anything else is a generic 500.
func writeServiceError(w http.ResponseWriter, err error, fallback string) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Msg)
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "document not found")
	case errors.Is(err, ErrStorageUnavailable):
		writeError(w, http.StatusNotFound, "file upload is not available yet (object storage lands in EPIC-06)")
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

// writeError writes the SPEC-07 §1 error envelope so the documents handlers speak
// the one public error contract (ADR-0027). The request-id is stamped on the
// response header by the obs middleware, consistent with the other cp handlers.
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
