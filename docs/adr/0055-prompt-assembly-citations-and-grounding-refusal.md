# ADR-0055: Prompt assembly, citations and grounding refusal — the `internal/answer` seam, min_score gate short-circuiting the LLM, `[n]`→chunk citation mapping, chars/4 token budget, usage folded into usage_daily

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-RET-04, FR-RET-05, SPEC-06 §4–5, SPEC-09 §2, NFR-REL-04, C-3, C-4 · **Decisions:** ADR-0024, ADR-0051, ADR-0052, ADR-0053, ADR-0054

## Context
SPEC-06 §4–5 is the answering stage: turn the ranked chunks (STORY-08.1/08.3) and
the question into a grounded, cited answer through the `internal/llm` seam
(STORY-08.4, ADR-0053) — or, when nothing is relevant enough, a fixed refusal with
**no** LLM call (SPEC-06 §4, FR-RET-05). STORY-08.5 builds exactly this and nothing
adjacent: the `/v1/query` HTTP endpoint + SSE (08.6), the follow-up→standalone
question rewrite (08.7) and the async query-log/feedback persistence (08.8) are
explicitly out of scope, each left as a clean seam. STORY-08.3 deliberately did NOT
apply the `min_score` floor ("that is STORY-08.5"), so the grounding gate is this
story's to own. Constraints: no tenant content in the control plane (C-3); secrets
never logged, errors carry no prompt content (C-4); providers gated by
`settings.providers_allowed` (SPEC-09 §2, enforced in `internal/llm`); provider loss
degrades gracefully (NFR-REL-04).

