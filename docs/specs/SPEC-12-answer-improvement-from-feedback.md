# SPEC-12: Answer improvement from feedback

**Implements:** FR-RET-10 (extends), FR-FBK-01..08 (new, see §2) · **Relates:** SPEC-06 (retrieval and answering), SPEC-03 (tenant data model), SPEC-02 §5 (settings), ADR-0079 (query expansion), ADR-0003 (tenant isolation)

**Status:** Proposed. This spec defines a closed loop that turns end-user feedback into measurably better answers, without fine-tuning a base model and without weakening grounding or tenant isolation. It is phased (§7): each phase ships value on its own, and later phases are optional.

## 1. Context and signal available today

The platform already collects the raw signal but consumes none of it for quality:

- `POST /v1/feedback` writes a rating (thumbs up/down plus an optional comment) into the tenant's `query_feedback` table, keyed to a `query_log` row (FR-RET-10, SPEC-06 §5.4).
- `query_log` records, per answered query, the question, the retrieved chunk ids and scores, the model, latency and token counts (FR-RET-09).
- `GET /v1/queries` lists the log with the joined feedback for human review.
- Nothing in retrieval, answering, the reranker or the eval harness reads `query_feedback`. A thumbs-down changes nothing.

The corpus is the tenant's own documents, and answers must stay grounded in retrieved context with citations (SPEC-06 §4). So "improve answers" means: retrieve the right chunks more often, and, where the corpus itself is the gap, make the gap visible and fixable. It does not mean letting the model invent ungrounded answers from popular votes.

## 2. New requirements (FR-FBK)

| ID | Requirement | Priority |
|---|---|---|
| FR-FBK-01 | The system SHALL provide a low-rated-queries view: logged queries with a negative rating, newest first, per tenant. | M |
| FR-FBK-02 | An operator SHALL be able to promote a logged query (with its expected answer) into an eval case in one step. | M |
| FR-FBK-03 | The system SHALL let an operator attach a curated answer to a question pattern for a tenant, stored as tenant content and retrievable like a document. | S |
| FR-FBK-04 | A curated answer SHALL be grounded: it cites the source documents it summarises, and it is subject to the same isolation and retention as other tenant content. | M |
| FR-FBK-05 | The eval harness SHALL run the feedback-derived eval set and report recall, grounded rate and answer correctness before and after a change. | M |
| FR-FBK-06 | Aggregated feedback SHALL surface content gaps: recurring negative questions with no strong retrieval match. | S |
| FR-FBK-07 | Feedback signals used for ranking SHALL never override grounding or citation rules, and SHALL be per tenant (no cross-tenant learning). | M |
| FR-FBK-08 | Feedback processing SHALL be abuse-resistant: a single principal cannot materially move ranking or promote content by repeated voting. | S |

## 3. Principles

1. **Grounded first.** Feedback tunes *retrieval and curation*, never the grounding gate. An answer with no supporting chunk still refuses (SPEC-06 §4), whatever the votes say.
2. **Human in the loop for content.** A down-vote is a signal, not an edit. Promoting an eval case or publishing a curated answer is an explicit operator action (FR-FBK-02/03), audited (SPEC-02 §6).
3. **Per-tenant only.** All learned signal stays inside the tenant database (ADR-0003, C-1). No cross-tenant model or shared ranking.
4. **No base-model fine-tuning (initially).** The base LLM stays as configured (SPEC-02 §5). Improvement comes from better retrieval, curated grounded content, few-shot exemplars and evaluation. Model fine-tuning is an explicit non-goal of the early phases (§8).
5. **Measured, not assumed.** Every change is validated by the eval harness on a feedback-derived set (FR-FBK-05), so a "fix" that regresses is caught before it ships.
6. **Privacy and safety.** Feedback comments are user content: never logged at info level, never sent to an unrelated service (C-3/C-4). Curated answers pass the same prompt-injection posture as ingested content (context is data, not instructions, SPEC-09 §2).

## 4. The loop

```
answer ──► user rates (FR-RET-10) ──► query_feedback
                                          │
              ┌───────────────────────────┼───────────────────────────┐
              ▼                            ▼                           ▼
   low-rated view (FR-FBK-01)   content-gap report (FR-FBK-06)   promote to eval (FR-FBK-02)
              │                            │                           │
              ▼                            ▼                           ▼
   operator triages          author content bridge OR         eval set grows
              │               curated answer (FR-FBK-03/04)            │
              └───────────────► change (content / settings) ──► eval gate (FR-FBK-05) ──► ship
```

The loop always closes through evaluation, so improvement is provable and regressions are blocked.

## 5. Components

### 5.1 Feedback triage (Phase 1, FR-FBK-01/02)
- **Reader:** `GET /v1/queries?rating=down` (extends the existing query-log list) returns negative-rated entries with the question, the retrieved chunk ids/scores and the produced answer.
- **Admin UI:** a "Needs attention" view over that reader, and a one-click "Add to eval cases" that writes an `eval_case` from the query (question + the answer the operator marks as expected, or the cited sources as the expected grounding). This is the `feedback → eval_case` bridge that does not exist today.
- **No pipeline change.** Phase 1 is read + curate only.

