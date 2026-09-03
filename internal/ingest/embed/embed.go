// Package embed turns chunk text into embedding vectors for retrieval (SPEC-05
// §4, FR-ING-05). It provides the Embedder seam and four provider
// implementations (OpenAI, Voyage, Cohere, TEI), and wraps a provider with the
// SPEC-05 §4 batching (≤96 texts / ≤100k tokens per request), bounded
// per-tenant concurrency (default 4 batches in flight), a per-provider circuit
// breaker, and exponential backoff honouring Retry-After on 429/5xx.
//
// The chunker (internal/ingest/chunk) produces Chunk.EmbedText; the sink
// (STORY-05.6) calls Embed with those texts, then maps each returned vector plus
// the configured model name onto documents.ChunkInput.Embedding /
// .EmbeddingModel. Embed reports token usage in Result.Tokens so the sink can
// record it into jobs.stats.embed_tokens and usage_daily (usage.Delta.EmbedTokens,
// STORY-03.7) — this package surfaces the count; STORY-05.6 writes it (ADR-0037).
//
// Provider access is fail-closed against the tenant's settings.providers_allowed
// (SPEC-09 §2): New refuses to build an embedder for a provider absent from the
// allowlist, so a tenant's content can never reach a provider it did not permit.
package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
)

// Defaults from SPEC-05 §4 (§8 for retries).
const (
	DefaultMaxBatchTexts    = 96
	DefaultMaxBatchTokens   = 100_000
	DefaultConcurrency      = 4
	DefaultMaxRetries       = 2 // 3 attempts (SPEC-05 §8)
	DefaultBreakerThreshold = 5
	DefaultBreakerCooldown  = 30 * time.Second
	defaultRequestTimeout   = 60 * time.Second
)

// Errors surfaced by New and the batching driver. All are checkable with
// errors.Is so callers (the sink) can react — e.g. snooze the job on
// ErrCircuitOpen rather than fail it (SPEC-05 §8).
var (
	// ErrProviderNotAllowed means the requested provider is not in the tenant's
	// settings.providers_allowed (fail-closed, incl. an empty allowlist).
	ErrProviderNotAllowed = errors.New("embed: provider not permitted by allowlist")
	// ErrUnknownProvider means the provider name has no implementation.
	ErrUnknownProvider = errors.New("embed: unknown provider")
	// ErrCircuitOpen means the provider's circuit breaker is open; the call was
	// short-circuited without hitting the provider.
	ErrCircuitOpen = errors.New("embed: circuit breaker open")
)

// Result is one embedding operation's output: vectors aligned one-to-one with the
// input texts, plus the provider-reported token usage. Tokens is 0 when the
// provider's API omits usage (TEI).
type Result struct {
	Vectors [][]float32
	Tokens  int
}

// Embedder embeds a batch of texts into vectors (SPEC-05 §4). It is the single
// interface a new provider implements (NFR-MNT-02). SPEC-05 §4 names the method
// Embed(ctx, []string) ([][]float32, error); it returns a Result instead so the
// token usage the same section requires be recorded is surfaced alongside the
// vectors rather than lost — Result.Vectors is exactly that [][]float32 (ADR-0037).
//
// The value New returns is itself an Embedder: it accepts an arbitrary number of
// texts, splits them into provider-sized batches, and runs them with bounded
// concurrency, breaker and retries — so the sink calls Embed once with every
// chunk's text.
type Embedder interface {
	Embed(ctx context.Context, texts []string) (Result, error)
}

// Config selects and configures a provider plus its batching/resilience.
type Config struct {
	// Provider is the provider name ("openai", "voyage", "cohere", "tei").
	Provider string
	// Allowed is the tenant's settings.providers_allowed. New fails closed unless
	// Provider is in this set (SPEC-09 §2).
	Allowed []string

	// APIKey authenticates to the provider (Bearer). Never logged. Optional for a
	// self-hosted TEI.
	APIKey string
	// Model is the provider's embedding model id (ignored by TEI, which serves one
	// model per process).
	Model string
	// BaseURL overrides the provider endpoint (self-hosted TEI, a proxy, tests).
	// Empty uses the provider default.
	BaseURL string
	// HTTPClient overrides the transport (and its timeout). Default carries
	// defaultRequestTimeout.
	HTTPClient *http.Client
	// MaxRetries is retries after the first attempt on a transient failure. 0 uses
	// DefaultMaxRetries.
	MaxRetries int

	// Batching (SPEC-05 §4). Zero values use the defaults.
	MaxBatchTexts  int
	MaxBatchTokens int
	Concurrency    int
	// CountTokens estimates a text's token count for the ≤MaxBatchTokens budget.
	// Nil uses a chars/4 approximation; the sink can inject the chunker's counter.
	CountTokens func(string) int

	// Circuit breaker (SPEC-05 §4). Zero values use the defaults.
	BreakerThreshold int
	BreakerCooldown  time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxRetries <= 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.MaxBatchTexts <= 0 {
		c.MaxBatchTexts = DefaultMaxBatchTexts
	}
	if c.MaxBatchTokens <= 0 {
		c.MaxBatchTokens = DefaultMaxBatchTokens
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = DefaultBreakerThreshold
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = DefaultBreakerCooldown
	}
	if c.CountTokens == nil {
		c.CountTokens = approxTokens
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: defaultRequestTimeout}
	}
	return c
}