## Options / decisions
- **A new `internal/answer` package, not code inside `internal/retrieve`.** The
  answering stage is a distinct responsibility (prompt/citations/grounding) with a
  different consumer (the query endpoint, not `/v1/retrieve`). A separate package
  keeps `internal/retrieve` (coverage-gated) focused and lets `answer` be unit-tested
  hermetically. `answer` depends on `internal/llm` and `internal/cp/usage` only —
  **not** on `internal/retrieve` (no import cycle, and the answering stage stays
  independent of the retrieval pipeline's shape).
- **A local `Chunk` type, not `retrieve.Result`.** `answer.Chunk` carries exactly
  the citation metadata SPEC-06 §5 needs (`ChunkID, DocumentID, Title, URI,
  HeadingPath, Content, Score`). The caller (08.6) maps `retrieve.Result → Chunk` —
  a trivial field copy — which keeps `answer` decoupled and its tests free of the
  retrieval package. `Chunk.Score` is documented as "reranker score when reranking
  is enabled, else fused RRF score" so the grounding floor is applied to the right
  number **without** `answer` knowing or caring which (mirrors SPEC-06 §3's
  "min_score then applies to reranker score").
- **`Answer(ctx, Request) (Result, error)`.** `Request{TenantID, Question, Chunks,
  History, Settings, RetrievalMs}` → `Result{ID, Answer, Grounded, Citations, Usage,
  Model}` (SPEC-06 §6). `Settings` is the resolved subset the caller passes in
  (tenant display name + `retrieval.min_score` + `answering.{token_budget,history_n}`
  + `llm.{provider,model,max_tokens,models_allowed}` + `providers_allowed`), so the
  service takes no `SettingsSource`/control-plane dependency and tests stay hermetic.
  `RetrievalMs` is measured by the caller (the answering stage does not retrieve) and
  folded into the response `usage`.
- **Grounding gate short-circuits BEFORE the provider is built (SPEC-06 §4).** The
  min_score filter runs first; if no chunk passes, `Answer` returns the fixed message
  `"I couldn't find information about that in <tenant name>'s content."` with
  `grounded=false`, zero citations, and **returns before the `ProviderFactory` is
  even called** — proven by a test asserting both `factory.calls == 0` and
  `provider.Complete calls == 0`. The tenant display name reaches the message via
  `Settings.TenantName` (the caller reads it from the control-plane `tenants.name`
  row — C-3: the name is registry data, not tenant content).
- **`min_score` default 0.02, applied when ≤ 0.** Matches `settings_defaults.json`.
  A non-positive configured value is treated as unset (defaulted), so grounding never
  silently degrades to "every chunk passes" if the caller forgets to populate it —
  fail-safe, not fail-open.
- **`[n]`→chunk citation mapping over the INCLUDED chunks; markers keep their
  number.** The context numbers the grounded chunks that fit the budget (1-based);
  `Answer` parses `\[(\d+)\]` markers from the model's text, maps each in-range
  distinct `n` to `included[n-1]`, and builds `{n, document_id, title, uri,
  heading_path, snippet}`. Unreferenced chunks are **dropped**; out-of-range markers
  are ignored. Citations are **not** renumbered — `n` stays exactly as it appears in
  the answer text so `[n]` in the prose always lines up with the citation list. A
  short single-line `snippet` (≤240 runes, ponytail) is derived from the content.
- **Token budget as a `len/4` estimate (ponytail).** SPEC-06 §5's "truncated to a
  token budget (default 6k)" is realised with a `chars/4` heuristic — no tokenizer
  dependency (lazy: the exact count isn't load-bearing, only a bound that keeps the
  context off the model's ceiling). Whole chunks are added greedily in rank order
  while they fit; the **top** chunk is always included (its content truncated to fit
  if it alone overflows) so a grounded query never has an empty context. The upgrade
  path (a real `count_tokens`) is named in a `ponytail:` comment. `token_budget` and
  `history_n` are read from a new `settings.answering` object (defaults 6000 / 6).
- **History included verbatim; the final user turn carries the sources.** The last
  `history_n` turns become `llm.Message`s ahead of a final user message that holds the
  numbered `Sources:` block and the `Question:`. The follow-up→standalone rewrite is
  STORY-08.7 — this story includes provided history unchanged. Ingested/crawled
  passage text is presented as **data** under a delimited header, never instructions;
  a fixed system prompt owns the task (prompt-injection defence, SPEC-09 §2).
- **System prompt = the SPEC-06 §5 instructions.** Tenant name; answer ONLY from the
  provided sources; cite as `[n]`; say when unsure; **reply in the user's language**
  (a prompt instruction — no language-detection library, the model matches). Asserted
  by a test on the assembled `Request.System`.
- **Usage folded into usage_daily AND the response (FR-RET-04, ADR-0024).** On the
  grounded path the provider's normalised `Usage` becomes
  `usage.Delta{LLMInTokens, LLMOutTokens}` handed to a `UsageRecorder` (`*usage.Counter`
  satisfies it structurally; nil = no counter, the response still carries usage) — the
  EPIC-05-reserved "LLM tokens in EPIC-08 answering" counters — and fills the response
  `usage{retrieval_ms, generation_ms, in_tokens, out_tokens}`. The refusal path records
  **no** LLM usage (no call). The `Queries` counter is deliberately **not** incremented
  here — that belongs to the query endpoint (08.6), avoiding a double count.
- **Provider failure surfaced cleanly (NFR-REL-04).** A `Complete` error is wrapped
  with `%w` so `errors.Is(err, llm.ErrCircuitOpen)` holds — STORY-08.6 decides whether
  to degrade to retrieval-only. The wrap carries no prompt content (C-4).
- **Logging seam for 08.8 called on BOTH paths.** A `QueryLogger` (nil = no-op) is
  invoked for the refusal and the grounded answer alike, with a `QueryRecord` carrying
  the retrieved chunk ids + scores, `grounded`, the cited chunk ids, usage and model —
  so 08.8's async query log can persist either path. Proven by a test that the
  grounded=false path logs one `grounded:false` record with the retrieved ids.

## Decision
Add `internal/answer`: `Service.Answer`, the `Chunk`/`Turn`/`Settings`/`Request`/
`Usage`/`Citation`/`Result`/`QueryRecord` types, the `ProviderFactory`/
`UsageRecorder`/`QueryLogger` seams, and the production `KeyedProviderFactory` (wraps
`llm.Factory`). `settings_schema.json`/`settings_defaults.json` gain the optional
`answering` object (`token_budget` 6000, `history_n` 6); `min_score` already lives
under `retrieval`. SPEC-06 §5.2 documents the seam. No HTTP wiring (08.6), no schema
migration, no OpenAPI change.

## Consequences
- **The seam 08.6 wraps.** The query endpoint maps `retrieve.Result → answer.Chunk`,
  measures `RetrievalMs`, calls `Answer`, and renders `Result` as JSON/SSE, wiring the
  `usage.Counter` (`UsageRecorder`) and, later, the 08.8 `QueryLogger`. The production
  `KeyedProviderFactory` is ready; nothing in `internal/cli` changes in this story.
- **No new dependency.** Reuses `internal/llm`, `internal/cp/usage`, `google/uuid`
  (already vendored, matches the `documents` id convention). `go 1.22` unchanged.
- **No migration / OpenAPI change.** `settings` is jsonb; only the schema JSON gained
  an optional object (defaults still validate — drift/validation stays green). The
  `/v1/query` contract lands with 08.6.
- **Fail-safe grounding.** A missing/≤0 `min_score` defaults to 0.02 rather than
  admitting everything; the refusal path is structurally incapable of an LLM call
  (returns before the factory).
- **Ceilings (ponytail):** (1) token budget is a `chars/4` estimate, not a real
  tokenizer — upgrade to `count_tokens` if exactness ever matters; today it only
  bounds the context, never the sole guard against the model's hard limit. (2) The
  citation snippet is a fixed 240-rune window, not sentence-aware. (3) One LLM key set
  per deployment via `llm.Factory` (C-5), inherited from ADR-0053.
- **Tests (hermetic — no keys/network/DB).** Against a fake `llm.Provider` + fake
  factory: below-floor and empty-chunks → `grounded=false`, fixed message with the
  tenant name, and **zero** provider/factory calls; `[n]` mapping maps to the right
  chunk ids and drops the unreferenced chunk; out-of-range markers ignored;
  token-budget truncation excludes the overflow chunk from the assembled prompt and
  its citation; history included verbatim and truncated to last N; the system prompt
  carries the tenant name + language-match + `[n]` + "only" instructions; usage folded
  into the counter (`LLMInTokens/LLMOutTokens`) and the response, none on refusal;
  provider error wraps `llm.ErrCircuitOpen`; the logging seam fires on both paths;
  defaults applied when unset. Coverage 88.8% of `internal/answer`.
- **e2e deferred to STORY-08.6 (precedent ADR-0053/0054).** This story adds no HTTP
  endpoint — the golden-path e2e through the real `/v1/query` + Postgres arrives with
  08.6, where the answering service is first reachable over HTTP. The answering logic
  itself is pure (no DB) and fully unit-covered.
