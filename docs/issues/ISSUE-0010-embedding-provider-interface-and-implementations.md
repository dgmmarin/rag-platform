# ISSUE-0010: Embedding provider interface and implementations

**Type:** Feature · **Status:** Done · **Story:** STORY-05.5 · **Traces:** FR-ING-05, NFR-MNT-02, SPEC-05 §4, ADR-0037

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-05.5 for traceability; the backlog
> story remains the authoritative work item.

## Summary
The embedding stage of the ingestion pipeline (SPEC-05 §4): an `Embedder` seam and
four provider implementations (OpenAI, Voyage, Cohere, TEI), with batching (≤96
texts / ≤100k tokens), bounded per-tenant concurrency (default 4 in flight),
retry/backoff honouring `Retry-After` on 429/5xx, a per-provider circuit breaker,
surfaced token usage, and a fail-closed `settings.providers_allowed` allowlist.

## Scope
- New `internal/ingest/embed`, pure client code (no DB): consumes
  `chunk.Chunk.EmbedText` (STORY-05.4) and produces vectors + token usage the sink
  (STORY-05.6) writes onto `documents.ChunkInput` and into
  `usage.Delta`/`jobs.stats`.
- Not in scope: the sink orchestration and the actual `jobs.stats`/`usage_daily`
  writes (STORY-05.6) — this issue surfaces the token count and marks the seam.

## Resolution
- `embed.go`: `Result{Vectors [][]float32, Tokens int}`, `Embedder` interface
  (`Embed(ctx, []string) (Result, error)` — widened from SPEC's `[][]float32` so
  token usage is surfaced, ADR-0037), `Config`, `New(cfg) (Embedder, error)`, and a
  `batcher` (itself an `Embedder`) that partitions input into provider-sized
  batches, runs them with `errgroup.SetLimit` concurrency, guards each with the
  breaker, and reassembles vectors in input order + sums tokens.
- `provider.go`: a `registry` map (single seam for a new provider, NFR-MNT-02) and
  four dependency-free `net/http`+`encoding/json` clients — `openAICompatible`
  (OpenAI + Voyage share the schema), `cohere`, `tei`. `ensureComplete` rejects a
  partial response so no nil embedding reaches the store.
- `retry.go`: `doer` with retry/backoff/`Retry-After`/OTel span + W3C propagation,
  mirroring `internal/ingest/sidecar` (`transientError`, `retryAfter`, `backoff`,
  `sleep`); 429/5xx transient, other non-2xx terminal.
- `breaker.go`: per-provider circuit breaker (closed/open/half-open) exposing
  `ErrCircuitOpen` so the sink can snooze rather than fail (SPEC-05 §8).
- Allowlist: `New` fails closed with `ErrProviderNotAllowed` (incl. empty allowlist)
  before touching the registry; `ErrUnknownProvider` for an unknown name.
- SPEC-05 §4 updated to the `Result` return and wrapper behaviour; ADR-0037 records
  the design.

## Verification
- TDD: `internal/ingest/embed/embed_test.go` written first and watched fail (no
  non-test files) before implementation. `go test ./internal/ingest/embed` green,
  `-race` clean: per-provider request shape/auth/path + out-of-order reassembly +
  token surfacing (OpenAI/Voyage/Cohere/TEI), batching bounds (max texts and token
  budget, order preserved across batches), bounded concurrency, retry-then-succeed,
  `Retry-After` cancellable by context, terminal-error-not-retried, breaker opens
  and short-circuits, allowlist rejection + empty-fails-closed, unknown provider,
  empty input makes no request.
- `gofmt -l` clean; `go vet` clean; pinned `golangci-lint v2.13.1` **0 issues** on
  the package. Full `go test ./...` otherwise green (the pre-existing env-sensitive
  `internal/cli` `*RequiresURL` failures under mise `.env` injection are the
  documented ISSUE-0003/0006 noise, unrelated).

## Notes / not in scope
- Ceiling (ponytail, ADR-0037): default token counter is chars/4 (grouping only —
  inject a real cl100k_base counter to tighten); half-open breaker may admit a few
  concurrent trials (bounded by batch concurrency); Cohere reads the default bare
  `embeddings` array (upgrade to `embeddings.float` for `embedding_types`).
- No schema/migration/OpenAPI change: `jobs.stats` is an existing JSON blob and
  `usage_daily` already exists (STORY-03.7); this package writes neither.
- Coverage: `internal/ingest` reports SKIP against the gate (matches the path
  exactly, has no direct Go files) so the subpackage is not gated.
- Module change limited to promoting the already-present `golang.org/x/sync`
  (errgroup) and `go.opentelemetry.io/otel/trace` to direct requires (`golang.org/x/net`
  correction is pre-existing drift — `internal/ingest/parse` uses `x/net/html`).
