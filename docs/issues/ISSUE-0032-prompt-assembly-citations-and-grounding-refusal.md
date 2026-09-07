# ISSUE-0032: Prompt assembly, citations and grounding refusal

**Type:** Feature · **Status:** Done · **Story:** STORY-08.5 · **Traces:** FR-RET-04, FR-RET-05, SPEC-06 §4–5, SPEC-09 §2, NFR-REL-04, ADR-0024, ADR-0055

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-08.5 for traceability; the backlog
> story remains the authoritative work item.

## Summary
The answering stage (SPEC-06 §4–5, FR-RET-04/05): a new `internal/answer` package
that turns the ranked chunks (STORY-08.1/08.3) + the question into a grounded, cited
answer through the `internal/llm` seam (STORY-08.4) — or a fixed refusal with **no**
LLM call when nothing passes `min_score`. It is the seam STORY-08.6 wraps with the
`/v1/query` endpoint + SSE.

## Scope
- New `internal/answer`: grounding gate, prompt assembly, `[n]` citation mapping,
  usage folding into `usage_daily`, and a query-log seam for 08.8.
- `settings.answering` (`token_budget`, `history_n`) added to the settings
  schema/defaults; `min_score` already lives under `settings.retrieval`.
- Not in scope (later stories): the `/v1/query` HTTP endpoint + SSE (08.6), the
  follow-up→standalone question rewrite (08.7), and the async query-log/feedback
  persistence (08.8 — a `QueryLogger` seam is left, called on both paths).
- Not touched: the retrieval/rerank logic (08.1/08.3).

## Resolution
- `internal/answer/answer.go`:
  - `Service.Answer(ctx, Request) (Result, error)` — SPEC-06 §6 shape
    `{id, answer, grounded, citations[], usage{retrieval_ms, generation_ms,
    in_tokens, out_tokens}, model}`.
  - **Grounding gate (§4):** keep chunks with `Score ≥ min_score`; none → fixed
    refusal `"I couldn't find information about that in <tenant name>'s content."`,
    `grounded=false`, zero citations, and return **before** building the provider
    (no LLM call). `min_score` defaults to 0.02 when ≤0 (fail-safe).
  - **Prompt assembly (§5):** system prompt (answer only from sources, cite `[n]`,
    say when unsure, reply in the user's language); a numbered `Sources:` context
    block trimmed to `token_budget` (chars/4 estimate, ponytail; top chunk always
    included); the last `history_n` history turns verbatim.
  - **Citations (§5):** parse `\[(\d+)\]`, map each in-range distinct `n` to
    `included[n-1]`, build `{n, document_id, title, uri, heading_path, snippet}`;
    drop unreferenced chunks and out-of-range markers; numbers not renumbered.
  - **Usage (FR-RET-04, ADR-0024):** fold `Response.Usage` into
    `usage.Delta{LLMInTokens, LLMOutTokens}` (a `UsageRecorder`, `*usage.Counter`
    satisfies it) and into the response `usage`; none on refusal.
  - **Provider failure (NFR-REL-04):** wrapped preserving `llm.ErrCircuitOpen`.
  - Seams: `ProviderFactory` (prod `KeyedProviderFactory` over `llm.Factory`),
    `UsageRecorder`, `QueryLogger` (called on both paths for 08.8).
- `internal/answer/answer_test.go`: hermetic unit tests (fake provider/factory/
  usage/logger) — refusal without LLM call + fixed message, empty chunks refuse,
  `[n]` mapping + unreferenced drop, out-of-range ignored, token-budget exclusion,
  history included + truncated to N, system-prompt instructions, usage folded (and
  none on refusal), provider error wraps `ErrCircuitOpen`, logging seam on both
  paths, defaults applied. Coverage 88.8%.
- `internal/cp/tenants/settings_schema.json` + `settings_defaults.json`: optional
  `answering` object (`token_budget` 6000, `history_n` 6). Drift/validation green.
- `docs/specs/SPEC-06-retrieval-and-answering.md`: §5.2 documents the seam; header
  `Decisions:` gains ADR-0055.

## Verification
- `mise run test` (clean env) — all packages pass; `internal/answer` 88.8%.
- `go vet ./internal/answer/` clean; golangci-lint (v2) on the package: 0 issues.
- `go build ./...` OK.
- No migration, no OpenAPI change, no new dependency.

## Notes / ceilings (ponytail)
- Token budget is a `chars/4` estimate, not a real tokenizer — upgrade to
  `count_tokens` if exactness matters; today it only bounds the context.
- Citation snippet is a fixed 240-rune window, not sentence-aware.
- The golden-path e2e through the real `/v1/query` + Postgres arrives with
  STORY-08.6 (the endpoint), precedent ADR-0053/0054; the answering logic is pure
  (no DB) and fully unit-covered.
