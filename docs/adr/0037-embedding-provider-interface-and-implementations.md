# ADR-0037: Embedding provider interface and implementations — `Result`-returning `Embedder`, dependency-free HTTP providers, batching/breaker/retry wrapper, fail-closed allowlist

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** FR-ING-05, NFR-MNT-02, SPEC-05 §4, SPEC-05 §6, SPEC-09 §2 · **Decisions:** ADR-0002, ADR-0024, ADR-0036

## Context
SPEC-05 §4 puts an embedding stage between the chunker (STORY-05.4, ADR-0036) and
the document store (STORY-05.1): the sink embeds each `chunk.Chunk.EmbedText` and
writes the vector + model onto `documents.ChunkInput`. It names an `Embedder`
interface and four providers (OpenAI, Voyage, Cohere, TEI), requires batching (≤96
texts / ≤100k tokens), bounded per-tenant concurrency (default 4 in flight),
retry/backoff honouring `Retry-After` on 429/5xx, a per-provider circuit breaker,
and that **token usage be recorded** into `jobs.stats` and `usage_daily`. FR-ING-05
and NFR-MNT-02 ("a new provider = implementing a single interface") are the
contract. This story builds the embedder; the sink that calls it and writes the
usage is STORY-05.6.

## Options
- **Interface shape — surfacing token usage.** SPEC-05 §4 writes the method as
  `Embed(ctx, []string) ([][]float32, error)`, but the *same section* requires the
  call's token usage be recorded. A bare `[][]float32` return cannot carry it, so
  the two sentences are only reconcilable by widening the return. (a) keep
  `[][]float32` and report tokens out-of-band via a second method or a mutable
  field — rejected: a per-call figure exposed as state is racy under the concurrent
  batches and easy to misattribute. (b) thread usage through `context` — rejected:
  invisible, side-channel coupling. (c) **return `Result{Vectors [][]float32,
  Tokens int}`** (chosen): `Vectors` is exactly the `[][]float32` the SPEC names
  (aligned to the input), `Tokens` is the provider-reported usage, the `error` is
  unchanged. This is an append-only widening of the named signature, not a
  contradiction; SPEC-05 §4 is updated to record it. `Tokens` is 0 when a provider
  omits usage (TEI), and the sink then falls back to the chunker's token counts.
- **Provider clients — SDKs vs stdlib.** Embedding endpoints are small JSON POSTs.
  Per ADR-0002 (Go primary, minimal deps) and the lazy-senior rule, use
  **`net/http` + `encoding/json`** — no vendor SDKs (OpenAI/Cohere/Voyage each ship
  a large transitive tree for one request shape). OpenAI and **Voyage share the
  identical schema** (`{model, input}` → `{data:[{index, embedding}],
  usage.total_tokens}`, Bearer auth) so they are **one implementation**
  (`openAICompatible`) parameterised by default base URL; Cohere (`{texts,
  input_type}` → `embeddings` + `meta.billed_units.input_tokens`) and TEI (`{inputs}`
  → bare `[[...]]`, no usage) get their own ~30-line files. A `registry` map is the
  single place a fifth provider is added (NFR-MNT-02) — no change elsewhere.
- **Where batching/concurrency/breaker live.** Not on each provider (they would
  duplicate it four times). A **provider `Embed` is one request**; the value
  `embed.New` returns is a `batcher` that *also* implements `Embedder`, partitions
  an arbitrary input into ≤96-text / ≤100k-token ranges, runs them through
  `errgroup` with `SetLimit(concurrency)` (already-present `golang.org/x/sync`, no
  new dep), guards each batch call with the breaker, and reassembles vectors in
  input order. So the sink calls `Embed` once with every chunk's text and gets
  ordered vectors + summed tokens; a new provider still implements only the
  one-request `Embedder`.
- **Retry/backoff/Retry-After.** Reuse the **exact approach from the sidecar
  client** (ADR-0035): a `transientError` carrying an optional `Retry-After`,
  `retryAfter()` parsing delta-seconds and HTTP-date, capped exponential `backoff`,
  and a context-cancellable `sleep`. 429/5xx (and transport errors) are transient
  and retried; every other non-2xx is terminal and returned immediately (so a 400/
  401 is not retried into the ground). Each request is wrapped in an OpenTelemetry
  span with W3C trace-context injection via the installed global propagator — **no
  `otelhttp` dependency**, matching the sidecar.
