package retrieve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/rerank"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// This file is the STORY-08.2 retrieve endpoint's service half (FR-RET-08,
// SPEC-06 §1/§2, SPEC-07 §2). It turns an incoming query STRING into a query
// EMBEDDING using the tenant's configured embedding provider (the same
// provider/model used at ingest — the query and documents MUST share an embedding
// space), then runs the STORY-08.1 hybrid query (the package-level Retrieve) and
// returns the ranked chunks. The tenant is resolved to its DB ONLY through the
// resolver (ADR-0003, C-1, C-3); it is never a request parameter (FR-ACC-03).
//
// Reranking (STORY-08.3), the min_score grounding floor and the LLM answer path
// (STORY-08.5) are NOT applied here: /v1/retrieve returns the raw fused retrieval
// results, which those layers consume downstream.

// defaultMaxTopK is the ceiling on a request's top_k. A client cannot ask for an
// unbounded fused result set (which would balloon the fusion + response). ponytail:
// a fixed ceiling, not a per-tenant setting; lift it into settings.retrieval if a
// tenant ever needs a larger page.
const defaultMaxTopK = 100

// defaultRerankTopN is the fused-candidate count sent to the reranker when a
// tenant enables reranking without setting reranker.top_n (mirrors the
// settings_defaults.json default). The service over-fetches this many fused
// results (SPEC-06 §1: rerank top_n → take final_k), reorders them, then truncates
// to final_k.
const defaultRerankTopN = 20

// Errors surfaced by Search, all checkable with errors.Is so the HTTP layer maps
// them to the SPEC-07 §1 envelope without string matching.
var (
	// ErrEmptyQuery means the request carried no (non-whitespace) query text.
	ErrEmptyQuery = errors.New("retrieve: query text is required")
	// ErrEmbedding wraps any embedding-provider or factory failure. The wrapped
	// detail is for logs only; the HTTP layer returns a generic message so provider
	// internals (endpoints, tokens, upstream error bodies) never reach the client.
	ErrEmbedding = errors.New("retrieve: could not embed query")
	// ErrTenantUnavailable means the tenant exists but cannot be served right now
	// (unknown, not-ready, or schema-behind). Mapped to 503.
	ErrTenantUnavailable = errors.New("retrieve: tenant unavailable")
)

// Settings is the subset of a tenant's settings document (SPEC-02 §5) the retrieve
// endpoint needs: the embedding provider/model/dim (to embed the query in the same
// space as the corpus), the provider allowlist (fail-closed gate, SPEC-09 §2), and
// the retrieval candidate/final-k defaults (ADR-0007).
type Settings struct {
	EmbeddingProvider string
	EmbeddingModel    string
	EmbeddingDim      int
	ProvidersAllowed  []string
	KVector           int
	KText             int
	FinalK            int

	// Reranker (SPEC-06 §3, STORY-08.3). Enabled is the per-tenant toggle (default
	// off); Provider selects cohere|llm; Model is the Cohere rerank model; TopN is
	// how many fused results to rerank. RerankLLMModel is the optional
	// reranker.llm_model override for the LLM reranker (absent → reuse LLMModel).
	RerankEnabled  bool
	RerankProvider string
	RerankModel    string
	RerankTopN     int
	RerankLLMModel string

	// LLM (settings.llm) — the LLM reranker reuses the tenant's configured LLM
	// provider/model and model allowlist, built through the llm.Factory.
	LLMProvider      string
	LLMModel         string
	LLMModelsAllowed []string
}

// SettingsSource returns a tenant's resolved settings document (SPEC-02 §5).
// *tenants.SettingsService satisfies it structurally (Get(ctx, tenantID)), so this
// package keeps no control-plane import.
type SettingsSource interface {
	Get(ctx context.Context, tenantID string) (map[string]any, error)
}

// EmbedderFactory builds an Embedder for a tenant's settings — mirroring the
// ingest path (ingestdoc.EmbedderFactory) so the query is embedded through exactly
// the same seam the corpus was. Tests inject a deterministic stub; production wires
// KeyedEmbedderFactory.
type EmbedderFactory interface {
	Embedder(ctx context.Context, s Settings) (embed.Embedder, error)
}

// RerankerFactory builds a Reranker for a tenant's settings (SPEC-06 §3,
// STORY-08.3). It returns a nil Reranker (no error) when reranking is disabled, so
// the service simply skips reranking. A build error (e.g. a missing key,
// fail-closed) is treated by the service as a reranker failure — logged, then
// fallback to fused order (FR-RET-03 AC). Tests inject a fake; production wires
// KeyedRerankerFactory.
type RerankerFactory interface {
	Reranker(ctx context.Context, s Settings) (rerank.Reranker, error)
}

