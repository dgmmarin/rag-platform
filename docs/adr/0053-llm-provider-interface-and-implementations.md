# ADR-0053: LLM provider interface and implementations — provider-neutral `Provider` seam, official SDK for Anthropic + raw HTTP for OpenAI, breaker/retry reuse, normalised token accounting, two-level fail-closed allowlist

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** NFR-MNT-02, NFR-REL-04, FR-RET-04, FR-RET-06, SPEC-06 §1/§5–6, SPEC-09 §2, C-2, C-4, C-5 · **Decisions:** ADR-0002, ADR-0037

## Context
SPEC-06 §1 puts an LLM generation stage after retrieval/grounding: the answering
layer (STORY-08.5 prompt assembly, STORY-08.6 the `POST /v1/query` endpoint with
SSE, FR-RET-04/06) assembles a grounded prompt and calls an LLM to produce the
cited answer; the LLM-based reranker (STORY-08.3, FR-RET-03) also needs a
non-streaming completion. STORY-08.4 builds the provider seam those consume,
**before** 08.3 (a deliberate, user-approved reorder — the reranker depends on
this). The AC: Anthropic, OpenAI and OpenAI-compatible (vLLM/Ollama) providers;
streaming and non-streaming; retries; token accounting; allowlist enforced. This
mirrors the embedding provider seam (ADR-0037) one layer up. Constraints: Go
primary with SDKs only where justified (C-2/ADR-0002), secrets never logged (C-4),
providers gated by `settings.providers_allowed` (SPEC-09 §2), graceful degradation
on provider loss (NFR-REL-04).

## Options
- **Interface shape.** A provider-neutral `Provider` with two methods —
  `Complete(ctx, Request) (Response, error)` and `Stream(ctx, Request) (Stream,
  error)` — rather than one method with a `stream bool` (the two return shapes
  differ: a buffered `Response` vs a pull iterator). `Request` carries the
  provider-neutral knobs the user specified: `{Model, System, Messages, MaxTokens,
  Temperature, TopP, Effort, Stream}`. `Response` carries `{Text, Usage,
  FinishReason, Model}`. `Request.Stream` is retained as advisory metadata for the
  caller's bookkeeping but the method invoked is the real switch (documented on the
  field) — a small, deliberate deviation from the literal "stream on/off knob" that
  keeps the return types honest.
- **Streaming abstraction.** A pull-based `Stream` iterator (`Recv() (Event,
  error)` returning `io.EOF` at end, plus `Close`) rather than a channel: it keeps
  cancellation explicit (via the request context + `Close`) with no goroutine to
  leak, and maps cleanly onto both the SDK's SSE stream and the raw OpenAI SSE
  stream. `Event` yields either a `TextDelta` or the single terminal `Done` event
  carrying final `Usage`/`FinishReason`. Each provider's `Recv` loops internally
  over transport events and surfaces only text deltas + the terminal event, so
  08.6 consumes one uniform shape.
