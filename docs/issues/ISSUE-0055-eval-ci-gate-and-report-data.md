# ISSUE-0055: Eval CI gate + machine-readable report (STORY-12.4, partial)

**Type:** Feature · **Status:** Partial (CI gate + report data done; admin-UI render deferred to EPIC-11) · **Story:** STORY-12.4 · **Traces:** FR-ADM-04, SPEC-06 §8

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs (ADR-0072).

## Summary
STORY-12.4 is "eval report in admin UI **and** CI gate for settings changes". The admin UI does
not exist yet (all of EPIC-11), so this issue ships the two halves that can be built honestly
now and defers the UI render:
- **CI gate** for settings/chunking/retrieval changes — fully built.
- **Machine-readable report** — the data contract the EPIC-11 admin report will render.
- **Deferred to EPIC-11:** rendering that report data in the admin UI (see "Deferred" below).

## Scope (shipped)
- **`internal/eval/gate.go`** — pure `GatePolicy` / `GateResult`, `ParseGatePolicy` (rejects a
  threshold-less policy), `CheckGate` (minimum thresholds, inclusive `>=`, no-cases → skip,
  correctness threshold with nothing judged → fail).
- **`internal/eval/report.go`** — `RunView`/`ResultView`/`Report`, `RunStore.GetRun` +
  `RunStore.Results` (LEFT JOIN `eval_cases`), `scanReportResult`; `Service.Report(ctx, tid,
  runID)`. Reads tenant content via the resolver (ADR-0003, C-3).
- **`internal/cli/eval_run.go`** — `--json` (emit `{summary, gate?}`) and `--gate FILE` (compare
  against the policy; non-zero exit on regression after printing the verdict).
- **`internal/cli/eval_report.go`** — `ragctl eval report <slug> <run-id>` prints the report JSON.
- **`mise-tasks/eval-gate`** — runs `ragctl eval run --json --gate .ci/eval-gate.json`, parses
  JSON with `jq`, blocks only on a real regression, self-skips without stack/keys/seeded data
  (mirrors `vulncheck-gate`/`backup-drill`). **`.ci/eval-gate.json`** — committed minimum
  thresholds. **`.github/workflows/ci.yml`** — an `eval-gate` job.

## Decisions (ADR-0072)
- Gate = committed minimum thresholds (not a stored prior-run baseline) — simplest, versioned,
  reviewable. Comparison is pure Go, unit-tested; the shell script orchestrates + self-skips.
- Report format = `{run{...}, results[...]}` from `eval_runs`/`eval_results`; `config`/`summary`
  passed through as raw JSON.

## Tests / runnable checks
- **Unit** (`internal/eval`): `ParseGatePolicy` (valid, empty rejected, bad JSON), `CheckGate`
  (pass, below-threshold with named metrics, skip-on-no-cases, correctness cases, inclusive
  boundary); `scanReportResult` (populated + all-nullable). `mise run test`: **PASS**.
- **CLI** (`internal/cli`): `eval report` added to `TestEvalCommandsRequireURL` (fail-closed).
- **e2e** (`test/e2e/eval_run_e2e_test.go`, extended): `Service.Report` reads the stored run +
  results (question enriched via join, judged_correct, finished_at) and an unknown run id →
  ErrNotFound. `go test -tags e2e -run TestEvalRunWritePath`: **PASS**.
- **Gate script**: `bash -n mise-tasks/eval-gate` OK; demonstrated self-skip (exit 0) with
  `EVAL_GATE_SLUG`/`CONTROL_PLANE_URL` absent and when the run yields no JSON; the fail→exit-1
  path is the `CheckGate` unit tests plus the jq verdict mapping.
- **Build/lint**: `mise run build` PASS; `mise run lint` zero issues in new/changed files (10
  known pre-existing elsewhere untouched).

## Deferred to EPIC-11 (admin UI)
- Rendering the eval report data (`ragctl eval report` / the `Report` JSON) in the admin UI: a
  run list, a per-run drill-down (recall@k / grounded / correctness + per-case rows), and a
  before/after settings-change comparison view. The data layer is complete and stable, so the
  EPIC-11 story is a view over it. This should be picked up as an EPIC-11 admin-UI story
  (reference: this issue + ADR-0072); STORY-12.4 stays **partial** until then.
