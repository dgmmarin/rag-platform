# ADR-0057: Conversation history and question rewrite — the pre-retrieval follow-up→standalone rewrite in `internal/query`, per-tenant toggle (default off), strict single-turn passthrough, and retrieval-uses-standalone / answer-uses-original

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-RET-07, SPEC-06 §1/§5/§5.3, SPEC-09 §2, NFR-REL-04, C-3, C-4 · **Decisions:** ADR-0053, ADR-0055, ADR-0056

## Context
FR-RET-07: the query API accepts conversation history to support multi-turn
questions. STORY-08.6 (ADR-0056) already accepts `history[]` on `POST /v1/query` and
passes it to the answering stage **verbatim**; retrieval runs on the raw question. But
a follow-up like "what about the X300?" carries no retrievable terms on its own — the
hybrid search (SPEC-06 §2) embeds and full-text-matches a context-dependent fragment,
so the relevant chunks are never found. SPEC-06 §1/§5 always anticipated an **optional
rewrite step** that turns a follow-up into a standalone question **before** retrieval.
This story inserts it.

The AC: follow-up questions resolved into a standalone query before retrieval; a
per-tenant toggle; **eval shows no regression on single-turn**. The scope is only the
rewrite step + toggle + wiring — not the retrieval/rerank/answer core, not the query
log (08.8), not a full eval harness (EPIC-12).