// Request is one /v1/retrieve call: the query text, optional filters (FR-RET-02)
// and an optional top_k. An unset top_k falls back to the tenant's
// settings.retrieval.final_k; any value is clamped to defaultMaxTopK.
type Request struct {
	Query   string
	Filters Filters
	TopK    int
}

// Service orchestrates one retrieval: resolve tenant → load settings → embed the
// query → run the hybrid query. It is stateless and safe for concurrent use.
type Service struct {
	Resolver tenant.Resolver
	Settings SettingsSource
	Embedder EmbedderFactory
	// Retriever runs the hybrid query against the tenant DB. It defaults to the
	// package-level Retrieve; tests inject a fake so Search is exercised without a
	// database (the ranking SQL itself is covered by the e2e suite).
	Retriever func(ctx context.Context, db *tenant.DB, p Params) ([]Result, error)
	// Reranker builds the per-tenant reranker (STORY-08.3, SPEC-06 §3). Nil means
	// reranking is never applied (the /v1/retrieve default before this story). When
	// set, it is consulted per request and honoured only if settings.reranker.enabled.
	Reranker RerankerFactory
	// MaxTopK overrides the top_k ceiling; 0 uses defaultMaxTopK.
	MaxTopK int
	// Metrics records query_retrieval_duration_seconds (SPEC-10 §2). Optional: a
	// nil Metrics disables the observation (the methods are no-ops).
	Metrics *obs.Metrics
}

// NewService builds a retrieve Service.
func NewService(resolver tenant.Resolver, settings SettingsSource, embedder EmbedderFactory) *Service {
	return &Service{Resolver: resolver, Settings: settings, Embedder: embedder}
}

// Search embeds the query and returns the ranked chunks for the tenant. The order
// is: validate → resolve tenant DB (fail fast on an unavailable tenant, before any
// provider call) → load settings → build embedder → embed → hybrid query.
func (s *Service) Search(ctx context.Context, tid tenant.ID, req Request) ([]Result, error) {
	if strings.TrimSpace(req.Query) == "" {
		return nil, ErrEmptyQuery
	}
	start := time.Now()

	db, err := s.open(ctx, tid)
	if err != nil {
		return nil, err
	}

	raw, err := s.Settings.Get(ctx, tid.String())
	if err != nil {
		return nil, fmt.Errorf("retrieve: load settings: %w", err)
	}
	st := parseSettings(raw)

	emb, err := s.Embedder.Embedder(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("%w: build embedder: %v", ErrEmbedding, err)
	}
	out, err := emb.Embed(ctx, []string{req.Query})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEmbedding, err)
	}
	if len(out.Vectors) == 0 || len(out.Vectors[0]) == 0 {
		return nil, fmt.Errorf("%w: provider returned no vector", ErrEmbedding)
	}

	// Reranking (SPEC-06 §3, STORY-08.3): when enabled, the hybrid query must
	// over-fetch top_n fused candidates (not just final_k) so the reranker reorders
	// the wider set; final_k is applied AFTER reranking. min_score/grounding refusal
	// is STORY-08.5 — not applied here.
	finalK := s.finalK(req.TopK, st.FinalK)
	rr, topN := s.buildReranker(ctx, tid, st)
	fetchK := finalK
	if rr != nil && topN > fetchK {
		fetchK = topN
	}

	p := Params{
		Embedding: out.Vectors[0],
		QueryText: req.Query,
		KVector:   st.KVector,
		KText:     st.KText,
		K:         fetchK,
		Filters:   req.Filters,
	}
	retrieve := s.Retriever
	if retrieve == nil {
		retrieve = Retrieve
	}
	results, err := retrieve(ctx, db, p)
	if err != nil {
		return nil, err
	}

	if rr != nil {
		results = s.applyRerank(ctx, tid, rr, req.Query, results, topN)
	}
	if len(results) > finalK {
		results = results[:finalK]
	}
	// query_retrieval_duration_seconds (SPEC-10 §2): timed only on the successful
	// path; reranked reflects whether the reranker actually ran. The tenant label is
	// the tenant id (bounded per tenant), consistent with the API request metric.
	s.Metrics.ObserveRetrieval(tid.String(), rr != nil, time.Since(start).Seconds())
	return results, nil
}

// buildReranker resolves the tenant's reranker (SPEC-06 §3). It returns (nil, 0)
// when reranking is disabled or no factory is wired, or when the factory fails
// (fail-closed build error, e.g. a missing key) — in which case it logs and the
// caller proceeds with the fused order (FR-RET-03 AC, NFR-REL-04). The second
// return is the top_n candidate count to over-fetch and rerank.
func (s *Service) buildReranker(ctx context.Context, tid tenant.ID, st Settings) (rerank.Reranker, int) {
	if s.Reranker == nil || !st.RerankEnabled {
		return nil, 0
	}
	rr, err := s.Reranker.Reranker(ctx, st)
	if err != nil {
		// The reranker errors carry only sanitised provider status, never query or
		// content (C-4); safe to log at warn. The query still succeeds on fused order.
		slog.WarnContext(ctx, "reranker unavailable; falling back to fused order",
			"tenant", tid.String(), "provider", st.RerankProvider, "err", err)
		return nil, 0
	}
	if rr == nil { // factory reported the reranker disabled
		return nil, 0
	}
	topN := st.RerankTopN
	if topN <= 0 {
		topN = defaultRerankTopN
	}
	return rr, topN
}

