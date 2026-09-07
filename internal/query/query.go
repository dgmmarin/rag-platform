// Package query is the answering query endpoint (SPEC-06 §6, FR-RET-06): it wires
// the existing retrieval (STORY-08.1/08.3, internal/retrieve) and answering
// (STORY-08.5, internal/answer) pipeline behind POST /v1/query in BOTH a
// non-streaming JSON mode and a streaming SSE mode. It is the composition root for
// those two packages — it does no ranking, prompt assembly, or citation mapping of
// its own (those stay in retrieve/answer); it orchestrates them, renders the SPEC-06
// §6 response shape, folds the Queries usage counter, and — for SSE — drives the
// llm.Provider.Stream pull iterator into the ordered retrieval→delta→done events.
//
// Tenant isolation: the tenant is always the resolved principal (FR-ACC-03), passed
// as a tenant.ID; there is no tenant_id request parameter. Tenant content is reached
// only through the retrieve service's resolver + *tenant.DB (ADR-0003, C-1, C-3);
// the tenant display name for the refusal message is control-plane registry data
// (C-3), read through the Names seam.
//
// Streaming citations (SPEC-06 §6 "retrieval (citations first)", ADR-0056): the SSE
// path emits the numbered CANDIDATE citations — one per context chunk — in the
// retrieval event BEFORE any text, because the full answer (and thus which [n]
// markers it uses) is not yet known. The client maps the [n] markers to these as the
// text streams; the numbering is stable (= context order). JSON mode keeps the
// post-hoc unreferenced-drop (internal/answer.mapCitations): same numbering, JSON
// simply omits the citations no marker referenced.
//
// Graceful degradation (NFR-REL-04): if generation is unavailable
// (llm.ErrCircuitOpen), the query does NOT hard-fail — it returns the retrieved
// citations with a fixed "generation unavailable" message (JSON), or streams the
// retrieval event + that message + done (SSE), so retrieval-only keeps working.
package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/cp/usage"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// fallbackTenantName is used in the refusal message when the tenant's display name
// cannot be read (a non-fatal degradation — the name only affects wording, never
// correctness). ponytail: a generic stand-in; the real name is read from the
// control-plane tenants row on the normal path.
const fallbackTenantName = "the knowledge base"

// generationUnavailableMessage is returned on the graceful-degradation path
// (NFR-REL-04) when the LLM provider's circuit is open. The retrieved sources are
// still cited so the client can fall back to reading them directly.
const generationUnavailableMessage = "Answer generation is temporarily unavailable. The most relevant sources are cited below."

// Retriever is the retrieval half of the pipeline (STORY-08.1/08.3). *retrieve.Service
// satisfies it structurally, so tests inject a fake without a database.
type Retriever interface {
	Search(ctx context.Context, tid tenant.ID, req retrieve.Request) ([]retrieve.Result, error)
}

// SettingsSource returns a tenant's resolved settings document (SPEC-02 §5).
// *tenants.SettingsService satisfies it structurally.
type SettingsSource interface {
	Get(ctx context.Context, tenantID string) (map[string]any, error)
}

// TenantNameSource resolves a tenant's display name for the refusal message
// (SPEC-06 §4). *tenants.NameService satisfies it structurally. The name is
// control-plane registry data (C-3).
type TenantNameSource interface {
	Name(ctx context.Context, tenantID string) (string, error)
}

// UsageRecorder folds usage into usage_daily (ADR-0024). *usage.Counter satisfies it.
// The query endpoint owns the Queries counter (08.5 deliberately left it here to
// avoid a double count); LLM token folding is done by internal/answer.
type UsageRecorder interface {
	Add(tenantID string, d usage.Delta)
}

// Request is one /v1/query call (SPEC-06 §6). The stream flag is handled by the HTTP
// handler (it selects Query vs QueryStream), so it is not carried here.
type Request struct {
	Question string
	Filters  retrieve.Filters
	History  []answer.Turn
	TopK     int
}

// Service orchestrates one query: retrieve → answer, in JSON or SSE form. It is
// stateless and safe for concurrent use.
type Service struct {
	// Retrieve runs hybrid retrieval + rerank for the tenant. Required.
	Retrieve Retriever
	// Answer is the answering stage (grounding, prompt, citations). Required.
	Answer *answer.Service
	// Settings resolves the tenant's settings document (min_score, answering, llm).
	// Required.
	Settings SettingsSource
	// Names resolves the tenant display name for the refusal message. Optional (nil
	// or an error falls back to a generic name — non-fatal).
	Names TenantNameSource
	// Usage folds the Queries counter (nil = no counter; the response is unaffected).
	Usage UsageRecorder
}