- **Provider clients — SDK vs raw HTTP (the user's decision).** **Anthropic uses
  the official `github.com/anthropics/anthropic-sdk-go`** (approved new dependency);
  **OpenAI and OpenAI-compatible use hand-rolled raw `net/http`** against the
  OpenAI `POST /v1/chat/completions` shape — one implementation parameterised by
  base URL serves OpenAI, vLLM, Ollama and any compatible endpoint, consistent with
  the embed providers (ADR-0037) and C-2/ADR-0002 (no OpenAI SDK dependency). The
  SDK is pinned to **v1.9.0** — the newest release whose `go` directive (1.21) is
  compatible with the repo's `go 1.22`; every release ≥ v1.9.1 requires go ≥ 1.23
  and ≥ v1.47.0 requires go ≥ 1.24, which would force a toolchain bump this change
  refuses to make. `go mod tidy` stayed on `go 1.22` with no `toolchain` line.
- **Retry/breaker — reuse vs share (NFR-REL-04).** The retry semantics
  (`transientError` carrying `Retry-After`, capped exponential `backoff`,
  context-cancellable `sleep`, 429/5xx transient / other-4xx terminal) and the
  three-state circuit breaker are **reused from `internal/ingest/embed`** (ADR-0037)
  — but **copied, not shared**: embed's helpers are coupled to embed's private error
  type and extracting a shared package would force a change to the stable embed
  package for no behavioural gain (against NFR-MNT-01/02's "no ripple outside the
  package"). A `ponytail:` note marks a third consumer as the trigger to extract
  `internal/resilience`. The retry driver is generic (`retryValue[T]`) so `Complete`
  and stream-establishment share one loop.
- **SDK retry vs ours.** The SDK's built-in retry is **disabled**
  (`option.WithMaxRetries(0)`) so this package's wrapper is the single retry
  authority — identical bounded-backoff + breaker behaviour for Anthropic and
  OpenAI, and one place to reason about it.
- **Streaming + retry.** `openStream` establishes the stream **without emitting any
  answer text**, so retrying establishment is safe and rides the same `retryValue`
  loop. The OpenAI provider checks the initial HTTP status there (retryable on
  429/5xx). The Anthropic SDK surfaces stream errors lazily (only on the first
  `Next`), so an establishment failure cannot be caught eagerly to drive a retry —
  it surfaces on the first `Recv` instead; the breaker still guards streams and
  `Complete` gets full retry. Documented as a ponytail ceiling.
- **Sampling / thinking / effort.** Sampling (`temperature`/`top_p`) is sent
  **only to OpenAI**; current Claude models reject sampling params, so the Anthropic
  provider omits them. Thinking is **not hardcoded** (the effort/thinking policy is
  the caller's, 08.5): `Request.Effort` maps to OpenAI's `reasoning_effort` and is a
  no-op for the Anthropic provider on the pinned SDK (adaptive thinking is not in
  v1.9.0 and `budget_tokens` is rejected by current models). No assistant prefill,
  no `budget_tokens`.
- **Token accounting (FR-RET-04 answering usage).** Every `Response`/terminal
  `Event` carries a provider-normalised `Usage{InputTokens, OutputTokens}` — from
  Anthropic's `usage.input_tokens`/`output_tokens` (streaming: `message_start` for
  input, `message_delta` for output) and OpenAI's `usage.prompt_tokens`/
  `completion_tokens` (streaming via `stream_options.include_usage`). This is the
  count 08.5 folds into `usage_daily` (ADR-0024, the reserved "LLM tokens in
  EPIC-08 answering" counter) and the response `usage` object; this package surfaces
  it, it does not write it.
- **Allowlist — two levels, fail-closed (NFR-MNT-02, SPEC-09 §2).** `New` fails
  closed on the **provider allowlist** (`settings.providers_allowed`,
  `ErrProviderNotAllowed`, incl. an empty allowlist) before constructing anything —
  identical to embed. The user's decision also names a **model allowlist**
  (`claude-opus-5`, `claude-sonnet-5`, `claude-haiku-4-5`, `gpt-*`): a new
  `settings.llm.models_allowed` list, enforced when non-empty (`ErrModelNotAllowed`)
  at both construction (the configured model) and call time (a per-request model
  override), with exact or trailing-`*` prefix matching. Empty imposes no
  model-level restriction (the provider allowlist still governs), and the shipped
  defaults populate it.
- **Default answer model / config.** `settings.llm.model` default becomes
  **`claude-sonnet-5`** (was `claude-sonnet-4-6`). Platform keys are supplied per
  provider from config/env — `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`,
  `OPENAI_BASE_URL` — mirroring `EMBEDDING_API_KEY`; a `Factory` selects the key by
  provider and builds through `New`. Keys are never logged or returned, and errors
  carry only the sanitised HTTP status + the provider's own error snippet, never the
  request body (system/prompt content) (C-4).

## Decision
Add `internal/llm`: the `Provider` interface (`Complete`/`Stream`), `Request`/
`Response`/`Usage`/`Event`/`Stream`/`Message` types, `Config`, `New(cfg)
(Provider, error)`, a generic retry+breaker wrapper (`resilient`), a copied
`breaker`, retry helpers mirroring `internal/ingest/embed`, a `registry` (one entry
per provider name), and the three providers — `anthropicProvider` (anthropic-sdk-go
v1.9.0, SDK retry disabled), `openAI` (raw HTTP, serving `openai` and
`openai-compatible`). Exported errors `ErrProviderNotAllowed`, `ErrUnknownProvider`,
`ErrCircuitOpen`, `ErrModelNotAllowed`. A `Factory`+`Keys` seam selects the
per-provider platform key. `settings_defaults.json`/`settings_schema.json` gain
`llm.models_allowed` and set the default model to `claude-sonnet-5`;
`internal/config` + `.env.example` gain the three keys. SPEC-06 is updated with the
provider-contract section.

## Consequences
- **The seam 08.3/08.5/08.6 consume.** The reranker calls `Complete`; prompt
  assembly (08.5) builds `System`+`Messages`, reads `Response.Text`/`Usage`/
  `FinishReason`; the query endpoint (08.6) consumes `Stream` for SSE `delta`/`done`.
  Adding a fourth provider is one file implementing the internal `rawProvider` plus
  one `registry` line — no change elsewhere (NFR-MNT-02).
- **Graceful degradation (NFR-REL-04).** `ErrCircuitOpen` is exported and
  `errors.Is`-checkable so 08.6 can fall back to retrieval-only when the LLM
  provider is down.
- **Dependency.** `github.com/anthropics/anthropic-sdk-go v1.9.0` added (with its
  transitive tree for the unused Bedrock/Vertex paths — pulled by `go mod tidy` as
  indirect); `golang.org/x/time` bumped v0.3.0→v0.5.0; `tidwall/gjson`,`sjson`,
  `pretty`,`match` added (SDK deps). No OpenAI SDK. `go 1.22` unchanged.
- **No migration/OpenAPI change.** `settings` is jsonb; only the defaults/schema
  JSON changed (drift/validation stays green). No endpoint exists yet (08.6), so no
  OpenAPI change.
- **Ceilings (ponytail):** (1) Anthropic stream establishment errors surface on the
  first `Recv` (SDK lazy errors), not retried — breaker still guards, `Complete`
  fully retried; upgrade when the SDK exposes eager stream errors. (2) `Request.
  Effort` is a no-op for Anthropic on SDK v1.9.0 (no adaptive thinking; budget-based
  thinking rejected by current models) — revisit when the pinned SDK advances past
  go 1.22. (3) The breaker/retry are copied from embed, not shared — extract
  `internal/resilience` on a third consumer. (4) One platform key per provider per
  deployment (C-5) — make it a tenant→key map only for heterogeneous deployments.
- **No e2e in this story (precedent: ADR-0037).** Like the embedding provider,
  `internal/llm` is a pure client library with no HTTP/worker path of its own; its
  golden-path e2e arrives with the query endpoint (STORY-08.6), exactly as embed's
  arrived with the sink (STORY-05.6). This story is covered by a hermetic `httptest`
  suite (per provider: non-streaming + streaming text/usage/finish, request shape,
  retry-then-succeed on 5xx/429, terminal-error-not-retried + sanitisation, breaker
  opens + short-circuits, provider allowlist fail-closed, model allowlist
  exact/wildcard/per-request/empty, unknown provider, factory key selection) — the
  Anthropic SDK is pointed at an `httptest` server via `option.WithBaseURL`. No real
  network or keys.
- **Coverage.** `internal/llm` is not under the coverage-gated paths
  (tenant/ingest/retrieve/connector); its behaviour is unit-covered as above.
