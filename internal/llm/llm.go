// Package llm turns a grounded prompt into a generated answer for the answering
// stage of retrieval (SPEC-06 §1, §5–6). It provides the provider-neutral
// Provider seam and three implementations — Anthropic (via the official
// anthropic-sdk-go), OpenAI and OpenAI-compatible (vLLM/Ollama/any endpoint that
// speaks the OpenAI POST /v1/chat/completions shape, via raw net/http) — each
// offering non-streaming Complete and streaming Stream, wrapped with bounded
// exponential-backoff retries and a per-provider circuit breaker (NFR-REL-04) and
// gated fail-closed by the tenant's settings.providers_allowed allowlist
// (NFR-MNT-02, SPEC-09 §2).
//
// This package is the seam the reranker (STORY-08.3, LLM-based rerank) and the
// answering layer (STORY-08.5 prompt assembly, STORY-08.6 the query endpoint's
// SSE) consume. It does no prompt assembly, citation mapping, grounding/refusal,
// or query logging — the caller supplies System + Messages and reads back the
// text, normalised token Usage (so 08.5 can fold LLM tokens into usage_daily,
// ADR-0024) and finish reason. Adding a fourth provider is one file implementing
// rawProvider plus one registry entry — no change elsewhere (NFR-MNT-02, ADR-0053).
//
// Secrets: the per-provider platform API key is passed via Config.APIKey (or the
// Factory's Keys), sent as the provider's auth header and never logged or returned
// in an error (C-4). Errors carry only the sanitised HTTP status and the
// provider's own error snippet — never the request body (system/prompt content).
package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rag-platform/ragctl/internal/obs"
)

// Defaults (NFR-REL-04). Retries mirror internal/ingest/embed.
const (
	DefaultMaxRetries       = 2 // 3 attempts total
	DefaultBreakerThreshold = 5
	DefaultBreakerCooldown  = 30 * time.Second
	// DefaultAnswerModel is the platform default answer model (SPEC-06 §5,
	// settings_defaults.json). Callers pass the tenant's configured model; this is
	// the documented fallback.
	DefaultAnswerModel = "claude-sonnet-5"
)

// Errors surfaced by New and the resilient wrapper. All are errors.Is-checkable so
// callers (08.5/08.6) can react — e.g. degrade to retrieval-only on ErrCircuitOpen
// (NFR-REL-04: loss of the LLM provider degrades gracefully).
var (
	// ErrProviderNotAllowed means the requested provider is not in the tenant's
	// settings.providers_allowed (fail-closed, incl. an empty allowlist).
	ErrProviderNotAllowed = errors.New("llm: provider not permitted by allowlist")
	// ErrUnknownProvider means the provider name has no implementation.
	ErrUnknownProvider = errors.New("llm: unknown provider")
	// ErrCircuitOpen means the provider's circuit breaker is open; the call was
	// short-circuited without hitting the provider.
	ErrCircuitOpen = errors.New("llm: circuit breaker open")
	// ErrModelNotAllowed means the requested model is not in the tenant's
	// settings.llm.models_allowed (fail-closed, when a model allowlist is set).
	ErrModelNotAllowed = errors.New("llm: model not permitted by allowlist")
)

// Role is a conversational role. The system prompt is carried by Request.System,
// not a message role (both provider APIs model it separately).
type Role string

// Conversational roles for Request.Messages.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one conversational turn.
type Message struct {
	Role    Role
	Content string
}

// Request is a provider-neutral completion request. Model and MaxTokens are
// required (Anthropic rejects a request without max_tokens). System is the system
// prompt; Messages are the alternating user/assistant turns.
type Request struct {
	// Model is the provider's model id (e.g. "claude-sonnet-5", "gpt-4o"). Empty
	// falls back to Config.Model.
	Model string
	// System is the system prompt (SPEC-06 §5). Optional.
	System string
	// Messages are the conversation turns, oldest first.
	Messages []Message
	// MaxTokens caps the generated output. Required (>0).
	MaxTokens int

	// Temperature / TopP are sampling knobs passed ONLY to providers that accept
	// them (OpenAI). Current Claude models reject sampling params, so the Anthropic
	// provider ignores them. Nil means the provider default.
	Temperature *float64
	TopP        *float64

	// Effort is an optional provider-neutral reasoning-effort hint ("low"/"medium"/
	// "high"), chosen by the caller (08.5). It maps to OpenAI's reasoning_effort;
	// the Anthropic provider on this SDK version ignores it (thinking policy is not
	// hardcoded here — see ADR-0053). Empty means the provider default.
	Effort string

	// Stream is advisory metadata for the caller's own bookkeeping; the actual
	// choice is which method is invoked (Complete vs Stream). It is not sent to any
	// provider.
	Stream bool
}

