# ISSUE-0033: Query endpoint with streaming

**Type:** Feature · **Status:** Done · **Story:** STORY-08.6 · **Traces:** FR-RET-06, SPEC-06 §6/§6.1, SPEC-07 §2f, NFR-REL-04, FR-ACC-03, ADR-0024, ADR-0056

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-08.6 for traceability; the backlog
> story remains the authoritative work item.

## Summary
The grounded answering endpoint (SPEC-06 §6, FR-RET-06): `POST /v1/query` wires the
existing retrieval (STORY-08.1/08.3) and answering (STORY-08.5) pipeline behind one
`query`-scope route in two response modes — non-streaming JSON and streaming SSE —
with citations emitted before text and usage in the `done` event.

## Scope
- New `internal/query`: `Service` (`Query` JSON, `QueryStream` SSE) + `Handlers`
  (`POST /v1/query`, `stream` flag dispatch, flushing `httpSink`).
- `internal/answer`: additive, behaviour-preserving extract of the shared front half
  (`Prepare`/`Prepared`/`candidateCitations`) so JSON and SSE assemble the same prompt
  and citation numbering, plus `RecordStreamed` for the SSE usage/log accounting.
- `internal/cp/tenants`: `NameService` to read the tenant display name for the refusal
  message (control-plane registry data, C-3).
- Router + code-derived OpenAPI (`internal/api`) gain the route; `api/openapi.yaml`
  regenerated. Wired in `internal/cli` (`ragctl serve`).
- Not in scope (later stories): the follow-up→standalone question rewrite (08.7 —
  history is passed through verbatim); the async query-log + feedback persistence
  (08.8 — the `QueryLogger` seam is left nil/no-op).
- Not touched: the retrieval/rerank/answer core logic (08.1/08.3/08.5).

## Resolution
- `internal/query/query.go`, `handlers.go`:
  - **JSON (`stream:false`):** `Service.Query` → `answer.Service.Answer` → SPEC-06 §6
    body `{id, answer, grounded, citations[], usage, model}`.
  - **SSE (`stream:true`):** `Service.QueryStream` shares `answer.Service.Prepare`,
    then drives `llm.Provider.Stream`; emits `retrieval` (candidate citations FIRST) →
    `delta` (text) → `done` (usage). Citations-before-text is resolved by approach (a)
    (candidate citations up front; the client maps `[n]`); JSON keeps the post-hoc
    unreferenced-drop; the numbering is identical across modes (ADR-0056).
  - **Grounding refusal (§4)** in both modes: below-floor → `grounded=false`, fixed
    message, zero citations, NO LLM/stream call.
  - **Degradation (NFR-REL-04):** `llm.ErrCircuitOpen` → retrieval-only (JSON 200 with
    candidate citations + a "generation unavailable" message; SSE
    `done{generation_unavailable:true}`) instead of a hard 500.
  - **Accounting:** `Queries` counter incremented once here (08.5 left it to avoid a
    double count); LLM tokens folded by `answer` (JSON inline, SSE via
    `RecordStreamed`). Tenant name via `tenants.NameService` (non-fatal fallback).
- `internal/answer/answer.go`: `Prepare`/`Prepared`/`candidateCitations`/
  `RecordStreamed`; `Answer` refactored to call `Prepare` (existing 08.5 suite green).
- `internal/cp/tenants/name.go`: `NameService.Name` over the control-plane pool.
- `internal/api/router.go` + `openapi.go`: `POST /v1/query` mounted under
  `RequireScopeQuery` + rate limit; documented (both modes noted in the summary).
- `internal/cli/api_server.go`: query service wired with the shared `llm.Factory`
  and the `usage.Counter`.
- `api/openapi.yaml`: regenerated (`mise run openapi`); drift + contract guards green.
- Docs: SPEC-06 §6.1, SPEC-07 §2f, ADR-0056.

## Verification
- `internal/query` unit tests (fakes): JSON §6 shape + referenced citation + usage;
  SSE retrieval→delta→done IN ORDER with citations before text and usage in `done`
  (SSE parsed in the test); refusal in both modes with zero provider calls;
  degradation on `ErrCircuitOpen`; `Queries` incremented exactly once; 401/400/503
  mapping; unknown-field rejection. `internal/answer` `Prepare`/`RecordStreamed` tests.
  Coverage: `internal/query` 81%, `internal/answer` 90%.
- DB-backed e2e `test/e2e/query_endpoint_e2e_test.go` over the REAL router + resolver +
  hybrid SQL (stubbed embedder + stubbed LLM, no real keys/network): JSON grounded
  answer + `[1]` citation + usage; full SSE order with citations-before-text; a
  below-floor refusal carrying the tenant name and no generation. PASS on Postgres :5432.
- `mise run test` — all pass except the pre-existing `internal/cli` `.env`-injection
  caveat (green with a clean env). golangci-lint (v2) on the touched packages: 0 issues.
- `mise run openapi` regenerated `api/openapi.yaml`; drift + OpenAPI contract e2e green.
- No migration, no new dependency.

## Notes / ceilings (ponytail)
- A tenant-name lookup failure falls back to a generic name (wording only, non-fatal).
- SSE refusal/degradation report retrieval-only usage (no `generation_ms`).
- The async query log + feedback endpoint is STORY-08.8 (the `QueryLogger` seam is nil).
- The OpenAPI document describes the SSE stream in prose, not a modelled
  `text/event-stream` schema — consistent with ADR-0028's minimal generator.
