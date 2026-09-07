# ADR-0054: Reranker interface and providers — provider-neutral `Reranker` seam, Cohere v2 + single-batched-call LLM reranker, per-tenant toggle, fail-open fallback to fused order

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-RET-03, NFR-MNT-02, NFR-REL-04, SPEC-06 §3, SPEC-09 §2, C-2, C-4, C-5 · **Decisions:** ADR-0007, ADR-0037, ADR-0051, ADR-0052, ADR-0053

## Context
SPEC-06 §1/§3 puts an optional reranking stage between RRF fusion and the final
top-`k`: when `settings.reranker.enabled`, the top `top_n` fused results are
re-scored by a reranker and re-ordered by that score. STORY-08.3 builds the seam
(FR-RET-03): a Cohere reranker AND an LLM-based reranker, a per-tenant toggle, and a
fallback to fused order on provider failure. It is built **after** STORY-08.4
(ADR-0053, `internal/llm`) — a deliberate, user-approved reorder — because the LLM
reranker consumes the `llm.Complete` seam. Constraints: Go primary, SDK only where
justified (C-2/ADR-0002); secrets never logged (C-4); providers gated by
`settings.providers_allowed` (SPEC-09 §2); graceful degradation on provider loss
(NFR-REL-04); single-region per-tenant deployment (C-5).

## Options / decisions
- **Interface shape.** A provider-neutral `Reranker` with one method
  `Rerank(ctx, query string, docs []Doc) ([]Scored, error)` (`Doc{ID, Text}`,
  `Scored{ID, Score}`), modelled on SPEC-06 §3's `Rerank(query, texts)` but carrying
  the chunk **id** alongside the text so the caller maps scores back to chunks
  without positional bookkeeping. A `registry`-free `New(Config)` factory selects the
  provider from `settings.reranker.provider` and returns **`(nil, nil)` when
  disabled** — the caller treats a nil `Reranker` as "no rerank". Adding a third
  reranker is one file + one `switch` arm (NFR-MNT-02).
- **Cohere reranker (real HTTP, fixture-tested).** `POST {base}/v2/rerank` with
  `{model, query, documents}` (Cohere **v2** rerank API; model `rerank-v3.5`), Bearer
  `COHERE_API_KEY`. `top_n` is deliberately **not** sent — the reranker must return
  every candidate scored so the service can re-order the full `top_n` set (the
  service already trimmed the fused set to `settings.reranker.top_n`). The response
  `results[{index, relevance_score}]` maps `index → docs[index].ID`; results are
  re-sorted by score defensively and any omitted doc is appended last (never
  dropped). Plain `net/http`+`encoding/json`, no vendor SDK (C-2/ADR-0002),
  consistent with the Cohere **embedder** (ADR-0037).
