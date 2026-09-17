# ISSUE-0074: Grounding floor (min_score) filters all semantic hits without a reranker

**Type:** Bug · **Status:** Fixed · **Priority:** High · **Traces:** FR-RET-06, SPEC-06 §3/§4, ADR-0055

## Summary
The grounding floor `settings.retrieval.min_score` (default 0.02, `internal/answer/answer.go`
`DefaultMinScore`) is compared against each chunk's `Score` in `passFloor`. That `Score` is the
reranker's 0..1 relevance score **only when a reranker runs**; with the reranker **off** (the platform
default) it is the fused **RRF** value `sum(1/(60+rank))`, where a chunk found by only one retrieval
list tops out at `1/61 ≈ 0.0164 < 0.02`. So every purely-semantic hit (in the vector list, absent from
the BM25 list) is filtered, and retrieval degrades to **keyword-only**: natural-language questions
return "I couldn't find information…" with no citations, while literal keyword phrases work.

## Observed (tenant acme / manual.tourpaq.com)
- `"per room per stay"` (both lists) → grounded, 8 citations.
- `"information about passenger air travel"` (vector-only, RRF max 0.0164) → 0 citations, refusal.
- Lowering `min_score` to 0.005 (below 1/61) made the semantic queries answer — confirming the floor,
  not retrieval, was dropping them.

## Root cause
A single absolute floor is applied to two different score scales. `0.02` is right for a reranker's
0..1 score but meaningless on rank-based RRF, and the platform ships with the reranker disabled.

## Fix
`internal/answer/answer.go`: add `Settings.Reranked`; `minScore()` returns `0` (floor disabled) when
`!Reranked`, and the configured/default floor otherwise. `internal/query/query.go`
`parseAnswerSettings` sets `Reranked` from `settings.reranker.enabled`. Without a reranker the floor no
longer applies — chunks flow to the model, which still refuses via its own grounding when the context
does not support an answer (SPEC-06 §4); with a reranker the 0..1 floor works as before.

Tests: `internal/answer` `TestBelowFloorPassesWithoutReranker` (low RRF score answers when not
reranked) plus the existing floor tests updated to mark chunks reranked; `internal/query` floor tests
enable the reranker in the settings doc. `go test ./internal/answer/ ./internal/query/`: PASS.

## Follow-up
- Optionally normalise the fused score to 0..1 so a floor could apply meaningfully even without a
  reranker; not required — the model's own grounding refusal covers the no-reranker case.
- The admin Settings help text for "Minimum score" now states the floor applies only with a reranker.
