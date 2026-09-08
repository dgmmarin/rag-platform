# ISSUE-0054: LLM-as-judge correctness scoring (STORY-12.3)

**Type:** Feature · **Status:** Done · **Story:** STORY-12.3 · **Traces:** FR-ADM-04, SPEC-06 §8

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs (ADR-0071).

## Summary
Add optional LLM-judged answer correctness to `ragctl eval run` (the `--judge` flag). When on,
each case with a non-empty `expected_answer` has its produced answer scored correct/incorrect by
an LLM, populating `eval_results.judged_correct` and a correctness rate in the run summary. Off
by default: a plain run is unchanged (no judging, no judge cost, `judged_correct` NULL). Admin
report / CI gate is STORY-12.4 (not built).

## Scope
- **`internal/eval` (extended):**
  - `run.go` — the `Judge` port (optional on `Runner`), `CaseResult.JudgedCorrect`, summary
    `CasesJudged`/`CorrectnessRate` (omitempty), and the runner rule: judge only cases with an
    expected answer and a successful pipeline; a judge error/unparseable verdict → NULL
    (fail-soft), never a silent pass.
  - `judge.go` — `llmJudge` over `llm.Provider`: the rubric/system prompt, the delimited
    prompt builder (inputs as data — SPEC-09 §2), and the strict deterministic verdict parser
    (`\bINCORRECT\b` before `\bCORRECT\b`; neither → error).
  - `runstore.go` — `RecordResult` now writes `judged_correct` (`*bool` → NULL when unscored).
- **`internal/cli/eval_run.go`** — `--judge` / `--judge-model` flags; when `--judge` is set,
  builds the judge provider through the existing `llm.Factory` (fail-closed on the tenant's
  provider + model allowlists), passes it into the run, and prints the correctness line only
  when cases were judged.

## Decisions (ADR-0071)
- Opt-in; default run byte-identical to 12.2. Judge model = tenant `settings.llm.model`
  (or `--judge-model`), fail-closed on the allowlists. Strict one-word `CORRECT`/`INCORRECT`
  verdict; unparseable = error = NULL. A case with no expected answer, or a judge failure,
  yields NULL and is excluded from the correctness denominator.

## Tests / runnable checks
- **Unit** (`internal/eval`): `parseVerdict` (CORRECT/INCORRECT, case-insensitive, embedded in
  a sentence, empty/ambiguous → error, "CORRECTNESS" → error), `buildJudgePrompt` (contains +
  labels the three inputs), `llmJudge` against a fake `llm.Provider` (verdict, model/temperature
  set, unparseable → error, provider error → error). Runner: `TestRunnerRunWithJudge`
  (correct/incorrect/no-expected/judge-error → NULL; correctness rate) and
  `TestRunnerRunNoJudgeLeavesNull` (default unchanged). `mise run test`: **PASS**.
- **e2e** (`test/e2e/eval_run_e2e_test.go`, extended): a judged run with a fake `eval.Judge`
  (no LLM keys) asserts `judged_correct` persisted true/false, NULL for a case with no expected
  answer, `cases_judged`/`correctness_rate` in the returned + stored summary. `go test -tags
  e2e -run TestEvalRunWritePath`: **PASS** (~3.7s).
- **Build**: `mise run build`: **PASS**. **Lint**: `mise run lint` — zero issues in new/changed
  files (the 10 known pre-existing elsewhere stay).

## Not in scope
- Admin-UI eval report and CI gate — STORY-12.4.
- Multi-sample / self-consistency judging, numeric rubric, per-tenant rubric override
  (ponytail upgrade paths in ADR-0071).
