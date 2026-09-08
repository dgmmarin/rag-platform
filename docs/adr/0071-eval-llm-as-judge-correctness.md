# ADR-0071: LLM-as-judge correctness — opt-in, strict single-word verdict, fail-soft to NULL

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** FR-ADM-04, SPEC-06 §8 · **Decisions:** ADR-0003, ADR-0070, ADR-0022 (settings)

## Context
STORY-12.3 adds optional LLM-judged answer correctness to `ragctl eval run` (FR-ADM-04). The
runner (ADR-0070) already produces an answer per case and left the seam: `judged_correct` NULL
and no correctness figure in the summary. This story fills it — when enabled, each case with an
`expected_answer` has its produced answer scored correct/incorrect by an LLM, populating
`eval_results.judged_correct` and a correctness rate in the run summary. The `judged_correct`
column already exists (nullable). LLM-as-judge is inherently noisy, so the design keeps it
opt-in, deterministic to parse, and conservative on any doubt.

## Options / decisions
- **Opt-in via `--judge`; default run is byte-identical to STORY-12.2.** Without the flag no
  judge is constructed, no judge LLM call is made, `judged_correct` stays NULL, and the summary
  omits the correctness fields (they are `omitempty`). Judging costs money and adds noise, so it
  is never on by default.

- **A `Judge` port on the runner (nil = no judging).** `Judge.Judge(ctx, question, expected,
  actual) (correct bool, err error)`. The runner calls it ONLY for cases with a non-empty
  `expected_answer` and only when the answer pipeline succeeded — a case with no ground truth
  keeps `judged_correct` NULL (correctness is undefined without an expected answer). The
  production `llmJudge` lives in `internal/eval` (its prompt + parser are core harness logic,
  unit-tested against a fake `llm.Provider`); the CLI builds the `llm.Provider` and injects it.

- **Fail-soft per case → NULL, never a silent pass.** A judge error OR an unparseable verdict
  is surfaced as an error, and the runner leaves that case's `judged_correct` NULL and
  continues. The harness never guesses "correct": an unscored case is excluded from the
  correctness denominator rather than counted as a pass.

- **Rubric + strict, machine-parseable verdict.** The system prompt tells the judge to compare
  the ACTUAL answer against the EXPECTED answer (paraphrase/extra correct detail OK; a
  contradiction, a missing key fact, or a refusal = INCORRECT) and to reply with exactly one
  word: `CORRECT` or `INCORRECT`. The three inputs are rendered as clearly delimited DATA
  (`<<< … >>>`) so they are compared, not executed (SPEC-09 §2 prompt-injection defence). The
  parser is deterministic: `\bINCORRECT\b` is checked BEFORE `\bCORRECT\b` (the word boundary
  keeps "CORRECT" from matching inside "INCORRECT"); anything with neither standalone word is
  an error. Temperature 0 where the provider honours it. `MaxTokens` is a small fixed cap.

- **Judge model selection.** The judge reuses the tenant's `settings.llm` provider/model,
  overridable with `--judge-model`, built through the SAME `llm.Factory` the answer path uses —
  so it is fail-closed on the tenant's `providers_allowed` and `llm.models_allowed` (a judge
  model outside the allowlist is refused, SPEC-09 §2). An unset provider/model (none in
  settings, no override) is an actionable error, not a silent default.

- **Correctness in the summary.** `cases_judged` = cases with a non-NULL verdict (the
  denominator); `correctness_rate` = judged-correct / cases_judged. Both are stored in
  `eval_runs.summary` and printed — only when judging actually scored cases. Grounded rate,
  recall@k and latency are unchanged.

## Consequences
- An operator can get a correctness signal alongside recall@k/grounded/latency for tuning,
  while a plain run stays cheap and unchanged. Correctness is measured only where there is
  ground truth; noise and judge failures degrade to "not scored", never to a false positive.
- No schema change; the write path reuses `RunStore.RecordResult` (now also writing
  `judged_correct`). The judge reuses the existing `llm.Provider`/`llm.Factory` seam — no new
  client, no change to the answer path.
- **ponytail:** a single judge call per case, no self-consistency / multi-sample voting.
  Ceiling: verdict noise on borderline answers. Upgrade path: majority vote over k samples, or
  a numeric rubric, if a tenant needs tighter agreement.
- **ponytail:** a fixed judge `MaxTokens` cap (256). Ceiling: a very verbose judge model could
  be truncated before the verdict word — the parser then errors and the case is NULL (fail-soft,
  never mis-scored). Upgrade path: make the cap configurable.
- **ponytail:** the rubric is a general semantic-equivalence check, not domain-specific.
  Ceiling: it may be too strict/lenient for specialised answers. Upgrade path: a per-tenant
  rubric override in settings.
