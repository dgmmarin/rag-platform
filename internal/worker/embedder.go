package worker

import (
	"context"

	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/ingestdoc"
	"github.com/rag-platform/ragctl/internal/obs"
)

// KeyedEmbedderFactory is the production ingestdoc.EmbedderFactory: it builds the
// shared embed.Embedder from a tenant's settings, authenticating with the
// platform's embedding-provider API key. embed.New fails closed on the tenant's
// providers_allowed (SPEC-09 §2) before building anything, so a tenant's data never
// reaches a provider it has not allowed. The key is never logged (C-4).
//
// It mirrors retrieve.KeyedEmbedderFactory so query embeddings and ingest
// embeddings share an embedding space and the same allowlist enforcement.
//
// ponytail: one API key per deployment. Under C-5 (single-region, per-tenant
// deployment) a deployment serves one embedding provider, so a single key suffices;
// make this a provider→key map only when a deployment serves heterogeneous providers.
type KeyedEmbedderFactory struct {
	APIKey  string
	BaseURL string
	// Concurrency / MaxBatchTexts throttle the embedder for a slow self-hosted
	// provider (a CPU TEI/Ollama endpoint serves roughly serially). 0 keeps the
	// embed package defaults (4 in flight, 96 texts/batch) — ISSUE-0063.
	Concurrency   int
	MaxBatchTexts int
	// Metrics is threaded into the embedder for provider_request metrics (SPEC-10 §2).
	Metrics *obs.Metrics
}

// Embedder builds the embedder for the tenant's configured provider/model.
func (f KeyedEmbedderFactory) Embedder(_ context.Context, s ingestdoc.Settings) (embed.Embedder, error) {
	return embed.New(embed.Config{
		Provider:      s.EmbeddingProvider,
		Model:         s.EmbeddingModel,
		Allowed:       s.ProvidersAllowed,
		APIKey:        f.APIKey,
		BaseURL:       f.BaseURL,
		Concurrency:   f.Concurrency,
		MaxBatchTexts: f.MaxBatchTexts,
		Metrics:       f.Metrics,
	})
}

// Ensure the factory satisfies the ingestdoc seam it is injected into.
var _ ingestdoc.EmbedderFactory = KeyedEmbedderFactory{}
