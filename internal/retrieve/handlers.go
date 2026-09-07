package retrieve

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Handlers is the HTTP entry point for POST /v1/retrieve (FR-RET-08, SPEC-07 §2).
// The tenant is always taken from the resolved context (tenant.TenantIDFromCtx, set
// by the API-key `query` scope middleware) — never a request parameter (FR-ACC-03).
type Handlers struct {
	Service *Service
}

// NewHandlers builds handlers over a retrieve service.
func NewHandlers(svc *Service) *Handlers { return &Handlers{Service: svc} }

// retrieveRequest is the POST /v1/retrieve body. filters mirror the STORY-08.1
// Filters (FR-RET-02); date bounds are RFC 3339. An absent field is a no-op filter.
type retrieveRequest struct {
	Query   string           `json:"query"`
	TopK    int              `json:"top_k,omitempty"`
	Filters *retrieveFilters `json:"filters,omitempty"`
}

type retrieveFilters struct {
	SourceIDs []string        `json:"source_ids,omitempty"`
	URIPrefix string          `json:"uri_prefix,omitempty"`
	DateFrom  *time.Time      `json:"date_from,omitempty"`
	DateTo    *time.Time      `json:"date_to,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// chunkView is one ranked chunk in the response: the citation metadata SPEC-06/07
// promise (id, document_id, source_id, content, uri, title, heading_path, metadata,
// score). The opaque embedding vector is never returned. `score` is the fused RRF
// score normally; when the tenant enables reranking (settings.reranker.enabled,
// STORY-08.3), results are reordered by the reranker and `score` is the reranker
// relevance score instead (SPEC-06 §3).
type chunkView struct {
	ID          string          `json:"id"`
	DocumentID  string          `json:"document_id"`
	SourceID    string          `json:"source_id"`
	Content     string          `json:"content"`
	URI         string          `json:"uri"`
	Title       string          `json:"title"`
	HeadingPath []string        `json:"heading_path"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Score       float64         `json:"score"`
}

type retrieveResponse struct {
	Chunks []chunkView `json:"chunks"`
}

// Retrieve serves POST /v1/retrieve: embed the query with the tenant's configured
// provider and return the ranked hybrid-retrieval chunks (no generation, FR-RET-08).
func (h *Handlers) Retrieve(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}

	var req retrieveRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	res, err := h.Service.Search(r.Context(), tid, Request{
		Query:   req.Query,
		TopK:    req.TopK,
		Filters: req.Filters.toFilters(),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, retrieveResponse{Chunks: toChunkViews(res)})
}

// toFilters maps the request filters onto the retrieve.Filters the hybrid query
// accepts. A nil filters object is the all-no-op zero value.
func (f *retrieveFilters) toFilters() Filters {
	if f == nil {
		return Filters{}
	}
	return Filters{
		SourceIDs:    f.SourceIDs,
		URIPrefix:    f.URIPrefix,
		DateFrom:     f.DateFrom,
		DateTo:       f.DateTo,
		MetadataTags: f.Metadata,
	}
}

func toChunkViews(res []Result) []chunkView {
	out := make([]chunkView, len(res))
	for i, r := range res {
		hp := r.HeadingPath
		if hp == nil {
			hp = []string{}
		}
		out[i] = chunkView{
			ID:          r.ChunkID,
			DocumentID:  r.DocumentID,
			SourceID:    r.SourceID,
			Content:     r.Content,
			URI:         r.URI,
			Title:       r.Title,
			HeadingPath: hp,
			Metadata:    r.Metadata,
			Score:       r.Score,
		}
	}
	return out
}

// writeServiceError maps a service error to the SPEC-07 §1 envelope. An empty
// query is 400; an embedding-provider failure is a generic 500 (never leaking
// provider internals); an unavailable tenant is 503; anything else is a 500.
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrEmptyQuery):
		writeError(w, http.StatusBadRequest, "query is required")
	case errors.Is(err, ErrEmbedding):
		writeError(w, http.StatusInternalServerError, "could not embed the query")
	case errors.Is(err, ErrTenantUnavailable):
		writeError(w, http.StatusServiceUnavailable, "tenant is not available")
	default:
		writeError(w, http.StatusInternalServerError, "could not retrieve")
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the SPEC-07 §1 error envelope so the retrieve handler speaks
// the one public error contract. The request-id is stamped on the response header
// by the obs middleware, consistent with the other handlers.
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
