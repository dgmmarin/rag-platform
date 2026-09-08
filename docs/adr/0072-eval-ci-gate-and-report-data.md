# ADR-0072: Eval CI gate (minimum-threshold policy) + machine-readable report; admin-UI render deferred to EPIC-11

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** FR-ADM-04, SPEC-06 §8 · **Decisions:** ADR-0003, ADR-0010, ADR-0014, ADR-0070, ADR-0071

## Context
STORY-12.4 is "eval report in admin UI and CI gate for settings changes". The admin UI does not
exist yet — it is all of EPIC-11 — so the "in admin UI" render cannot be built now without
faking a frontend. This ADR records how the story was split: the **CI gate** (the real
engineering) and the **machine-readable report data** (the contract the future UI renders) ship
now; the **admin-UI rendering is deferred to EPIC-11**, recorded honestly in the backlog/issue.
The gate builds on `ragctl eval run` (STORY-12.2/12.3), which already computes and stores the
Summary (recall@k / grounded rate / correctness).

## Options / decisions
- **Gate = committed minimum-threshold policy, not a stored prior-run baseline.** The gate
  compares a run's Summary against minimum acceptable metrics in a committed JSON file
  (`.ci/eval-gate.json`): `min_recall_at_k`, `min_grounded_rate`, optional
  `min_correctness_rate`. A settings/chunking/retrieval change must keep the run at or above the
  floor to land. Committed minimums are the simplest thing that is reviewable, versioned, and
  needs no baseline-run bookkeeping; comparing against a stored prior run adds state and a
  "which run is canonical" question for little gain at this stage.
  - **ponytail:** static floors, not a moving baseline. Ceiling: a floor does not catch a small
    regression that stays above it. Upgrade path: store a baseline run id and add a
    `max_regression` delta if that becomes necessary.

- **The comparison is pure Go, unit-tested; the shell script orchestrates.** `eval.CheckGate(
  summary, policy) GateResult` and `eval.ParseGatePolicy` live in `internal/eval` and are unit-
  tested (thresholds, boundary is inclusive `>=`, a no-threshold policy is rejected, a
  correctness threshold with nothing judged fails). `>=` at the threshold passes. A run with
  **no cases is Skipped** (quality cannot be assessed) — a non-blocking pass. The CLI `eval run
  --gate FILE` runs the comparison and returns a non-zero exit on a real regression (after
  printing the verdict), so a human and CI both get the signal.

- **`mise-tasks/eval-gate` mirrors `vulncheck-gate`/`backup-drill`: parse JSON, self-skip.** It
  runs `ragctl eval run <slug> --json --gate .ci/eval-gate.json`, parses the JSON with `jq`, and
  **blocks ONLY on `gate.passed=false`**. It self-skips (exit 0) when `jq`/`EVAL_GATE_SLUG`/
  `CONTROL_PLANE_URL`/the baseline file are absent, and — critically — when the run produces no
  parseable JSON (no stack, DEK, or provider keys), so a keyless CI runner is never red-walled.
  The infra-failure-vs-regression distinction is made from the JSON: a regression carries a
  `gate` verdict; an infra failure produces none. The CI job wires `EVAL_GATE_SLUG` +
  connection secrets on the environment where a seeded eval tenant exists.

- **Machine-readable report = `--json` + `ragctl eval report <slug> <run-id>`.** `eval run
  --json` emits `{summary, gate?}`; `eval report` emits `{run{id,config,started_at,finished_at,
  summary}, results[{case_id, question, expected_answer, retrieved_doc_ids, recall_hit,
  judged_correct, answer, latency_ms}]}` read from `eval_runs`/`eval_results` (tenant content via
  the resolver, ADR-0003, C-3; `eval_results` LEFT JOINs `eval_cases` so a case deleted since the
  run still shows with a null question). `config`/`summary` are passed through as raw JSON so the
  report never re-shapes what the run stored. This IS the data contract the EPIC-11 admin report
  renders.

- **Admin-UI rendering deferred to EPIC-11.** No HTML/frontend is built (there is no admin UI
  yet). The report endpoint/view over this data is EPIC-11 work; STORY-12.4 is therefore marked
  **partial** — the CI gate + report data shipped; the UI render is carried forward.

## Consequences
- A quality regression from a settings/chunking/retrieval change is caught in CI (where a seeded
  eval tenant is configured), while keyless PR runners self-skip. The gate logic is pure and
  unit-tested; the script is `bash -n`/self-skip-checked like the other mise gates (ADR-0014).
- The eval results are fully consumable without a UI (`--json`, `eval report`), so the EPIC-11
  admin report is a view over a stable, tested data contract — nothing is faked now.
- No schema change (the tables exist); no new dependency (jq is already used by vulncheck-gate).
- STORY-12.4 is partial by design: the admin-UI render remains for EPIC-11.
