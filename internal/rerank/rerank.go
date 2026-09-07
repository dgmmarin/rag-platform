// Package rerank re-orders the fused hybrid-retrieval candidates by relevance to
// the query before generation (SPEC-06 §3, FR-RET-03). It is the seam the retrieve
// service applies when settings.reranker.enabled: the top top_n fused results go to
// Reranker.Rerank(query, docs), and the service re-orders by the returned reranker
// score.
//
// Two providers implement the single Reranker interface (NFR-MNT-02): a Cohere
// reranker (real HTTP against the Cohere v2 /v2/rerank API, wrapped with the same
// bounded-backoff retry + circuit breaker the embedding/LLM seams use) and an
// LLM-based reranker that scores ALL candidates in ONE batched llm.Complete call (a
// listwise prompt — never per-document calls). New selects the provider from
// settings.reranker.provider and fails closed on the provider allowlist (Cohere)
// and on a missing key/provider; a disabled reranker is (nil, nil).
//
// Fallback (FR-RET-03 AC, NFR-REL-04): a reranker never fails the query. Any error
// — network, breaker-open, missing key, unparseable LLM output — is returned to the
// caller (the retrieve service), which logs it and falls back to the original fused
// order. Secrets are never logged or returned (C-4).
package rerank

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/rag-platform/ragctl/internal/llm"
)

// Provider names selectable via settings.reranker.provider.
const (
	ProviderCohere = "cohere"
	ProviderLLM    = "llm"
)

// Errors surfaced by New and Rerank, all errors.Is-checkable so the retrieve
// service can react (it falls back to fused order on any of them).
var (
	// ErrProviderNotAllowed means the reranker provider is not in the tenant's
	// settings.providers_allowed (fail-closed, SPEC-09 §2). Applies to Cohere (the
	// LLM reranker's underlying provider is gated upstream when the llm.Provider is
	// built).
	ErrProviderNotAllowed = errors.New("rerank: provider not permitted by allowlist")
	// ErrUnknownProvider means the reranker provider name has no implementation.
	ErrUnknownProvider = errors.New("rerank: unknown provider")
	// ErrMissingKey means the selected provider is enabled but not usable: the
	// Cohere API key is absent, or the LLM reranker has no llm.Provider wired.
	// Fail-closed — the service treats it as a reranker failure and falls back.
	ErrMissingKey = errors.New("rerank: provider selected but not configured")
	// ErrCircuitOpen means the provider's circuit breaker is open (Cohere path).
	ErrCircuitOpen = errors.New("rerank: circuit breaker open")
	// ErrUnparseable means the LLM reranker's output could not be parsed into a
	// ranking (defensive parse failed). Treated as a provider failure → fallback.
	ErrUnparseable = errors.New("rerank: could not parse reranker output")
)

// Doc is one candidate passed to a reranker: the chunk id and the text scored
// against the query. The embedding vector is never involved.
type Doc struct {
	ID   string
	Text string
}

// Scored is one reranked candidate: the chunk id and its reranker relevance score.
// The service orders results by Score descending (SPEC-06 §3).
type Scored struct {
	ID    string
	Score float64
}

// Reranker re-orders candidates by relevance to the query. It is the single
// interface a new reranker provider implements (NFR-MNT-02). Rerank returns one
// Scored per input Doc (a doc a provider omits is not dropped — it lands last), or
// an error the caller treats as a fallback-to-fused signal.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []Doc) ([]Scored, error)
}

// Completer is the subset of llm.Provider the LLM reranker needs: one batched
// non-streaming completion. *llm resilient providers satisfy it, and a fake
// satisfies it in tests, keeping this package testable without a network.
type Completer interface {
	Complete(ctx context.Context, req llm.Request) (llm.Response, error)
}