### 5.2 Curated answers (Phase 2, FR-FBK-03/04)
Some questions are answered badly because the corpus phrases the answer in words the question never uses (the "sell an offer / 0 allotment / Waiting list" case). Two complementary fixes:
- **Content bridge (author at source):** the operator edits the source page to add the missing phrasing. Reliable, already possible, no new mechanism. The triage view should point the operator at the top-cited document to edit.
- **Curated answer (in-platform):** for questions the source cannot easily host, the operator writes a short grounded answer bound to a question pattern. It is stored as a first-class **tenant document** (`source_kind = curated`), chunked and embedded like any other, so retrieval finds it through the normal hybrid path. It MUST cite the underlying sources (FR-FBK-04); an uncited curated answer is rejected. It is soft-deletable and GC-eligible like other content (SPEC-03 §4).
- Query expansion (HyDE, ADR-0079) is the automatic complement: it fixes wording mismatches without editing content, and pairs with curated answers rather than replacing them.

### 5.3 Feedback-weighted retrieval (Phase 3, FR-FBK-07/08, optional)
A conservative ranking signal, never a grounding override:
- **Exemplar retrieval / few-shot:** up-voted (question, grounded-answer) pairs become retrieval exemplars, injected as few-shot guidance to the answer prompt for similar questions. Bounded, per tenant, cited only from real chunks.
- **Chunk usefulness prior:** chunks that repeatedly appear in up-voted answers get a small, capped rank boost; chunks in down-voted answers a small penalty. Applied as a re-rank tie-breaker after fusion, never large enough to surface an ungrounded or filtered chunk. Abuse-resistant: dedupe by principal, decay over time, cap the total adjustment (FR-FBK-08).
- **Reranker tuning:** the up/down labels are a training set for a per-tenant reranker if one is ever trained; out of scope until the reranker itself is in wide use.

### 5.4 Evaluation gate (Phase 1+, FR-FBK-05)
The eval harness (EPIC-12) runs the feedback-derived eval set and reports recall@k, grounded rate and LLM-judged correctness. A change (content, settings, expansion, ranking weights) is compared before/after on this set, and CI can gate on no regression. This is what makes every phase safe.

## 6. Data model (tenant database, ADR-0003)

- `query_feedback` (exists): rating, comment, `query_id`. Extend reads only.
- `eval_case` (exists, EPIC-12): gains rows via FR-FBK-02. No schema change beyond a `source: feedback` provenance tag.
- `curated_answer` (new, Phase 2): `id`, `question_pattern`, `answer_markdown`, `cited_document_ids`, `created_by`, `status`, timestamps. On publish it is materialised into the normal `documents`/`document_versions`/`chunks` tables as a `curated` source, so retrieval needs no special path.
- `chunk_feedback_stat` (new, Phase 3, optional): per-chunk up/down counts with decay, for the capped usefulness prior. Never leaves the tenant DB.

All new tables are tenant content, reached only through the resolver and `*tenant.DB` (C-1, C-3), and are covered by the isolation suite (SPEC-01 §9).

## 7. Phasing

| Phase | Delivers | New surface | Risk |
|---|---|---|---|
| 1. Triage + eval bridge | Low-rated view, one-click promote to eval, eval gate on the feedback set | read endpoint + UI, eval provenance tag | low |
| 2. Curation | Curated grounded answers + content-gap report | `curated_answer`, gap report | medium (content authoring) |
| 3. Feedback-weighted retrieval | Exemplars + capped chunk prior | `chunk_feedback_stat`, rank tie-breaker | medium (must not break grounding) |
| 4. Model tuning | Per-tenant reranker / model tuning | training pipeline | high, deferred |

Ship Phase 1 first: it is low-risk, needs no pipeline change, and immediately turns feedback into a measured tuning loop.

## 8. Non-goals

- Base-LLM fine-tuning from votes (Phase 4 at most, deferred).
- Cross-tenant learning or a shared ranking model (violates ADR-0003).
- Letting popularity override grounding or citations (FR-FBK-07).
- Fully automatic content changes with no operator review (Phase 2/3 keep a human for anything that publishes content).

## 9. Observability and abuse

- Metrics: feedback volume and up/down rate per tenant; eval recall/grounded/correctness trend; curated-answer hit rate; expansion effect on rank (ties to SPEC-10 §2).
- Abuse: rate-limit feedback per principal; dedupe votes by principal for any ranking use; decay old votes; cap the total ranking adjustment so no vote campaign can surface ungrounded content (FR-FBK-08).
- Audit: promoting an eval case and publishing a curated answer write audit events (SPEC-02 §6).

## 10. Open decisions (ADR candidates)

- Curated answer as materialised tenant document vs a separate retrieval path (this spec proposes materialised, for zero special-casing). ADR on publish.
- The exact capped-prior formula and decay for §5.3, if Phase 3 proceeds. ADR on Phase 3.
- Whether the eval gate blocks CI or only reports, per tenant. ADR with Phase 1.
