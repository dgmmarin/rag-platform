# ISSUE-0086: Answer improvement from feedback (epic / spec)

**Type:** Epic · **Status:** Proposed · **Priority:** Medium · **Traces:** SPEC-12, FR-RET-10, FR-FBK-01..08, ADR-0079

## Summary
Feedback (`query_feedback`, FR-RET-10) is collected but consumed by nothing in the pipeline, so a
thumbs-down changes no future answer. SPEC-12 defines a closed loop that turns feedback into measurably
better answers, without base-model fine-tuning and without weakening grounding or tenant isolation
(ADR-0003). This issue tracks the spec and its phased delivery.

## Design
See [SPEC-12](../specs/SPEC-12-answer-improvement-from-feedback.md). Principles: grounded first, human in
the loop for content, per-tenant only, measured by the eval harness, privacy/abuse safe.

## Phased stories (to break out when scheduled)
1. **Triage + eval bridge (low risk, do first).** `GET /v1/queries?rating=down`; admin "Needs attention"
   view; one-click "Add to eval cases" (`feedback → eval_case`); run the feedback-derived eval set as a
   gate. No retrieval/answer pipeline change.
2. **Curation.** `curated_answer` published as a `curated`-source tenant document (chunked/embedded like
   any doc, must cite sources); content-gap report; pairs with HyDE (ADR-0079).
3. **Feedback-weighted retrieval (optional).** Up-voted exemplars as few-shot; a capped, decaying
   per-chunk usefulness prior applied as a re-rank tie-breaker that can never surface an ungrounded or
   filtered chunk; abuse-resistant (dedupe by principal, cap total adjustment).
4. **Model tuning (deferred, high risk).** Per-tenant reranker / model tuning from labels.

## Follow-ups before Phase 1 code
- Add FR-FBK-01..08 to the SRS §3.5 (this spec introduces them; the SRS table should carry them).
- ADR on: eval gate blocks CI vs reports; curated-answer materialisation (SPEC-12 §10).

## Related
SPEC-12, SPEC-06 §5.4 (feedback), EPIC-12 (eval harness), ADR-0079 (query expansion), ADR-0003.