// Usage is provider-normalised token accounting for one completion (SPEC-06 §6).
// The answering layer folds these into usage_daily (ADR-0024) and the response
// usage object.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is a non-streaming completion's result.
type Response struct {
	Text         string
	Usage        Usage
	FinishReason string // normalised: "stop" | "length" | "content_filter" | "tool_use" | provider value
	Model        string // provider-echoed model id
}

// Event is one item from a streaming completion. Non-terminal events carry a
// TextDelta; the single terminal event has Done=true with the final Usage and
// FinishReason. After the terminal event, Recv returns io.EOF.
type Event struct {
	TextDelta    string
	Done         bool
	Usage        Usage
	FinishReason string
}

// Stream is a pull-based token stream (SPEC-06 §6). Recv returns the next Event,
// or io.EOF when the stream is exhausted. A pull iterator (not a channel) keeps
// cancellation explicit — the caller controls it via the request context and
// Close, with no goroutine to leak. Close is idempotent and releases the
// underlying connection.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// Provider generates answers. It is the single interface a new LLM provider
// implements (NFR-MNT-02). The value New returns is a Provider that wraps a raw
// provider with retries and a circuit breaker.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request) (Stream, error)
}

// rawProvider is one provider's single-attempt operations, wrapped by resilient.
// complete performs one request. openStream establishes a stream and MUST NOT
// emit any answer text before returning (so a transient establishment failure is
// safe to retry). Transient failures are returned as *transientError.
type rawProvider interface {
	complete(ctx context.Context, req Request) (Response, error)
	openStream(ctx context.Context, req Request) (Stream, error)
}

// Config selects and configures a provider plus its resilience wrapper.
type Config struct {
	// Provider is the provider name ("anthropic", "openai", "openai-compatible").
	Provider string
	// Allowed is the tenant's settings.providers_allowed. New fails closed unless
	// Provider is in this set (SPEC-09 §2).
	Allowed []string
	// AllowedModels is the tenant's settings.llm.models_allowed. When non-empty, a
	// requested model outside it fails closed (ErrModelNotAllowed); entries may be an
	// exact id or a trailing-wildcard prefix (e.g. "gpt-*"). Empty means no
	// model-level restriction (the provider allowlist still applies).
	AllowedModels []string

	// Model is the default model when a Request omits one.
	Model string
	// APIKey authenticates to the provider. Never logged (C-4).
	APIKey string
	// BaseURL overrides the provider endpoint (OpenAI-compatible self-hosted
	// vLLM/Ollama, a proxy, tests). Empty uses the provider default.
	BaseURL string
	// HTTPClient overrides the transport. The default has NO Timeout: LLM calls —
	// streaming especially — are bounded by the request context (SPEC-06 §7), not a
	// fixed client timeout that would truncate a long stream.
	HTTPClient *http.Client
	// MaxRetries is retries after the first attempt on a transient failure. 0 uses
	// DefaultMaxRetries.
	MaxRetries int

	// Circuit breaker. Zero values use the defaults.
	BreakerThreshold int
	BreakerCooldown  time.Duration

	// Metrics records provider_request_duration_seconds / provider_errors_total
	// (SPEC-10 §2). Optional: a nil Metrics is a no-op.
	Metrics *obs.Metrics
}

func (c Config) withDefaults() Config {
	if c.MaxRetries <= 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = DefaultBreakerThreshold
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = DefaultBreakerCooldown
	}
	if c.HTTPClient == nil {
		// No Timeout on purpose: streaming responses can outlast any fixed timeout;
		// the caller bounds latency via the request context (SPEC-06 §7).
		c.HTTPClient = &http.Client{}
	}
	return c
}

