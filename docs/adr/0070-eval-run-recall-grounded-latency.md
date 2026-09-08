# ADR-0070: `ragctl eval run` — recall@k / grounded-rate / latency over the real pipeline, behind injectable ports

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** FR-ADM-04, SPEC-06 §8 · **Decisions:** ADR-0003, ADR-0007, ADR-0009, ADR-0069

## Context
STORY-12.2 adds the *running* half of the evaluation harness (FR-ADM-04, SPEC-06 §8):
`ragctl eval run <slug>` executes a tenant's `eval_cases` through the real retrieval +
answering pipeline, records an `eval_runs` row plus one `eval_results` row per case, and
prints recall@k, grounded rate and mean latency — the gate before changing a tenant's
chunking/retrieval settings. The `eval_runs`/`eval_results` tables already exist. LLM-as-judge
correctness (`judged_correct`, a correctness figure in the summary) is STORY-12.3 and is out
of scope; the summary here carries no correctness number and `judged_correct` is left NULL.

Retrieval and grounding must NOT be reimplemented: the story reuses `retrieve.Service.Search`
(for retrieved doc ids → recall@k) and `query.Service.Query` (for `grounded` and per-case
latency), wired exactly as the `serve` composition root wires them.

## Options / decisions
- **A pure runner behind three ports, so scoring is unit-testable without a DB or an LLM.**
  `eval.Runner` depends on `Pipeline` (retrieve+answer), `caseSource` (list cases) and
  `runSink` (persist run/results) — all injectable. The scoring functions (`recallHit`,
  `distinct`, `summarize`) are pure and unit-tested directly; the runner loop is tested with
  fakes; the DB-backed `RunStore` (over `eval_runs`/`eval_results`) is exercised by an
  `//go:build e2e` test against a real tenant DB. The production `Pipeline` adapter (wrapping
  `retrieve.Service` + `query.Service`) lives in `internal/cli`, so `internal/eval` keeps no
  dependency on the retrieval/answer packages.

- **recall@k definition.** `k` = the run's effective `settings.retrieval.final_k` (default 8,
  ADR-0007), overridable by `--config-file`. Per case, `recall_hit` = at least one of the
  case's `expected_doc_ids` is among the **distinct document ids behind the top-k retrieved
  chunks** (retrieval returns chunks; several may share a document, so the set is deduped).
  A case with **no** `expected_doc_ids` is **excluded** from recall — `recall_hit` is stored
  NULL — because recall is undefined without a ground truth; auto-missing such cases would
  unfairly depress the score for answer-only cases. The printed **recall@k** = `hits /
  cases-with-expected-docs`.

- **grounded rate** = fraction of ALL cases whose answer returned `grounded=true` (the
  `POST /v1/query` grounding decision). Denominator is every case run (answer-only cases
  included), since grounding is defined for every question.

- **mean latency** = mean over all cases of the wall-clock time around the answer (query)
  call, in ms — the metric an operator tuning settings cares about. Retrieval time for the
  separate recall@k `Search` call is not included.

- **`--config-file` (not `--config`).** An optional partial settings document overlaid on the
  tenant's live settings **for this run only** — it never mutates stored settings. Both the
  retrieval and answering stages read settings through one `SettingsSource`, so a deep-merged
  overlay applies uniformly (retrieval/answering/reranker/llm). The **effective merged**
  settings are stored in `eval_runs.config` so a run is reproducible. The flag is
  `--config-file` because the global `--config` is already the ragctl config-file flag
  (ADR-0009); `<slug>` is a positional argument.

- **Fail-soft per case.** A pipeline error for one case records that case (recall miss, not
  grounded, latency measured, error counted in the summary) and the run continues; only a
  persistence error (create/record/finish) aborts. A single bad case must not lose a whole
  run's results.

## Consequences
- An operator can measure retrieval/answer quality for a tenant and compare runs before/after
  a settings change, with every run and per-case result persisted for later inspection (and
  for the STORY-12.4 admin report / CI gate to build on).
- The runner is fully unit-tested (pure scorer + fake pipeline/sink); the DB write path is
  e2e-tested with a fake pipeline, so it needs no LLM/embedding keys. The real pipeline
  wiring reuses the `serve` composition root unchanged (no new retrieval/answer code).
- No schema change; `eval_runs.summary` stores the recall@k/grounded/latency figures as JSON;
  `judged_correct` stays NULL until STORY-12.3.
- **ponytail:** the run is sequential (one case at a time). Ceiling: a large eval set takes
  proportional wall-clock and one LLM call per grounded case. Upgrade path: bounded-concurrency
  fan-out over cases if eval sets grow large.
- **ponytail:** recall counts a document as "retrieved" if any of its chunks is in the top-k
  chunks (chunk-level k, document-level hit). Ceiling: k is a chunk budget, not a document
  budget. Upgrade path: a document-level k if that ever matters for a tenant's tuning.