// applyRerank sends the top headN fused results to the reranker and reorders them
// by reranker score (SPEC-06 §3). ANY reranker failure falls back to the original
// fused order (the query never fails on it, FR-RET-03 AC). Results beyond headN
// (only when final_k > top_n) keep their fused position.
func (s *Service) applyRerank(ctx context.Context, tid tenant.ID, rr rerank.Reranker, query string, results []Result, topN int) []Result {
	headN := topN
	if headN > len(results) {
		headN = len(results)
	}
	if headN == 0 {
		return results
	}
	docs := make([]rerank.Doc, headN)
	for i := 0; i < headN; i++ {
		docs[i] = rerank.Doc{ID: results[i].ChunkID, Text: results[i].Content}
	}
	scored, err := rr.Rerank(ctx, query, docs)
	if err != nil {
		slog.WarnContext(ctx, "reranker failed; falling back to fused order",
			"tenant", tid.String(), "err", err)
		return results
	}
	return reorderByScore(results, scored, headN)
}

// reorderByScore rebuilds the result slice in the reranker's returned order,
// replacing each reranked result's Score with its reranker relevance score
// (SPEC-06 §3: "min_score then applies to reranker score"). Head results the
// reranker omitted are appended after the ranked ones (defence — rerank providers
// return every doc); the untouched tail (beyond headN) follows unchanged.
func reorderByScore(results []Result, scored []rerank.Scored, headN int) []Result {
	byID := make(map[string]Result, headN)
	for _, r := range results[:headN] {
		byID[r.ChunkID] = r
	}
	out := make([]Result, 0, len(results))
	used := make(map[string]bool, headN)
	for _, sc := range scored {
		r, ok := byID[sc.ID]
		if !ok || used[sc.ID] {
			continue
		}
		used[sc.ID] = true
		r.Score = sc.Score
		out = append(out, r)
	}
	for _, r := range results[:headN] {
		if !used[r.ChunkID] {
			out = append(out, r)
		}
	}
	return append(out, results[headN:]...)
}

// open resolves the tenant to its *tenant.DB, mapping the resolver's lifecycle
// outcomes to ErrTenantUnavailable so the caller never leaks internal detail
// (mirrors documents.Service.open — ADR-0003, the only place a handle is obtained).
// A suspended tenant resolves to a read-only handle, on which retrieval (a
// read-only transaction) still works.
func (s *Service) open(ctx context.Context, tid tenant.ID) (*tenant.DB, error) {
	db, err := s.Resolver.Open(ctx, tid)
	if err != nil {
		switch {
		case errors.Is(err, tenant.ErrTenantUnavailable),
			errors.Is(err, tenant.ErrTenantNotFound),
			errors.Is(err, tenant.ErrSchemaOutdated):
			return nil, ErrTenantUnavailable
		default:
			return nil, fmt.Errorf("retrieve: open tenant: %w", err)
		}
	}
	return db, nil
}

// finalK resolves the top_k for a request: the requested value, or the tenant's
// settings.retrieval.final_k when unset (Retrieve applies the ADR-0007 default of 8
// when both are zero), clamped to the ceiling.
func (s *Service) finalK(requested, settingsFinalK int) int {
	k := requested
	if k <= 0 {
		k = settingsFinalK
	}
	ceiling := s.MaxTopK
	if ceiling <= 0 {
		ceiling = defaultMaxTopK
	}
	if k > ceiling {
		k = ceiling
	}
	return k
}