// Query runs the pipeline and returns the SPEC-06 §6 JSON result. On generation
// unavailability (llm.ErrCircuitOpen) it degrades to a retrieval-only result rather
// than erroring (NFR-REL-04).
func (s *Service) Query(ctx context.Context, tid tenant.ID, req Request) (answer.Result, error) {
	ar, err := s.build(ctx, tid, req)
	if err != nil {
		return answer.Result{}, err
	}
	res, aerr := s.Answer.Answer(ctx, ar)
	if aerr != nil {
		if !errors.Is(aerr, llm.ErrCircuitOpen) {
			return answer.Result{}, aerr
		}
		res = s.degradedResult(ctx, ar)
	}
	s.countQuery(tid)
	return res, nil
}

// EventSink receives SSE events in order. The HTTP handler provides a flushing
// implementation; tests provide a recorder. data is JSON-encoded by the sink.
type EventSink interface {
	Send(event string, data any) error
}

// SSE event payloads (SPEC-06 §6). Emitted in order: retrieval → delta(s) → done.
type retrievalEvent struct {
	Citations []answer.Citation `json:"citations"`
}
type deltaEvent struct {
	Text string `json:"text"`
}
type doneEvent struct {
	ID                    string       `json:"id"`
	Grounded              bool         `json:"grounded"`
	Model                 string       `json:"model"`
	Usage                 answer.Usage `json:"usage"`
	GenerationUnavailable bool         `json:"generation_unavailable,omitempty"`
}
type errorEvent struct {
	Message string `json:"message"`
}

// QueryStream runs the pipeline and emits the SSE event sequence (SPEC-06 §6):
// `retrieval` (candidate citations FIRST), then `delta` text events, then `done`
// (usage). A returned error means the failure happened BEFORE any event was emitted
// (retrieval/settings/provider build) — the caller renders a normal JSON error
// envelope, no SSE headers written. Once the retrieval event is sent, a mid-stream
// failure is surfaced as an `error` event (the HTTP status is already committed) and
// QueryStream returns nil.
func (s *Service) QueryStream(ctx context.Context, tid tenant.ID, req Request, sink EventSink) error {
	ar, err := s.build(ctx, tid, req)
	if err != nil {
		return err
	}
	p, err := s.Answer.Prepare(ctx, ar)
	if err != nil {
		return err
	}

	// Citations first (SPEC-06 §6): the candidate citations for every context chunk
	// (empty on refusal). The client maps [n] markers to these as text streams.
	if err := sink.Send("retrieval", retrievalEvent{Citations: nonNilCitations(p.Citations)}); err != nil {
		return err
	}

	// Grounding refusal (SPEC-06 §4): a single delta with the fixed message + done;
	// no stream call.
	if !p.Grounded {
		u := answer.Usage{RetrievalMs: ar.RetrievalMs}
		_ = sink.Send("delta", deltaEvent{Text: p.RefusalText})
		_ = sink.Send("done", doneEvent{ID: p.ID, Grounded: false, Model: p.Model, Usage: u})
		s.Answer.RecordStreamed(ctx, ar, p, u)
		s.countQuery(tid)
		return nil
	}

	// Grounded: open the provider stream.
	start := time.Now()
	stream, serr := p.Provider.Stream(ctx, p.LLMRequest)
	if serr != nil {
		if errors.Is(serr, llm.ErrCircuitOpen) {
			u := answer.Usage{RetrievalMs: ar.RetrievalMs}
			_ = sink.Send("delta", deltaEvent{Text: generationUnavailableMessage})
			_ = sink.Send("done", doneEvent{ID: p.ID, Grounded: true, Model: p.Model, Usage: u, GenerationUnavailable: true})
			s.Answer.RecordStreamed(ctx, ar, p, u)
			s.countQuery(tid)
			return nil
		}
		// Establishment failed for another reason; the retrieval event (and thus the
		// 200 + SSE headers) is already out, so surface it as an error event, not a
		// changed status. Errors carry no prompt content (C-4).
		slog.WarnContext(ctx, "query: stream open failed", "tenant", tid.String(), "err", serr)
		_ = sink.Send("error", errorEvent{Message: "generation failed"})
		return nil
	}
	defer func() { _ = stream.Close() }()

	u := answer.Usage{RetrievalMs: ar.RetrievalMs}
	for {
		ev, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			slog.WarnContext(ctx, "query: stream interrupted", "tenant", tid.String(), "err", rerr)
			_ = sink.Send("error", errorEvent{Message: "generation interrupted"})
			break
		}
		if ev.Done {
			u.InTokens = ev.Usage.InputTokens
			u.OutTokens = ev.Usage.OutputTokens
			continue
		}
		if ev.TextDelta != "" {
			if err := sink.Send("delta", deltaEvent{Text: ev.TextDelta}); err != nil {
				return nil // client hung up; stop streaming
			}
		}
	}
	u.GenerationMs = time.Since(start).Milliseconds()
	_ = sink.Send("done", doneEvent{ID: p.ID, Grounded: true, Model: p.Model, Usage: u})
	s.Answer.RecordStreamed(ctx, ar, p, u)
	s.countQuery(tid)
	return nil
}