- **Circuit breaker.** A small per-provider breaker: `threshold` consecutive
  failures trip it open; while open it short-circuits with `ErrCircuitOpen` for a
  `cooldown` without touching the provider; after the cooldown one half-open trial
  is admitted whose outcome closes or re-opens it. `ErrCircuitOpen` is exported and
  `errors.Is`-checkable so the sink can *snooze* the job (SPEC-05 §8, "provider
  quota exhausted → paused, not failed") rather than fail it.
- **Allowlist enforcement.** SPEC-09 §2: `settings.providers_allowed` gates which
  providers a tenant's data may reach. `embed.New` takes the allowed set and **fails
  closed** — `ErrProviderNotAllowed` when the provider is absent, including an empty
  allowlist — *before* looking at the registry, so a disallowed provider is never
  even constructed and no tenant content can leave for it.
- **Token counting for batch sizing.** The ≤100k-token split needs a counter.
  Rather than couple to the chunker or add tiktoken, `Config.CountTokens` is
  injectable and defaults to a chars/4 approximation; the sink can pass the
  chunker's counter. This only affects how texts are *grouped* into requests, never
  correctness.

## Decision
Add `internal/ingest/embed`: `Result{Vectors, Tokens}`, the `Embedder` interface
(`Embed(ctx, []string) (Result, error)`), `Config`, and `New(cfg) (Embedder,
error)` that checks the allowlist (fail-closed), resolves the provider from a
`registry`, and returns a `batcher` wrapping the provider with batching, bounded
concurrency, a breaker and per-request retries. Providers `openAICompatible`
(OpenAI + Voyage), `cohere` and `tei` are plain `net/http`/`encoding/json`
clients. Retry/backoff/`Retry-After`/tracing mirror `internal/ingest/sidecar`.
Exported errors `ErrProviderNotAllowed`, `ErrUnknownProvider`, `ErrCircuitOpen`.
SPEC-05 §4 is updated to the `Result` return and the wrapper behaviour.

## Consequences
- The sink (STORY-05.6) constructs one `Embedder` per tenant from
  `settings.providers_allowed` + the embedding provider/model, calls `Embed` with
  all chunk `EmbedText`s, writes `Result.Vectors`/model onto `ChunkInput`, and folds
  `Result.Tokens` into `usage.Delta.EmbedTokens` (ADR-0024) and
  `jobs.stats.embed_tokens` (SPEC-05 §6). Order alignment means `Vectors[i]` is the
  embedding of `texts[i]` regardless of batch boundaries or a provider returning
  `data` out of order.
- Adding a provider is one file + one `registry` line, no change outside the package
  (NFR-MNT-02). The four are dependency-free; the only module change is promoting the
  already-present `golang.org/x/sync` (errgroup) and `go.opentelemetry.io/otel/trace`
  from indirect to direct.
- **Ceiling (ponytail):** (1) the default token counter is a chars/4 approximation —
  batch *grouping* only, inject a real cl100k_base counter to tighten the ≤100k
  budget; (2) under concurrency the half-open breaker may admit several trials before
  the first records (bounded by the batch concurrency, default 4); (3) the Cohere
  client reads the default `embeddings:[[...]]` array — upgrade to `embeddings.float`
  if a tenant pins `embedding_types`. None affect correctness.
- No schema/migration/OpenAPI change: `jobs.stats` is an existing JSON blob,
  `usage_daily` already exists (STORY-03.7), and this package writes neither — it
  surfaces the count for the sink.
- Tests: hermetic `httptest` per provider (request shape/auth/path, out-of-order
  reassembly, token surfacing), batching bounds (texts + token budget, order across
  batches), bounded concurrency, retry-then-succeed, `Retry-After` cut short by
  context, terminal-error-not-retried, breaker opens + short-circuits, allowlist
  fail-closed, unknown provider, empty input. No real network. Coverage: the package
  sits under `internal/ingest`, which reports SKIP against the gate (the gate matches
  `internal/ingest` exactly and it has no direct Go files) — the subpackage is not
  gated.