// parseSettings extracts the retrieval-relevant fields from a settings document
// (SPEC-02 §5). Missing/typed-wrong fields fall back to zero, which downstream
// treats as its default (Retrieve/withDefaults for the k's; the embedder factory
// still needs a real provider/model). JSON numbers decode as float64.
func parseSettings(doc map[string]any) Settings {
	var s Settings
	if emb, ok := doc["embedding"].(map[string]any); ok {
		s.EmbeddingProvider, _ = emb["provider"].(string)
		s.EmbeddingModel, _ = emb["model"].(string)
		s.EmbeddingDim = toInt(emb["dim"])
	}
	if ret, ok := doc["retrieval"].(map[string]any); ok {
		s.KVector = toInt(ret["k_vector"])
		s.KText = toInt(ret["k_text"])
		s.FinalK = toInt(ret["final_k"])
	}
	if allowed, ok := doc["providers_allowed"].([]any); ok {
		for _, a := range allowed {
			if p, ok := a.(string); ok {
				s.ProvidersAllowed = append(s.ProvidersAllowed, p)
			}
		}
	}
	if rk, ok := doc["reranker"].(map[string]any); ok {
		s.RerankEnabled, _ = rk["enabled"].(bool)
		s.RerankProvider, _ = rk["provider"].(string)
		s.RerankModel, _ = rk["model"].(string)
		s.RerankTopN = toInt(rk["top_n"])
		s.RerankLLMModel, _ = rk["llm_model"].(string)
	}
	if l, ok := doc["llm"].(map[string]any); ok {
		s.LLMProvider, _ = l["provider"].(string)
		s.LLMModel, _ = l["model"].(string)
		if ma, ok := l["models_allowed"].([]any); ok {
			for _, a := range ma {
				if m, ok := a.(string); ok {
					s.LLMModelsAllowed = append(s.LLMModelsAllowed, m)
				}
			}
		}
	}
	return s
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

// KeyedEmbedderFactory is the production EmbedderFactory: it builds the shared
// embed.Embedder from the tenant's settings, authenticating with the platform's
// embedding-provider API key. embed.New fails closed on the tenant's
// providers_allowed (SPEC-09 §2) before building anything.
//
// ponytail: one API key per deployment. Under C-5 (single-region, per-tenant
// deployment) a deployment serves one embedding provider, so a single key
// suffices; when a deployment ever serves tenants on heterogeneous providers, make
// this a provider→key map. BaseURL is for a self-hosted/proxy endpoint (e.g. TEI).
type KeyedEmbedderFactory struct {
	APIKey  string
	BaseURL string
	// Metrics is threaded into the embedder for provider_request metrics (SPEC-10 §2).
	Metrics *obs.Metrics
}

// Embedder builds the embedder for the tenant's configured provider/model.
func (f KeyedEmbedderFactory) Embedder(_ context.Context, s Settings) (embed.Embedder, error) {
	return embed.New(embed.Config{
		Provider: s.EmbeddingProvider,
		Model:    s.EmbeddingModel,
		Allowed:  s.ProvidersAllowed,
		APIKey:   f.APIKey,
		BaseURL:  f.BaseURL,
		Metrics:  f.Metrics,
	})
}

// KeyedRerankerFactory is the production RerankerFactory (SPEC-06 §3, STORY-08.3).
// It builds the tenant's reranker from settings.reranker: the Cohere reranker
// authenticates with the platform CohereAPIKey (fail-closed on the tenant's
// providers_allowed and on a missing key, inside rerank.New); the LLM reranker
// reuses the tenant's configured LLM provider, built through the llm.Factory (which
// enforces the provider + model allowlists, SPEC-09 §2), using the
// reranker.llm_model override when set, else settings.llm.model. A disabled
// reranker yields a nil Reranker (no rerank). Keys are never logged (C-4).
//
// ponytail: one Cohere key per deployment (C-5 single-region-per-tenant), mirroring
// KeyedEmbedderFactory; make it a provider→key map only for heterogeneous
// deployments.
type KeyedRerankerFactory struct {
	CohereAPIKey  string
	CohereBaseURL string
	// LLM builds the tenant's llm.Provider for the LLM-based reranker. Its zero value
	// still builds providers, but a nil per-provider key makes that provider fail
	// with a clean auth error (which the service turns into a fallback).
	LLM llm.Factory
	// Metrics is threaded into the reranker for provider_request metrics (SPEC-10 §2).
	Metrics *obs.Metrics
}

// Reranker builds the tenant's reranker, or (nil, nil) when reranking is disabled.
func (f KeyedRerankerFactory) Reranker(_ context.Context, s Settings) (rerank.Reranker, error) {
	if !s.RerankEnabled {
		return nil, nil
	}
	cfg := rerank.Config{
		Enabled:       true,
		Provider:      s.RerankProvider,
		TopN:          s.RerankTopN,
		Allowed:       s.ProvidersAllowed,
		Model:         s.RerankModel,
		CohereAPIKey:  f.CohereAPIKey,
		CohereBaseURL: f.CohereBaseURL,
		Metrics:       f.Metrics,
	}
	if s.RerankProvider == rerank.ProviderLLM {
		model := s.RerankLLMModel
		if model == "" {
			model = s.LLMModel
		}
		// llm.Factory.Provider fails closed on the provider + model allowlists before
		// any key is used; a build error propagates and the service falls back.
		provider, err := f.LLM.Provider(s.LLMProvider, model, s.ProvidersAllowed, s.LLMModelsAllowed)
		if err != nil {
			return nil, err
		}
		cfg.LLM = provider
		cfg.LLMModel = model
	}
	return rerank.New(cfg)
}
