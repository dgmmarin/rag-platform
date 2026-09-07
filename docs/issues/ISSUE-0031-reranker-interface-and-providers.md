# ISSUE-0031: Reranker interface and providers

**Type:** Feature · **Status:** Done · **Story:** STORY-08.3 · **Traces:** FR-RET-03, NFR-MNT-02, NFR-REL-04, SPEC-06 §3, SPEC-09 §2, ADR-0054

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-08.3 for traceability; the backlog
> story remains the authoritative work item.
>
> Build order: STORY-08.3 was implemented **after** STORY-08.4 (the LLM-based
> reranker consumes the `internal/llm` provider seam) — a deliberate, user-approved
> reorder.

## Summary
The reranking stage between RRF fusion and the final top-`k` (SPEC-06 §3,
FR-RET-03): a provider-neutral `Reranker` interface with two implementations — a
Cohere reranker (real HTTP, Cohere v2 `/v2/rerank`) and an LLM-based reranker that
scores ALL candidates in ONE batched `llm.Complete` listwise call — a per-tenant
toggle (`settings.reranker.enabled`), and a fallback to fused order on any reranker
failure (NFR-REL-04). Wired into `internal/retrieve.Service`.

## Scope
- New `internal/rerank`: the seam STORY-08.5 (grounding over the reranker score)
  builds on. Consumes `internal/llm` (08.4) for the LLM reranker.
- Wiring into `internal/retrieve.Service.Search` (over-fetch top_n → rerank →
  reorder → truncate final_k).
- Not in scope (later stories): `min_score` grounding refusal / prompt assembly /
  citations (08.5), the query endpoint + SSE (08.6), history (08.7), query
  logging (08.8).

## Resolution
- `internal/rerank/rerank.go`: `Reranker` (`Rerank(ctx, query, docs []Doc)
  ([]Scored, error)`, `Doc{ID,Text}` / `Scored{ID,Score}`), `Config`, `Completer`
  (the `llm.Provider` subset), `New(cfg)` (returns nil reranker when disabled;
  fail-closed on provider allowlist + missing key/provider), `orderMissingLast`.
  Errors `ErrProviderNotAllowed`, `ErrUnknownProvider`, `ErrMissingKey`,
  `ErrCircuitOpen`, `ErrUnparseable`.
- `cohere.go`: Cohere v2 rerank client (`POST {base}/v2/rerank` `{model, query,
  documents}`, Bearer `COHERE_API_KEY`, `top_n` not sent so every candidate is
  scored; index→id mapping, re-sort by score, omitted docs last).
- `llm.go`: the LLM reranker — ONE `llm.Complete` call with a listwise system
  prompt + numbered passages, defensive JSON-array extraction (bracket-depth,
  fence/prose tolerant) → `[{id,score}]`, out-of-range/duplicate id drops,
  `ErrUnparseable` on no valid entries, order-preserving fallback score.
- `breaker.go` / `retry.go`: circuit breaker + `doer`/`transientError`/`backoff`/
  `retryAfter`/`sleep` copied from `internal/ingest/embed` (ADR-0037) — the THIRD
  copy (see "Notes").
- `internal/retrieve`: `RerankerFactory` seam + `Service.Reranker` field; `Settings`
  gains reranker + llm fields (`parseSettings`); `Search` over-fetches
  `max(top_n, final_k)`, reranks top_n, reorders by reranker score, truncates to
  final_k; production `KeyedRerankerFactory` (Cohere key + `llm.Factory`).
- Config/settings: `internal/config` + `.env.example` gain `COHERE_API_KEY`,
  `COHERE_BASE_URL` (C-4, never logged); `settings_schema.json` gains the optional
  `reranker.llm_model` override (defaults unchanged). `internal/cli/api_server.go`
  wires the factory. ADR-0054 records the design; SPEC-06 §3 documents the seam;
  the code-derived OpenAPI summary notes `score` becomes the reranker score.

## Verification
- TDD: `internal/rerank/rerank_test.go` written first and watched fail (undefined
  symbols) before any implementation; the retrieve-service wiring, the config key,
  and the `reranker.llm_model` schema each added test-first (red → green).
- Hermetic (no real network/keys): Cohere against an `httptest` fixture — reorder by
  relevance, 429-retry-then-succeed, terminal-400 not retried + API key not leaked,
  missing-key/allowlist/unknown-provider fail-closed, disabled → nil. LLM reranker
  against a fake `Completer` — reorders in exactly ONE call, tolerates ```json```
  fences, unparseable → `ErrUnparseable`, provider error propagates, omitted doc
  survives last, empty docs no-op. Retrieve service — reranks when enabled, fallback
  to fused on reranker error and on factory error, no-rerank/no-over-fetch when
  disabled, over-fetch-top_n-then-truncate, settings parsing, `KeyedRerankerFactory`
  (nil/cohere/allowlist/llm/disallowed-override paths).
- `go test ./internal/rerank ./internal/retrieve ./internal/config ./internal/cp/tenants`
  green; `go build ./...` clean; `go vet` / lint clean on the new files.

## Notes / not in scope
- No new dependency: Cohere is raw `net/http` (C-2/ADR-0002); the LLM reranker
  reuses `internal/llm`. `go 1.22` unchanged.
- No migration; no OpenAPI response-schema change (the response already carries
  `score`; only the summary/description gained a note). Settings schema gained one
  optional property (drift/validation green).
- **`internal/resilience` extraction deferred (overdue ponytail).** ADR-0053 named a
  third resilience consumer as the trigger to extract a shared package; ADR-0054
  records the deliberate decision to **copy** the breaker/retry a third time rather
  than edit two stable, coverage-gated packages (`internal/ingest/embed`,
  `internal/llm`) while the local Docker/Postgres stack is wedged (can't fully
  re-verify a cross-package refactor). **Follow-up:** a dedicated refactor story
  should extract `internal/resilience` and migrate all three copies at once under
  green embed/llm/rerank suites.
- Ceilings (ponytail, ADR-0054): one Cohere key per deployment (C-5); the LLM
  reranker one-lines each passage and relies on the output-token budget heuristic —
  a token-aware truncation of very long passages is a future refinement.
- The golden-path e2e through the real `/v1/retrieve` + Postgres rides the existing
  retrieve HTTP path (reranking is off by default; enabling it per tenant reorders
  the same endpoint). The local stack being wedged, reranker tests are kept fully
  hermetic (fixtures + fake provider) per the story's environment guidance.
