# ADR-0079: Query expansion (HyDE) for the vector side of hybrid retrieval

**Status:** Accepted · **Date:** 2026-09-18 · **Requirements:** FR-RET-01/06, NFR-REL-04 · **Decisions:** SPEC-06 §1/§3, ADR-0057 (follow-up rewrite), ADR-0003 (tenant isolation) · **Relates:** ISSUE-0084

## Context
Hybrid retrieval embeds the user's question and matches it against chunk embeddings (plus BM25). When
the question and the answer passage share little vocabulary, the right chunk ranks far down. A live
example: the question "can I still sell an offer if I have 0 allotment on the hotel?" did not retrieve
the "Waiting list (WL)" page **even in the top 100**, because the page speaks "book / waitlist / sold
out", not "sell / offer". Raising `k_text`/`k_vector` did not help (the chunk was outside the pool),
and a reranker cannot help either — it only re-orders chunks retrieval already fetched.

The existing follow-up **rewrite** (ADR-0057) does not address this: it only condenses a multi-turn
follow-up into a standalone question using history; it adds no vocabulary and is a no-op on a
single-turn question.

## Decision
Add an optional **query-expansion** step before retrieval, gated per tenant by
`settings.expansion.mode` (default `off`). The first mode is **HyDE** (Hypothetical Document
Embeddings): one cheap LLM call drafts a short hypothetical answer to the question, and retrieval
**embeds that answer** for the **vector** side instead of the question.

- **Vector side uses the hypothetical; full-text and reranker keep the real question.** The hypothetical
  is written in answer/document vocabulary, so its embedding lands near the target passage. BM25 keeps
  the user's exact terms (so real keywords still count), and the reranker keeps the user's question (so
  it scores against real intent). Realised by `internal/retrieve.Request.EmbedText`: when set, it is
  embedded for the vector CTE while `QueryText` (BM25) and the rerank query stay `Query`.
- **Reuse the tenant's LLM factory.** The step reuses the same `answer.ProviderFactory` the answer and
  rewrite paths already use (no new provider wiring); `settings.expansion.model` may swap in a cheaper
  model, still gated by the model allowlist inside the factory.
- **Lives beside the rewrite step** (`internal/query/expand.go`), applied in `query.build` after the
  rewrite: rewrite makes the question standalone, then HyDE drafts the hypothetical from it.

### HyDE over a synonym glossary or multi-query, first
HyDE is one LLM call and needs no curation, and it fixes vocabulary mismatch generically (proven on the
waitlist query: HyDE-shaped text returns the WL page at ranks #1–10). A hand-maintained synonym glossary
needs per-domain curation; multi-query (retrieve N reformulations, fuse) costs N retrievals. Both remain
open as future `expansion.mode` values behind the same seam; HyDE ships first as the highest value per
unit of complexity.

## Consequences
- **Strict passthrough / no regression (NFR-REL-04).** `mode:off`, no factory wired, or ANY failure
  (provider build, `Complete` error incl. `ErrCircuitOpen`, empty output) returns no EmbedText, and
  retrieval embeds the question exactly as before. Expansion never fails a query. Default is `off`, so
  existing tenants are byte-identical until they opt in.
- **Cost/latency.** One extra short LLM call (`hydeMaxTokens = 256`) per query when enabled. It reuses
  the tenant's configured provider (Claude here) — no new dependency.
- **The answer stage is unaffected.** It still receives the user's ORIGINAL question + history
  (SPEC-06 §5); only what retrieval embeds changes.
- **Prompt-injection posture.** The question is passed to the HyDE call as data, not instructions
  (SPEC-09 §2), matching the rewrite step; the hypothetical is used only as an embedding input, never
  shown to the user or the answer model.
- **Isolation unchanged (ADR-0003).** Expansion touches no tenant data; it only transforms the query
  text before the existing tenant-scoped retrieval.

## Observability / follow-up
- A hit-rate signal (did the expanded query change the top-k?) and an eval on a fixed question set would
  quantify the gain; EPIC-12's eval harness is the home for a before/after rank metric.
- Content bridges on the source pages remain the most reliable per-page fix; HyDE is the systematic fix
  that works without editing every page.