// registry maps a provider name to a rawProvider constructor. Adding a provider is
// a single entry here plus one file implementing rawProvider — no change elsewhere
// (NFR-MNT-02). The set is also the closed vocabulary the allowlist checks against.
var registry = map[string]func(Config) rawProvider{
	"openai": func(c Config) rawProvider {
		return newOpenAI(c, nonEmpty(c.BaseURL, "https://api.openai.com"))
	},
	// OpenAI-compatible endpoints (vLLM, Ollama, any OpenAI /v1/chat/completions
	// server) are the same client with a required BaseURL override — no default.
	"openai-compatible": func(c Config) rawProvider {
		return newOpenAI(c, c.BaseURL)
	},
	"anthropic": func(c Config) rawProvider {
		return newAnthropic(c)
	},
}

// New builds a Provider for cfg.Provider. It fails closed on the allowlist
// (ErrProviderNotAllowed) before anything else, then rejects an unknown provider
// (ErrUnknownProvider). The returned Provider wraps the raw provider with bounded
// retries and a per-provider circuit breaker.
func New(cfg Config) (Provider, error) {
	if !allowed(cfg.Provider, cfg.Allowed) {
		return nil, fmt.Errorf("%w: %q", ErrProviderNotAllowed, cfg.Provider)
	}
	build, ok := registry[cfg.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, cfg.Provider)
	}
	if err := checkModel(cfg.Model, cfg.AllowedModels); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	return &resilient{
		raw:           build(cfg),
		breaker:       newBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown),
		tracer:        otel.Tracer("llm"),
		maxRetries:    cfg.MaxRetries,
		provider:      cfg.Provider,
		defModel:      cfg.Model,
		allowedModels: cfg.AllowedModels,
		metrics:       cfg.Metrics,
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

// checkModel enforces the model allowlist fail-closed. An empty allowlist or an
// empty model imposes no restriction (the provider allowlist still governs which
// provider the tenant's data may reach); otherwise the model must match an entry
// exactly or via a trailing-"*" prefix (e.g. "gpt-*").
func checkModel(model string, allowedModels []string) error {
	if len(allowedModels) == 0 || model == "" {
		return nil
	}
	for _, pat := range allowedModels {
		if pat == model {
			return nil
		}
		if strings.HasSuffix(pat, "*") && strings.HasPrefix(model, strings.TrimSuffix(pat, "*")) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrModelNotAllowed, model)
}

// resilient wraps a rawProvider with retries and a circuit breaker. It is itself a
// Provider. Complete and Stream share one retry driver; openStream establishes the
// stream without emitting text, so retrying it is safe.
type resilient struct {
	raw           rawProvider
	breaker       *breaker
	tracer        trace.Tracer
	maxRetries    int
	provider      string
	defModel      string
	allowedModels []string
	metrics       *obs.Metrics
}

func (r *resilient) Complete(ctx context.Context, req Request) (Response, error) {
	req = r.withModel(req)
	if err := checkModel(req.Model, r.allowedModels); err != nil {
		return Response{}, err
	}
	start := time.Now()
	resp, err := retryValue(ctx, r, "llm.complete", func(ctx context.Context) (Response, error) {
		return r.raw.complete(ctx, req)
	})
	// provider_request_duration_seconds / provider_errors_total (SPEC-10 §2): one
	// observation per logical request (retries included). No prompt content in labels.
	r.metrics.ObserveProvider(r.provider, "llm.complete", err, time.Since(start).Seconds())
	return resp, err
}

func (r *resilient) Stream(ctx context.Context, req Request) (Stream, error) {
	req = r.withModel(req)
	if err := checkModel(req.Model, r.allowedModels); err != nil {
		return nil, err
	}
	start := time.Now()
	stream, err := retryValue(ctx, r, "llm.stream", func(ctx context.Context) (Stream, error) {
		return r.raw.openStream(ctx, req)
	})
	// Times stream ESTABLISHMENT (SPEC-10 §2); the stream body is consumed by the
	// caller. No prompt content in labels.
	r.metrics.ObserveProvider(r.provider, "llm.stream", err, time.Since(start).Seconds())
	return stream, err
}

func (r *resilient) withModel(req Request) Request {
	if req.Model == "" {
		req.Model = r.defModel
	}
	return req
}