- **LLM reranker — ONE batched call (the user's decision).** The LLM reranker scores
  **all** candidates in a **single** `llm.Complete` call: a **listwise** prompt
  numbers the `top_n` passages and the query and asks for a JSON `[{id, score}]`
  ranking, most-relevant first — **not** per-document calls. One extra LLM request
  per query, only when a tenant enables it. It reuses the tenant's `internal/llm`
  provider (built from `settings.llm`), so provider resilience (retry/breaker) and
  the provider + model allowlists live in `internal/llm`; this type adds only the
  prompt and a **defensive** parse (isolate the first top-level JSON array by bracket
  depth, tolerating ```code fences``` and prose; drop out-of-range/duplicate ids;
  require ≥1 valid entry, else `ErrUnparseable`). Crawled/ingested passage text is
  presented as **data**, not instructions (prompt-injection defence, SPEC-09 §2): a
  fixed system prompt owns the task and passages are delimited/one-lined. Output
  budget is bounded (`64 + 16·n`, capped 4096) so a large `top_n` can't request an
  unbounded completion; `temperature=0` where the provider honours it. If a model
  returns a correct order but degenerate scores, a position-derived strictly
  descending score preserves the order under the service's score-sort.
- **LLM reranker model choice.** Reuse `settings.llm.{provider,model}` by default;
  add an **optional** `settings.reranker.llm_model` override (schema-only, not in
  defaults) so a tenant can point reranking at a cheaper model without touching the
  answer model. Absent → `settings.llm.model` (default `claude-sonnet-5`). Kept
  cheap-configurable per the story's guidance.
- **Toggle.** `settings.reranker.enabled` (default false, already present) is the
  per-tenant switch; `provider` selects `cohere`|`llm`; `top_n` (default 20) is how
  many fused results to rerank.
- **Fallback (FR-RET-03 AC, NFR-REL-04).** A reranker **never** fails the query. Any
  error — network, breaker-open, missing key, unparseable LLM output, fail-closed
  allowlist at build time — is logged (sanitised, no query/content — C-4) and the
  service returns the **original fused order**. Proven by tests at both seams
  (factory build error and `Rerank` call error).
- **Fail-closed (SPEC-09 §2).** For Cohere, the provider `"cohere"` must be in
  `settings.providers_allowed` and `COHERE_API_KEY` must be set, else `New` returns
  `ErrProviderNotAllowed`/`ErrMissingKey`. The LLM reranker's underlying provider is
  gated where its `llm.Provider` is built (`llm.New`), so `rerank` trusts the passed
  `Completer`; a nil provider is `ErrMissingKey`.
- **Service wiring.** `internal/retrieve.Service` gains a `Reranker RerankerFactory`
  seam (nil = never rerank, the pre-08.3 default). `Search` builds the reranker per
  request from settings, **over-fetches** `max(top_n, final_k)` fused candidates,
  reranks the top `top_n`, reorders by reranker score (replacing `Result.Score`),
  then truncates to `final_k`. `min_score`/grounding refusal stays with STORY-08.5 —
  a clean seam. The production `KeyedRerankerFactory` builds the Cohere client from
  `COHERE_API_KEY` and the LLM reranker from an `llm.Factory` (`reranker.llm_model`
  else `settings.llm.model`).
- **Resilience — copy vs extract `internal/resilience` (the story's explicit
  choice).** ADR-0053 flagged a **third** resilience consumer (embed, llm, now
  rerank) as the trigger to extract a shared `internal/resilience`. **Decision:
  copy the pattern a third time (breaker + `doer`/retry) into `internal/rerank`, and
  re-flag the ponytail — do NOT extract in this story.** Rationale: (1) only the
  Cohere HTTP client needs it — the LLM reranker rides `internal/llm`'s own
  breaker/retry — so the copy is small and self-contained; (2) extracting would force
  edits to two **stable, coverage-gated, already-shipped** packages (`internal/ingest/
  embed`, `internal/llm`), each with a package-private `ErrCircuitOpen`/`transientError`
  the public API would have to alias, and the local Docker/Postgres stack is currently
  wedged, so a cross-package refactor could not be fully re-verified here — against
  the story's "don't let a refactor balloon the story"; (3) it keeps this change
  additive and trivially reversible (delete one package). The ponytail is sharpened:
  the extraction is now **overdue** and should be its own dedicated refactor story
  (tracked in ISSUE-0031), migrating all three copies at once under green
  embed/llm/rerank suites.

## Decision
Add `internal/rerank`: the `Reranker` interface, `Doc`/`Scored`/`Config`/`Completer`
types, `New(cfg)` factory (disabled → nil; fail-closed provider/key), the `cohere`
client (v2 `/v2/rerank`, breaker+retry), the `llmReranker` (single batched
`llm.Complete`, defensive listwise parse), and copied `breaker`/`retry` helpers.
Exported errors `ErrProviderNotAllowed`, `ErrUnknownProvider`, `ErrMissingKey`,
`ErrCircuitOpen`, `ErrUnparseable`. `internal/retrieve` gains the `RerankerFactory`
seam, `Settings` reranker/llm fields, `Search` over-fetch+rerank+truncate wiring, and
the production `KeyedRerankerFactory`. `internal/config` + `.env.example` gain
`COHERE_API_KEY`/`COHERE_BASE_URL` (C-4, never logged). `settings_schema.json` gains
the optional `reranker.llm_model` (defaults unchanged). `internal/cli/api_server.go`
wires the factory. SPEC-06 §3 documents the seam.

## Consequences
- **The seam 08.5/08.6 build on.** Reranking now reorders `/v1/retrieve` results and
  turns `score` into the reranker score when enabled; the grounding floor over that
  score is 08.5. No new response field (the response already carries `score`), so the
  OpenAPI change is a summary/description note only.
- **No new dependency.** Cohere is raw HTTP; the LLM reranker reuses `internal/llm`.
  No SDK added. `go 1.22` unchanged.
- **No migration/OpenAPI-schema change.** `settings` is jsonb; only the schema JSON
  gained an optional property (drift/validation stays green). The `/v1/retrieve`
  response schema is unchanged (description note only).
- **Fail-open on the reranker, fail-closed on the provider.** The query degrades to
  fused order on any reranker fault (NFR-REL-04) while the tenant's data still cannot
  reach a non-allowlisted provider (SPEC-09 §2).
- **Ceilings (ponytail):** (1) breaker/retry copied a third time — extract
  `internal/resilience` as a dedicated refactor (ISSUE-0031). (2) One Cohere key per
  deployment (C-5) — make it a provider→key map only for heterogeneous deployments.
  (3) The LLM reranker trims each passage to one line and sends full text within the
  output-budget heuristic; a token-aware truncation of very long passages is a future
  refinement (bounded today only by the model's context, not this code).
- **Tests (hermetic, no real keys/network).** `internal/rerank`: Cohere against an
  `httptest` fixture (reorder by relevance, 429-retry-then-succeed, terminal-400
  not-retried + key-not-leaked), missing-key/allowlist/unknown-provider fail-closed,
  disabled → nil; LLM reranker against a fake `Completer` (reorders in ONE call,
  tolerates fenced JSON, unparseable → `ErrUnparseable`, provider error propagates,
  omitted doc survives last, empty docs no-op). `internal/retrieve`: reranks when
  enabled, fallback-to-fused on reranker error and on factory error, no-rerank when
  disabled (no over-fetch), over-fetch-top_n-then-truncate, settings parsing, and the
  production `KeyedRerankerFactory` (nil/cohere/allowlist/llm/override paths).
  `internal/config`: Cohere key/base URL. `internal/cp/tenants`: `reranker.llm_model`
  validates. The golden-path e2e through the real `/v1/retrieve` + Postgres arrives
  with the query endpoint work (as the retrieve e2e already exercises the HTTP path).
- **Coverage.** `internal/retrieve` (coverage-gated) keeps its unit coverage;
  `internal/rerank` is unit-covered as above.
