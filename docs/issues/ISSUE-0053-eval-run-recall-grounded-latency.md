# ISSUE-0053: `ragctl eval run` — recall@k, grounded rate, latency (STORY-12.2)

**Type:** Feature · **Status:** Done · **Story:** STORY-12.2 · **Traces:** FR-ADM-04, SPEC-06 §8

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs (ADR-0070).

## Summary
Add the *running* half of the evaluation harness (SPEC-06 §8): `ragctl eval run <slug>
[--config-file file]` runs a tenant's `eval_cases` through the real retrieval + answering
pipeline, records an `eval_runs` row plus one `eval_results` row per case, and prints
recall@k, grounded rate and mean latency. LLM-as-judge correctness is STORY-12.3 (NOT built:
`judged_correct` left NULL, no correctness figure in the summary); the admin report / CI gate
is STORY-12.4.

## Scope
- **`internal/eval` (extended):**
  - `run.go` — `Runner` (DB- and pipeline-agnostic, behind the `Pipeline`, `caseSource`,
    `runSink` ports), the exported `Pipeline` port, `CaseResult`/`Summary`, and the pure
    scorers `distinct` / `recallHit` / `summarize`.
  - `runstore.go` — `RunStore` (`RunWriter`) over `eval_runs`/`eval_results` via `*tenant.DB`
    (ADR-0003, C-3), plus the DB-backed port adapters.
  - `service.go` — `Service.Run(ctx, tid, RunOptions)` opens the tenant DB once and drives the
    runner; `Service.Runs` (optional, defaults to `NewRunStore()`).
- **`internal/cli/eval_run.go`** — `ragctl eval run <slug>`: composes retrieve+query exactly
  as `serve` does, applies the `--config-file` overlay (deep-merged onto tenant settings for
  the run only, stored in `eval_runs.config`), resolves k, and prints the summary. Registered
  as `EvalRunCmd` in `internal/cli/eval.go`.

## Definitions (ADR-0070, SPEC-06 §8.1)
- **k** = effective `settings.retrieval.final_k` (default 8), overridable by `--config-file`.
- **recall_hit** = at least one `expected_doc_ids` element in the distinct documents behind the
  top-k retrieved chunks; a case with no expected docs is excluded (NULL). **recall@k** =
  hits / cases-with-expected-docs.
- **grounded rate** = grounded answers / all cases. **mean latency** = mean ms around the
  query (answer) call. Per-case pipeline errors are fail-soft.

## Tests / runnable checks
- **Unit** (`internal/eval`): `distinct`, `recallHit` (nil/hit/miss/empty), `summarize`
  (recall denominator excludes nil, grounded rate, mean latency, errors), and `Runner.Run`
  golden path + fail-soft-per-case + config-stored, all with fake ports. `mise run test`:
  **PASS**.
- **CLI** (`internal/cli`): `eval run acme` added to `TestEvalCommandsRequireURL` — fails
  closed with a "control-plane URL" error when none is set (hermetic via `TestMain`).
- **e2e** (`test/e2e/eval_run_e2e_test.go`, `//go:build e2e`): the runner + RunStore against a
  REAL enrolled tenant DB with a deterministic fake `eval.Pipeline` — asserts one finished
  `eval_runs`, three `eval_results`, correct `recall_hit` (true/false/NULL), deduped
  `retrieved_doc_ids`, `judged_correct` NULL, and the stored effective config. `go test -tags
  e2e -run TestEvalRunWritePath`: **PASS** (~3.7s). The STORY-12.1 e2e still passes.
- **Build**: `mise run build`: **PASS**. **Lint**: `mise run lint` reports zero issues in the
  files added/changed by this story.

## Not in scope
- LLM-as-judge correctness scoring — STORY-12.3.
- Admin-UI eval report and CI gate — STORY-12.4.
- The 10 pre-existing lint issues on unrelated files — untouched.