// Config selects and configures a reranker (from a tenant's settings.reranker plus
// the platform credentials). Enabled is the per-tenant toggle: New returns
// (nil, nil) when it is false, and the service skips reranking.
type Config struct {
	// Enabled is settings.reranker.enabled — the per-tenant toggle (default off).
	Enabled bool
	// Provider is settings.reranker.provider ("cohere" | "llm").
	Provider string
	// TopN is settings.reranker.top_n (informational here; the service trims the
	// fused set to TopN before calling Rerank).
	TopN int
	// Allowed is the tenant's settings.providers_allowed — the fail-closed gate for
	// the Cohere provider (SPEC-09 §2).
	Allowed []string

	// Model is settings.reranker.model — the Cohere rerank model (e.g. rerank-v3.5).
	Model string
	// CohereAPIKey authenticates to Cohere (platform key, never logged — C-4).
	CohereAPIKey string
	// CohereBaseURL overrides the Cohere endpoint (self-host/proxy/tests). Empty
	// uses the public API.
	CohereBaseURL string

	// LLM is the pre-built llm.Provider the LLM reranker calls (built by the caller
	// from settings.llm, so its provider/model allowlist is enforced upstream).
	LLM Completer
	// LLMModel is the model the LLM reranker requests (settings.reranker.llm_model
	// override, else settings.llm.model).
	LLMModel string

	// HTTPClient overrides the Cohere transport (a fixed short timeout is fine for a
	// rerank call, unlike streaming generation). Default has no timeout; the caller
	// bounds latency via the request context.
	HTTPClient *http.Client
	// MaxRetries / breaker knobs. Zero values use the defaults.
	MaxRetries       int
	BreakerThreshold int
	BreakerCooldown  time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxRetries <= 0 {
		c.MaxRetries = defaultMaxRetries
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = defaultBreakerThreshold
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = defaultBreakerCooldown
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
	return c
}

// New builds a Reranker for cfg. It returns (nil, nil) when the reranker is
// disabled (the per-tenant toggle), so the caller treats a nil Reranker as "no
// rerank". Otherwise it selects the provider and fails closed: Cohere requires the
// provider in the allowlist (ErrProviderNotAllowed) and a key (ErrMissingKey); the
// LLM reranker requires a wired llm.Provider (ErrMissingKey); an unknown provider
// is ErrUnknownProvider.
func New(cfg Config) (Reranker, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	switch cfg.Provider {
	case ProviderCohere:
		if !allowed(ProviderCohere, cfg.Allowed) {
			return nil, fmt.Errorf("%w: %q", ErrProviderNotAllowed, ProviderCohere)
		}
		if cfg.CohereAPIKey == "" {
			return nil, fmt.Errorf("%w: cohere api key", ErrMissingKey)
		}
		cfg = cfg.withDefaults()
		return &cohere{
			doer: &doer{
				provider:   ProviderCohere,
				httpc:      cfg.HTTPClient,
				maxRetries: cfg.MaxRetries,
				tracer:     otel.Tracer("rerank"),
				propagator: otel.GetTextMapPropagator(),
			},
			breaker: newBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown),
			baseURL: nonEmpty(cfg.CohereBaseURL, "https://api.cohere.com"),
			apiKey:  cfg.CohereAPIKey,
			model:   cfg.Model,
		}, nil
	case ProviderLLM:
		if cfg.LLM == nil {
			return nil, fmt.Errorf("%w: llm provider", ErrMissingKey)
		}
		return &llmReranker{
			provider: cfg.LLM,
			model:    cfg.LLMModel,
			tracer:   otel.Tracer("rerank"),
		}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, cfg.Provider)
	}
}

func allowed(provider string, list []string) bool {
	for _, p := range list {
		if p == provider {
			return true
		}
	}
	return false
}

// orderMissingLast completes a partial ranking: docs the provider scored keep their
// order (already sorted by the provider); any input doc the provider omitted is
// appended with score 0 so it is ranked last but never dropped (a later final_k
// truncation still sees a full candidate set). Duplicate ids from the provider are
// de-duplicated (first wins).
func orderMissingLast(docs []Doc, scored []Scored) []Scored {
	seen := make(map[string]bool, len(scored))
	out := make([]Scored, 0, len(docs))
	for _, s := range scored {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		out = append(out, s)
	}
	for _, d := range docs {
		if !seen[d.ID] {
			out = append(out, Scored{ID: d.ID, Score: 0})
		}
	}
	return out
}