// build runs retrieval and assembles the answer.Request (settings + tenant name +
// chunk mapping) shared by both modes. Retrieval runs first so a bad request or an
// unavailable tenant fails before any SSE headers are written.
func (s *Service) build(ctx context.Context, tid tenant.ID, req Request) (answer.Request, error) {
	start := time.Now()
	results, err := s.Retrieve.Search(ctx, tid, retrieve.Request{
		Query:   req.Question,
		Filters: req.Filters,
		TopK:    req.TopK,
	})
	if err != nil {
		return answer.Request{}, err
	}
	retrievalMs := time.Since(start).Milliseconds()

	raw, err := s.Settings.Get(ctx, tid.String())
	if err != nil {
		return answer.Request{}, fmt.Errorf("query: load settings: %w", err)
	}
	st := parseAnswerSettings(raw)
	st.TenantName = s.tenantName(ctx, tid)

	return answer.Request{
		TenantID:    tid.String(),
		Question:    req.Question,
		Chunks:      toAnswerChunks(results),
		History:     req.History,
		Settings:    st,
		RetrievalMs: retrievalMs,
	}, nil
}

// degradedResult builds the retrieval-only JSON result when generation is
// unavailable (NFR-REL-04): the candidate citations from the assembled prompt plus a
// fixed message, with retrieval usage only. It re-runs Prepare (grounding + assembly,
// no network) to obtain the exact candidate citations the SSE path would emit — so
// both modes degrade with the same sources.
func (s *Service) degradedResult(ctx context.Context, ar answer.Request) answer.Result {
	p, err := s.Answer.Prepare(ctx, ar)
	res := answer.Result{
		Answer:    generationUnavailableMessage,
		Grounded:  true,
		Citations: []answer.Citation{},
		Usage:     answer.Usage{RetrievalMs: ar.RetrievalMs},
		Model:     ar.Settings.LLMModel,
	}
	if err == nil {
		res.ID = p.ID
		res.Grounded = p.Grounded
		res.Citations = nonNilCitations(p.Citations)
	}
	return res
}

// tenantName reads the tenant display name for the refusal message (SPEC-06 §4). A
// lookup failure is non-fatal (it only affects wording): log and fall back.
func (s *Service) tenantName(ctx context.Context, tid tenant.ID) string {
	if s.Names == nil {
		return fallbackTenantName
	}
	name, err := s.Names.Name(ctx, tid.String())
	if err != nil || name == "" {
		slog.WarnContext(ctx, "query: tenant name lookup failed; using fallback",
			"tenant", tid.String(), "err", err)
		return fallbackTenantName
	}
	return name
}

// countQuery folds one Queries count for the tenant (FR-RET-04 accounting; owned by
// 08.6 to avoid a double count with 08.5's LLM-token fold).
func (s *Service) countQuery(tid tenant.ID) {
	if s.Usage != nil {
		s.Usage.Add(tid.String(), usage.Delta{Queries: 1})
	}
}

// toAnswerChunks maps the ranked retrieval results onto the answering stage's local
// Chunk type (a field copy — internal/answer stays independent of the retrieval
// pipeline's shape, ADR-0055). Score is the reranker score when reranking is enabled,
// else the fused RRF score (SPEC-06 §3); the grounding floor applies to it.
func toAnswerChunks(res []retrieve.Result) []answer.Chunk {
	out := make([]answer.Chunk, len(res))
	for i, r := range res {
		out[i] = answer.Chunk{
			ChunkID:     r.ChunkID,
			DocumentID:  r.DocumentID,
			Title:       r.Title,
			URI:         r.URI,
			HeadingPath: r.HeadingPath,
			Content:     r.Content,
			Score:       r.Score,
		}
	}
	return out
}

// nonNilCitations guarantees a non-nil slice so JSON renders `[]`, never `null`.
func nonNilCitations(c []answer.Citation) []answer.Citation {
	if c == nil {
		return []answer.Citation{}
	}
	return c
}

// parseAnswerSettings extracts the answering-relevant fields from a settings
// document (SPEC-02 §5): the grounding floor (retrieval.min_score), the context
// budget/history bounds (answering.*), and the LLM provider/model + fail-closed
// allowlists (llm.*, providers_allowed). Missing/typed-wrong fields fall back to
// zero, which internal/answer treats as its documented default. TenantName is filled
// by the caller.
func parseAnswerSettings(doc map[string]any) answer.Settings {
	var s answer.Settings
	if ret, ok := doc["retrieval"].(map[string]any); ok {
		s.MinScore = toFloat(ret["min_score"])
	}
	if ans, ok := doc["answering"].(map[string]any); ok {
		s.TokenBudget = toInt(ans["token_budget"])
		s.HistoryN = toInt(ans["history_n"])
	}
	if l, ok := doc["llm"].(map[string]any); ok {
		s.LLMProvider, _ = l["provider"].(string)
		s.LLMModel, _ = l["model"].(string)
		s.MaxTokens = toInt(l["max_tokens"])
		s.LLMModelsAllowed = toStringSlice(l["models_allowed"])
	}
	s.ProvidersAllowed = toStringSlice(doc["providers_allowed"])
	return s
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, a := range arr {
		if str, ok := a.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}