// New builds an Embedder for cfg.Provider. It fails closed on the allowlist
// (ErrProviderNotAllowed) before anything else, then rejects an unknown provider
// (ErrUnknownProvider). The returned Embedder wraps the raw provider with
// batching, bounded concurrency, a circuit breaker and per-request retries.
func New(cfg Config) (Embedder, error) {
	if !allowed(cfg.Provider, cfg.Allowed) {
		return nil, fmt.Errorf("%w: %q", ErrProviderNotAllowed, cfg.Provider)
	}
	build, ok := registry[cfg.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, cfg.Provider)
	}
	cfg = cfg.withDefaults()
	return &batcher{
		inner:       build(cfg),
		breaker:     newBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown),
		tracer:      otel.Tracer("ingest/embed"),
		maxTexts:    cfg.MaxBatchTexts,
		maxTokens:   cfg.MaxBatchTokens,
		concurrency: cfg.Concurrency,
		count:       cfg.CountTokens,
	}, nil
}

func allowed(provider string, list []string) bool {
	for _, p := range list {
		if p == provider {
			return true
		}
	}
	return false
}

// batcher wraps a raw provider Embedder with the SPEC-05 §4 batching, bounded
// concurrency and per-provider circuit breaker. It is itself an Embedder.
type batcher struct {
	inner       Embedder
	breaker     *breaker
	tracer      trace.Tracer
	maxTexts    int
	maxTokens   int
	concurrency int
	count       func(string) int
}

type batchRange struct {
	offset int
	texts  []string
}

func (b *batcher) Embed(ctx context.Context, texts []string) (Result, error) {
	if len(texts) == 0 {
		return Result{Vectors: [][]float32{}}, nil
	}
	ctx, span := b.tracer.Start(ctx, "embed.batch", trace.WithAttributes(
		attribute.Int("embed.texts", len(texts)),
	))
	defer span.End()

	batches := b.partition(texts)
	vecs := make([][]float32, len(texts))
	var tokens int64

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(b.concurrency)
	for _, bt := range batches {
		bt := bt
		g.Go(func() error {
			if err := b.breaker.allow(); err != nil {
				return err
			}
			res, err := b.inner.Embed(ctx, bt.texts)
			b.breaker.record(err)
			if err != nil {
				return err
			}
			if len(res.Vectors) != len(bt.texts) {
				return fmt.Errorf("embed: provider returned %d vectors for %d texts", len(res.Vectors), len(bt.texts))
			}
			copy(vecs[bt.offset:], res.Vectors)
			atomic.AddInt64(&tokens, int64(res.Tokens))
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		span.RecordError(err)
		return Result{}, err
	}
	span.SetAttributes(
		attribute.Int("embed.batches", len(batches)),
		attribute.Int("embed.tokens", int(tokens)),
	)
	return Result{Vectors: vecs, Tokens: int(tokens)}, nil
}

// partition splits texts into consecutive batches bounded by maxTexts and the
// maxTokens budget (SPEC-05 §4), preserving order. A single text that alone
// exceeds the token budget still forms its own batch (the provider, not this
// splitter, enforces its own per-input limit).
func (b *batcher) partition(texts []string) []batchRange {
	var out []batchRange
	i := 0
	for i < len(texts) {
		start := i
		tok := 0
		for i < len(texts) {
			n := b.count(texts[i])
			if i > start && (i-start >= b.maxTexts || tok+n > b.maxTokens) {
				break
			}
			tok += n
			i++
		}
		out = append(out, batchRange{offset: start, texts: texts[start:i]})
	}
	return out
}

// approxTokens is the default token estimate for batch sizing: the ~4 chars per
// token rule of thumb. ponytail: rough (a real cl100k_base count differs); inject
// Config.CountTokens (the chunker already has one) when the ≤100k budget must be
// tight. Only affects how texts are grouped into requests, never correctness.
func approxTokens(s string) int { return (len(s) + 3) / 4 }
