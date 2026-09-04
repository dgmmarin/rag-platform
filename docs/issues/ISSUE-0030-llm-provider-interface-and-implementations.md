# ISSUE-0030: LLM provider interface and implementations

**Type:** Feature · **Status:** Done · **Story:** STORY-08.4 · **Traces:** NFR-MNT-02, NFR-REL-04, FR-RET-04, FR-RET-06, SPEC-06 §1/§5–6, SPEC-09 §2, ADR-0053

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-08.4 for traceability; the backlog
> story remains the authoritative work item.
>
> Build order: STORY-08.4 was implemented **before** STORY-08.3 (the LLM-based
> reranker depends on this provider seam) — a deliberate, user-approved reorder.

## Summary
The LLM generation seam for the answering stage (SPEC-06 §1, §5–6): a
provider-neutral `Provider` interface with non-streaming `Complete` and streaming
`Stream`, and three implementations — Anthropic (official anthropic-sdk-go), OpenAI
and OpenAI-compatible (vLLM/Ollama, raw HTTP `/v1/chat/completions`) — wrapped with
bounded-backoff retries and a per-provider circuit breaker (NFR-REL-04), surfacing
provider-normalised token usage, and gated fail-closed by a two-level allowlist
(provider + model) per SPEC-09 §2 / NFR-MNT-02.

## Scope
- New `internal/llm`, pure client code (no DB): the seam STORY-08.3 (reranker),
  STORY-08.5 (prompt assembly) and STORY-08.6 (query endpoint SSE) consume.
- Not in scope (later stories): prompt assembly / citations / grounding refusal
  (08.5), the query endpoint + client-facing SSE (08.6), the reranker (08.3),
  conversation history (08.7), query logging (08.8). This issue provides the
  interface those depend on and surfaces the token counts 08.5 writes.

## Resolution
- `llm.go`: `Provider` (`Complete(ctx, Request) (Response, error)`, `Stream(ctx,
  Request) (Stream, error)`), the `Request`/`Response`/`Usage`/`Event`/`Stream`/
  `Message` types, `Config`, `New(cfg)` (fail-closed provider + model allowlist →
  `registry` → generic retry+breaker wrapper `resilient`), `normalizeFinish`, and a
  `Factory`+`Keys` seam selecting the per-provider platform key. Errors
  `ErrProviderNotAllowed`, `ErrUnknownProvider`, `ErrCircuitOpen`,
  `ErrModelNotAllowed`.
- `provider.go`: the `openAI` raw `net/http`+`encoding/json` client serving both
  `openai` and `openai-compatible` (base-URL override); `complete` and `openStream`
  (SSE `data:` parsing, `stream_options.include_usage` for streamed usage).
- `anthropic.go`: the `anthropicProvider` over anthropic-sdk-go **v1.9.0**
  (`Messages.New` / `Messages.NewStreaming`), SDK retry disabled
  (`WithMaxRetries(0)`) so the wrapper is the single retry authority; SSE events
  mapped to text deltas + terminal usage/finish; SDK error classified by
  `StatusCode` exposing only the sanitised status.
- `breaker.go` / `retry.go`: circuit breaker + `transientError`/`backoff`/
  `retryAfter`/`sleep` **reused from `internal/ingest/embed` (ADR-0037), copied not
  shared** (ponytail: extract `internal/resilience` on a third consumer).
- Sampling sent only to OpenAI (current Claude models reject it); thinking not
  hardcoded (`Request.Effort` → OpenAI `reasoning_effort`, no-op on Anthropic
  v1.9.0). Token usage provider-normalised on every response/terminal event.
- Config/settings: `internal/config` + `.env.example` gain `ANTHROPIC_API_KEY`,
  `OPENAI_API_KEY`, `OPENAI_BASE_URL` (C-4, never logged); `settings_defaults.json`
  default answer model → `claude-sonnet-5` and a new `llm.models_allowed`
  (`claude-opus-5`, `claude-sonnet-5`, `claude-haiku-4-5`, `gpt-*`);
  `settings_schema.json` gains `models_allowed`. ADR-0053 records the design;
  SPEC-06 updated with the provider-contract section.

## Verification
- TDD: `internal/llm/llm_test.go` written first and watched fail (undefined
  symbols) before any implementation; the model-allowlist behaviour was added
  test-first in a second red→green cycle. `go test -race ./internal/llm` green.
- Hermetic `httptest` (no real network/keys; the Anthropic SDK pointed at the test
  server via `option.WithBaseURL`): per provider (openai / openai-compatible /
  anthropic) — non-streaming text+usage+finish, streaming deltas+usage+finish,
  request shape (system flattened to a leading message), retry-then-succeed on
  5xx→429, terminal 400 not retried + error sanitisation (no API key, no prompt
  content), plus provider allowlist fail-closed (incl. empty), model allowlist
  (exact / `gpt-*` wildcard / per-request override / empty = no restriction),
  unknown provider, breaker opens + short-circuits, factory key selection.
- `mise run lint` (pinned golangci-lint v2.13.1) **0 issues** on `internal/llm` and
  `internal/config`; `go vet` clean; `gofmt` clean. `mise run test` otherwise green
  apart from the pre-existing env-sensitive `internal/cli` `*RequiresURL` failures
  under mise `.env` injection (ISSUE-0003/0006 noise; `internal/cli` passes with a
  clean env). `internal/cp/tenants` settings drift/validation stays green.

## Notes / not in scope
- Dependency: `github.com/anthropics/anthropic-sdk-go v1.9.0` — the newest release
  whose `go` directive (1.21) is compatible with the repo's `go 1.22`; **all
  releases ≥ v1.9.1 require go ≥ 1.23 (≥ v1.47.0 require go ≥ 1.24)**, which would
  force a toolchain bump the story refuses. `go mod tidy` stayed on `go 1.22` (no
  `toolchain` line). No OpenAI SDK (raw HTTP, C-2/ADR-0002).
- No e2e in this story (precedent ADR-0037): `internal/llm` is a pure client
  library with no HTTP/worker path; its golden-path e2e arrives with the query
  endpoint (STORY-08.6), as embed's did with the sink (STORY-05.6).
- No schema/migration/OpenAPI change (settings jsonb; no endpoint yet).
- Ceilings (ponytail, ADR-0053): Anthropic stream establishment errors surface on
  first `Recv` (SDK lazy errors), not retried; `Effort` a no-op on Anthropic
  v1.9.0; breaker/retry copied from embed; one platform key per provider per
  deployment (C-5).
