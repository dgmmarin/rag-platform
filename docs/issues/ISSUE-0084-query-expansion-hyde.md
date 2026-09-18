# ISSUE-0084: Query expansion (HyDE) to fix vocabulary-mismatch retrieval misses

**Type:** Feature · **Status:** Done · **Priority:** High · **Traces:** FR-RET-01/06, SPEC-06 §1/§3, ADR-0079

## Summary
Questions that use different words from the documentation retrieve nothing useful. Live example on the
`qwen-demo` tenant: "can I still sell an offer if I have 0 allotment on the hotel?" did not surface the
"Waiting list (WL)" page even in the top 100, because the page says "book / waitlist / sold out", not
"sell / offer". Wider `k_text`/`k_vector` did not help (chunk outside the pool), and a reranker cannot
help (it only re-orders retrieved chunks). The existing follow-up rewrite (ADR-0057) does not apply to a
single-turn question and adds no vocabulary.

## What was built
- **HyDE query expansion** (`internal/query/expand.go`), gated by `settings.expansion.mode`
  (`off` default | `hyde`). When `hyde`, one cheap LLM call drafts a short hypothetical answer, and
  retrieval embeds THAT for the vector side.
- **Retrieval seam** (`internal/retrieve.Request.EmbedText`): embedded for the vector CTE when set; the
  full-text (BM25) `QueryText` and the reranker query stay the user's real `Query`.
- **Wiring:** `query.build` applies HyDE after the rewrite, reusing the tenant's existing
  `answer.ProviderFactory` (no new provider wiring); `settings.expansion.model` optionally overrides the
  model (allowlist-gated).
- **Settings:** `settings.expansion` added to the JSON schema + defaults (`{"mode":"off"}`); SPEC-02 §5
  and SPEC-06 updated.

## Validation (live, before/after)
Against the running API with the real corpus:
- Embedding the raw question → the WL page is **not in the top 100**.
- Embedding a HyDE-style hypothetical answer (what the feature sends) → the WL page is **ranks #1–10**.

## Not fixed by this
- The running API must be rebuilt/restarted to pick up the new binary; enabling
  `settings.expansion.mode=hyde` takes effect after that.
- Content bridges on source pages remain the most reliable per-page fix; HyDE is the systematic one.

## Tests
- `internal/query` unit: HyDE enabled embeds the hypothetical while keeping the question for full-text;
  `off` / no-factory / provider-error are strict passthrough (no EmbedText, query never fails).
- `internal/retrieve` unit: `EmbedText` is embedded while `QueryText` stays `Query`.

## Related
ADR-0079, ADR-0057 (follow-up rewrite), ISSUE-0074 (grounding floor / reranker), the retrieval-quality
diagnosis on the waitlist question.