// retryValue runs once per attempt through the circuit breaker with bounded
// exponential backoff honouring Retry-After. Transient failures (429/5xx/transport
// — surfaced as *transientError by the raw provider) are retried; any other error
// is terminal and returned immediately. ErrCircuitOpen short-circuits without
// running the call.
func retryValue[T any](ctx context.Context, r *resilient, span string, once func(context.Context) (T, error)) (T, error) {
	var zero T
	ctx, sp := r.tracer.Start(ctx, span, trace.WithAttributes(attribute.String("llm.provider", r.provider)))
	defer sp.End()

	var lastErr error
	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		if attempt > 0 {
			var te *transientError
			errors.As(lastErr, &te)
			if err := sleep(ctx, backoff(attempt, te)); err != nil {
				return zero, err
			}
			sp.AddEvent("retry", trace.WithAttributes(attribute.Int("attempt", attempt+1)))
		}
		if err := r.breaker.allow(); err != nil { // circuit open: fail fast
			sp.SetStatus(codes.Error, err.Error())
			return zero, err
		}
		v, err := once(ctx)
		r.breaker.record(err)
		if err == nil {
			return v, nil
		}
		lastErr = err
		var te *transientError
		if !errors.As(err, &te) { // terminal
			sp.RecordError(err)
			sp.SetStatus(codes.Error, err.Error())
			return zero, err
		}
	}
	err := fmt.Errorf("llm: %s: giving up after %d attempts: %w", r.provider, r.maxRetries+1, lastErr)
	sp.RecordError(err)
	sp.SetStatus(codes.Error, err.Error())
	return zero, err
}

// normalizeFinish maps a provider's finish/stop reason onto a small shared
// vocabulary so callers do not branch per provider. It handles both the OpenAI and
// Anthropic vocabularies; unknown values pass through unchanged.
func normalizeFinish(reason string) string {
	switch reason {
	case "end_turn", "stop", "stop_sequence":
		return "stop"
	case "max_tokens", "length":
		return "length"
	case "content_filter":
		return "content_filter"
	case "tool_use", "tool_calls":
		return "tool_use"
	default:
		return reason
	}
}

// Keys holds the platform's per-provider LLM credentials (C-4; never logged). One
// key per provider per deployment — under C-5 (single-region, per-tenant) a
// deployment serves a small fixed provider set. Supplied from config/env
// (ANTHROPIC_API_KEY, OPENAI_API_KEY, OPENAI_BASE_URL).
type Keys struct {
	Anthropic     string
	OpenAI        string
	OpenAIBaseURL string
}

func (k Keys) forProvider(provider string) (apiKey, baseURL string) {
	switch provider {
	case "anthropic":
		return k.Anthropic, ""
	case "openai":
		return k.OpenAI, ""
	case "openai-compatible":
		return k.OpenAI, k.OpenAIBaseURL
	default:
		return "", ""
	}
}

// Factory builds a Provider for a tenant's configured LLM settings, selecting the
// per-provider platform key. It is the seam STORY-08.3/08.5/08.6 consume: they
// hold one Factory (built from config) and call Provider with the tenant's
// settings.llm.{provider,model} and settings.providers_allowed. New fails closed
// on the allowlist before any key is used.
type Factory struct {
	Keys       Keys
	HTTPClient *http.Client
	// Metrics is threaded into every built provider for provider_request_duration_
	// seconds / provider_errors_total (SPEC-10 §2). Optional (nil = no-op).
	Metrics *obs.Metrics
}

// Provider builds the Provider for the tenant's configured provider/model, gated
// by the tenant's provider allowlist (providersAllowed) and optional model
// allowlist (modelsAllowed). Both fail closed via New.
func (f Factory) Provider(provider, model string, providersAllowed, modelsAllowed []string) (Provider, error) {
	apiKey, baseURL := f.Keys.forProvider(provider)
	return New(Config{
		Provider:      provider,
		Model:         model,
		Allowed:       providersAllowed,
		AllowedModels: modelsAllowed,
		APIKey:        apiKey,
		BaseURL:       baseURL,
		HTTPClient:    f.HTTPClient,
		Metrics:       f.Metrics,
	})
}
