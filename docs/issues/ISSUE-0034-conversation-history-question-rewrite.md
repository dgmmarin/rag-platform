# ISSUE-0034: Conversation history and question rewrite

**Type:** Feature · **Status:** Done · **Story:** STORY-08.7 · **Traces:** FR-RET-07, SPEC-06 §1/§5/§5.3, SPEC-09 §2, NFR-REL-04, C-4, ADR-0057

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-08.7 for traceability; the backlog
> story remains the authoritative work item.

## Summary
A follow-up question ("what about the X300?") carries no retrievable terms on its own.
This story inserts an optional **rewrite step** in `internal/query` that turns a
follow-up into a standalone question BEFORE retrieval (FR-RET-07, SPEC-06 §1/§5),
gated by a per-tenant toggle. Retrieval runs on the standalone question; the answer
stage keeps the original question + history verbatim (SPEC-06 §5).

## Scope
- New `internal/query/rewrite.go`: `rewriteSettings` + `parseRewriteSettings`,
  `Service.standaloneQuestion` (toggle/history/passthrough gate + one `llm.Complete`
  call + defensive parse + fallback), the delimited-data prompt builder, `parseRewritten`.
- `internal/query/query.go`: a `Providers answer.ProviderFactory` field (nil =
  passthrough); `build` reordered settings → rewrite → retrieve, feeding retrieval the
  standalone question and the answer the original question + history.
- `internal/cp/tenants`: a `rewrite` object added to `settings_defaults.json` and
  `settings_schema.json` (`enabled` default false; optional `model`).
- `internal/cli/api_server.go`: the shared provider factory wired into both the answer
  stage and the query service's `Providers`.
- Not in scope: the retrieval/rerank/answer core (08.1/08.3/08.5, untouched); the async
  query log + feedback (08.8); a numeric eval harness (EPIC-12 / SPEC-06 §8).

## Resolution
- **Toggle:** `settings.rewrite.enabled` (default OFF, opt-in — mirrors
  `settings.reranker.enabled`). `settings.rewrite.model` optionally overrides the model
  for the rewrite call only, still gated by `settings.llm.models_allowed` fail-closed
  inside the shared `internal/llm` factory.
- **When it runs:** only when the toggle is on AND `history[]` is present. Toggle off,
  single-turn (no history), or no factory ⇒ the original question is used unchanged and
  **no rewrite LLM call is made** (strict passthrough).
- **The call:** one `Provider.Complete` (last `settings.answering.history_n` turns,
  default 6) with a fixed system prompt resolving pronouns/ellipsis; the conversation +
  follow-up are a single user message rendered as delimited **data**, never instructions
  (SPEC-09 §2). Output parsed defensively (unwrap fences/quotes); on empty/garbled
  output or any provider error (incl. `ErrCircuitOpen`) it falls back to the ORIGINAL
  question — the rewrite never fails the query (NFR-REL-04). Errors carry no prompt
  content (C-4).
- **Retrieval-uses-standalone / answer-uses-original:** retrieval (embed + full-text)
  runs on the standalone question; `answer.Request.Question` stays the original follow-up
  and history is passed verbatim (SPEC-06 §5).
- **Single-turn no-regression (the AC):** interpreted as the strict single-turn
  zero-call passthrough guarantee (byte-identical to pre-08.7), proven by tests, rather
  than a numeric eval — the harness is EPIC-12 (recorded deliberately in ADR-0057).

## Verification
- `internal/query` unit tests (hermetic, no keys/network/DB): multi-turn + enabled →
  exactly one rewrite call, retrieval sees the standalone, the answer prompt carries the
  original question, the rewrite prompt carries the history; single-turn + enabled and
  multi-turn + toggle-off → **zero** rewrite calls, original question retrieved; nil
  factory → passthrough; rewrite error and empty output → fall back to the original,
  query still grounded; `settings.rewrite.model` threaded through; `parseRewritten`
  unwraps fences/quotes.
- `internal/cp/tenants` drift guard green (defaults validate against the schema with the
  new `rewrite` object).
- DB-backed e2e `test/e2e/query_endpoint_e2e_test.go` extended: with the toggle off the
  single-turn queries make zero rewrite calls (passthrough end-to-end), then enabling
  `settings.rewrite.enabled` and sending a multi-turn query makes exactly one rewrite
  call fed the conversation history and still returns a grounded answer (the original
  context-dependent follow-up would not ground). PASS on Postgres :5432.
- `mise run test` — all pass except the pre-existing `internal/cli` `.env`-injection
  caveat (green with a clean env). golangci-lint (v2) on `internal/query`: 0 issues.
- No migration, no OpenAPI change (`history[]` already in the request), no new dependency.

## Notes / ceilings (ponytail)
- The rewrite round trip is not added to `usage.retrieval_ms` (only `Search` is
  measured); a dedicated `rewrite_ms` is the upgrade path.
- `parseRewritten` is a light unwrapper (fences/quotes/trim), not a full parser; a
  chatty model degrades to a slightly noisy retrieval query, never a failure.
- "Eval shows no regression on single-turn" is met by the zero-call passthrough proof;
  the numeric eval harness is EPIC-12 / SPEC-06 §8.
