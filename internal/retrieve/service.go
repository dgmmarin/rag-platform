package retrieve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rag-platform/ragctl/internal/ingest/embed"
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
	// MaxTopK overrides the top_k ceiling; 0 uses defaultMaxTopK.
	MaxTopK int
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

	p := Params{
		Embedding: out.Vectors[0],
		QueryText: req.Query,
		KVector:   st.KVector,
		KText:     st.KText,
		K:         s.finalK(req.TopK, st.FinalK),
		Filters:   req.Filters,
	}
	retrieve := s.Retriever
	if retrieve == nil {
		retrieve = Retrieve
	}
	return retrieve(ctx, db, p)
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
}

// Embedder builds the embedder for the tenant's configured provider/model.
func (f KeyedEmbedderFactory) Embedder(_ context.Context, s Settings) (embed.Embedder, error) {
	return embed.New(embed.Config{
		Provider: s.EmbeddingProvider,
		Model:    s.EmbeddingModel,
		Allowed:  s.ProvidersAllowed,
		APIKey:   f.APIKey,
		BaseURL:  f.BaseURL,
	})
}