## Options / decisions
- **The rewrite lives in `internal/query`, before retrieval — not in `internal/answer`.**
  `internal/answer` is deliberately the *answering* stage and documents that it does
  NOT rewrite (ADR-0055); the rewrite must run before `retrieve.Service.Search`, which
  the query composition root (ADR-0056) owns. So the step is a method on
  `query.Service` (`standaloneQuestion`, `internal/query/rewrite.go`) invoked inside
  `build`, which is reordered to load settings → rewrite → retrieve (settings are
  needed for the toggle and the provider; both settings and retrieval still fail before
  any SSE frame, so ADR-0056's pre-stream-error contract holds).
- **One cheap `llm.Complete` call, reusing the tenant's provider (§5.1).** The rewrite
  reuses the same `answer.ProviderFactory` the answer path uses (wired as one shared
  `KeyedProviderFactory` in `internal/cli`), so the provider + model allowlists and the
  retry/breaker resilience are enforced in one place (ADR-0053). A fixed system prompt
  owns the task (resolve pronouns/ellipsis into a self-contained question, preserve
  language/intent, output only the question); the conversation + follow-up are a single
  user message rendered as **delimited data, never instructions** (prompt-injection
  defence, SPEC-09 §2). The last N turns (`settings.answering.history_n`, default 6)
  bound the prompt, matching the answer stage's own window.
- **Optional model override `settings.rewrite.model`.** A tenant may point the rewrite
  at a cheaper model than the answer model; absent, it reuses `settings.llm.model`. The
  override is still checked against `settings.llm.models_allowed` fail-closed inside the
  factory — a rejection is just another rewrite failure and falls back to the original
  question. (Mirrors `settings.reranker.llm_model`, ADR-0054.)
- **Per-tenant toggle `settings.rewrite.enabled`, default OFF.** Opt-in and
  conservative, exactly like `settings.reranker.enabled` (ADR-0054): the platform does
  not silently add an LLM call and change retrieval behaviour for existing tenants. A
  new `rewrite` object is added to `settings_defaults.json` and `settings_schema.json`
  (the merged-defaults drift guard in `tenants` keeps the two in sync).
- **Strict single-turn passthrough is the AC's no-regression proof.** When the toggle
  is off, OR there is no history (single-turn), OR no provider factory is wired, the
  original question is returned unchanged and **zero** rewrite LLM calls are made — the
  pipeline is byte-identical to pre-08.7. A full eval harness (recall@k / grounded-rate
  over `eval_cases`) is EPIC-12 / SPEC-06 §8; here the AC's "eval shows no regression on
  single-turn" is satisfied by this zero-call passthrough **guarantee**, proven directly
  by tests (single-turn and toggle-off assert the stub provider is untouched and
  retrieval sees the original question). This interpretation is recorded deliberately so
  08.7 is not blocked on EPIC-12.
- **Retrieval uses the standalone question; the answer uses the original + history.**
  RETRIEVAL (embed + full-text) runs on the standalone question so the right chunks are
  found; the `answer.Request.Question` stays the **original** follow-up and the history
  is still passed verbatim (SPEC-06 §5), so the model answers the user's actual turn in
  its conversational context. The two are intentionally different inputs.
- **The rewrite never fails the query (NFR-REL-04).** Any provider-build error,
  `Complete` error (incl. `ErrCircuitOpen`), allowlist rejection, or empty/garbled
  output logs a warning (no prompt content, C-4) and falls back to the original
  question. Output is parsed defensively (`parseRewritten`: unwrap ``` fences and
  wrapping quotes, trim) — reusing the tolerant-parse philosophy of the LLM reranker
  (ADR-0054) without needing its JSON machinery.
- **No transport/schema change.** `history[]` is already in the request (ADR-0056) and
  the response shape is unchanged, so no OpenAPI change and no migration. Retrieval
  latency (`usage.retrieval_ms`) still measures only `Search`, so the reported number is
  unchanged on the passthrough path.

## Decision
Add `internal/query/rewrite.go`: `rewriteSettings` + `parseRewriteSettings`,
`Service.standaloneQuestion` (the toggle/history/passthrough gate + the one
`Complete` call + defensive parse + fallback), the delimited-data prompt builder, and
`parseRewritten`. Add a `Providers answer.ProviderFactory` field to `query.Service`
(nil = passthrough) and reorder `build` to settings → rewrite → retrieve, feeding
retrieval the standalone question while the answer keeps the original question +
history. Add the `rewrite` object to `settings_defaults.json` / `settings_schema.json`.
Wire the shared provider factory into both the answer stage and the query service in
`internal/cli`. Document it in SPEC-06 §5.3 (header `Decisions:` gains ADR-0057).

## Consequences
- **Multi-turn retrieval works when a tenant opts in**, with a single extra cheap LLM
  call before retrieval, and no behaviour change for anyone who does not.
- **No new dependency, no schema migration, no API change.** Reuses `internal/llm`,
  `internal/answer`'s factory + `Turn`/`Settings`, and `internal/cp/tenants` settings.
- **Ceilings (ponytail):** (1) the rewrite's latency is not added to
  `usage.retrieval_ms` (only `Search` is measured), so an enabled tenant's reported
  retrieval time excludes the rewrite round trip — acceptable for now; a dedicated
  `rewrite_ms` is the upgrade path. (2) `parseRewritten` is a light unwrapper, not a
  parser — it tolerates the common fence/quote noise, not arbitrary prose; a chatty
  model that ignores "output only the question" degrades gracefully to a slightly noisy
  retrieval query, never a failure. (3) The AC's "eval shows no regression" is met by
  the single-turn zero-call passthrough proof, not a numeric eval — the harness is
  EPIC-12.
- **Tests (hermetic, no keys/network/DB):** multi-turn + enabled → exactly one rewrite
  call, retrieval sees the standalone, the answer prompt carries the original question,
  the rewrite prompt carries the history; single-turn + enabled and multi-turn +
  toggle-off → **zero** rewrite calls and the original question retrieved; nil factory →
  passthrough; rewrite error and empty output → fall back to the original, query still
  grounded; `settings.rewrite.model` threaded through; `parseRewritten` unwraps
  fences/quotes. The DB-backed query e2e is extended: with the toggle off the earlier
  single-turn queries make zero rewrite calls, then enabling the toggle and sending a
  multi-turn query makes exactly one rewrite call fed the history and still returns a
  grounded answer (the original context-dependent follow-up would not ground).
