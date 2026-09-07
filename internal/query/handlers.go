package query

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// Handlers is the HTTP entry point for POST /v1/query (FR-RET-06, SPEC-06 §6,
// SPEC-07 §2). The tenant is always taken from the resolved context (set by the
// API-key `query` scope middleware) — never a request parameter (FR-ACC-03). The
// body's `stream` flag selects JSON (Service.Query) or SSE (Service.QueryStream).
type Handlers struct {
	Service *Service
}

// NewHandlers builds handlers over a query service.
func NewHandlers(svc *Service) *Handlers { return &Handlers{Service: svc} }

// queryRequest is the POST /v1/query body (SPEC-06 §6). filters mirror the retrieve
// endpoint's (FR-RET-02); an absent field is a no-op filter. history is included
// verbatim (the follow-up rewrite is STORY-08.7).
type queryRequest struct {
	Question string        `json:"question"`
	Filters  *queryFilters `json:"filters,omitempty"`
	History  []turnJSON    `json:"history,omitempty"`
	Stream   bool          `json:"stream,omitempty"`
	TopK     int           `json:"top_k,omitempty"`
}

type queryFilters struct {
	SourceIDs []string        `json:"source_ids,omitempty"`
	URIPrefix string          `json:"uri_prefix,omitempty"`
	DateFrom  *time.Time      `json:"date_from,omitempty"`
	DateTo    *time.Time      `json:"date_to,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

type turnJSON struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (b queryRequest) toRequest() Request {
	return Request{
		Question: b.Question,
		Filters:  b.Filters.toFilters(),
		History:  toTurns(b.History),
		TopK:     b.TopK,
	}
}

func (f *queryFilters) toFilters() retrieve.Filters {
	if f == nil {
		return retrieve.Filters{}
	}
	return retrieve.Filters{
		SourceIDs:    f.SourceIDs,
		URIPrefix:    f.URIPrefix,
		DateFrom:     f.DateFrom,
		DateTo:       f.DateTo,
		MetadataTags: f.Metadata,
	}
}

func toTurns(h []turnJSON) []answer.Turn {
	if len(h) == 0 {
		return nil
	}
	out := make([]answer.Turn, len(h))
	for i, t := range h {
		out[i] = answer.Turn{Role: t.Role, Content: t.Content}
	}
	return out
}

// Query serves POST /v1/query. It resolves the tenant from context, decodes the
// body, and dispatches to the JSON or SSE path per the `stream` flag.
func (h *Handlers) Query(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}

	var body queryRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req := body.toRequest()

	if body.Stream {
		sink := &httpSink{w: w}
		if f, ok := w.(http.Flusher); ok {
			sink.flusher = f
		}
		// A pre-stream failure (bad request, unavailable tenant, provider build)
		// returns an error before any event is sent, so a normal JSON envelope can
		// still be written. Once events have started the status is committed and any
		// failure is surfaced as an SSE `error` event (QueryStream returns nil).
		if err := h.Service.QueryStream(r.Context(), tid, req, sink); err != nil && !sink.started {
			writeServiceError(w, err)
		}
		return
	}

	res, err := h.Service.Query(r.Context(), tid, req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// httpSink writes SSE frames to the response, opening the stream (headers + 200) on
// the first event so a pre-stream error can still be a JSON envelope. It flushes
// after every frame so the client receives tokens as they are produced.
type httpSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
}

func (s *httpSink) Send(event string, data any) error {
	if !s.started {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("Connection", "keep-alive")
		// Disable proxy buffering so events are not held back (SPEC-06 §6 streaming).
		s.w.Header().Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

// writeServiceError maps a service error to the SPEC-07 §1 envelope. Errors bubble
// up from the retrieval half (empty query, unavailable tenant, embedding failure);
// any other failure (settings load, provider build, non-circuit generation error)
// is a generic 500 that never leaks provider internals (C-4). A generation
// circuit-open is NOT an error here — it degrades to a 200 retrieval-only result
// inside the service (NFR-REL-04).
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, retrieve.ErrEmptyQuery):
		writeError(w, http.StatusBadRequest, "question is required")
	case errors.Is(err, retrieve.ErrTenantUnavailable):
		writeError(w, http.StatusServiceUnavailable, "tenant is not available")
	case errors.Is(err, retrieve.ErrEmbedding):
		writeError(w, http.StatusInternalServerError, "could not embed the query")
	default:
		writeError(w, http.StatusInternalServerError, "could not answer the query")
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the SPEC-07 §1 error envelope so the query handler speaks the
// one public error contract (the request-id is stamped on the response header by the
// obs middleware, matching the retrieve handler).
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
