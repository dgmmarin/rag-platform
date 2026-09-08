# ISSUE-0050: Runbooks

**Type:** Feature · **Status:** Done · **Story:** STORY-10.8 · **Traces:** SRS §8, SPEC-10 §5, ISSUE-0045 (alert runbook links)

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs.

## Summary
Authors the operational runbooks the AC lists, in `docs/runbooks/` (OKF markdown), reusing
the structure of the existing runbooks. Also closes the STORY-10.2 loose end: the alert
`runbook_url` links now resolve to real headings, guarded by a test so they cannot drift.

## Scope
- New runbooks: `observability.md` (alert → action index, carries the 5 alert anchors),
  `enrol-tenant.md`, `tenant-deletion.md`, `failed-migration.md`, `provider-outage.md`,
  `stuck-job.md`, `incident-response.md`; plus `README.md` (index).
- Reused, not duplicated: `move-tenant.md` (STORY-02) and `backup-and-pitr.md` (STORY-10.5)
  are referenced from the index and cross-linked where relevant.
- Runnable check: `internal/obs/runbook_links_test.go` asserts every alert `runbook_url`
  in `deploy/prometheus/rules/ragctl.rules.yml` resolves to a heading anchor in the target
  runbook — the check that fails if a runbook and an alert drift.
- Docs: this issue, backlog.

## AC → runbook mapping
enrol tenant → `enrol-tenant.md`; move tenant → `move-tenant.md` (existing); failed
migration → `failed-migration.md`; provider outage → `provider-outage.md`; stuck job →
`stuck-job.md`; tenant deletion → `tenant-deletion.md`; incident response →
`incident-response.md`.

## Consistency check
The STORY-10.2 alerts link `docs/runbooks/observability.md#{query-latency, grounded-rate-drop,
job-failures, queue-depth, provider-errors}`. `observability.md` was authored with exactly
those H2 headings so all five anchors resolve; the new test enforces it. No alert filename
had to be changed (they already targeted `observability.md`).

## Tests
- `go test ./internal/obs/ -run AlertRunbookLinksResolve` → PASS (all 5 anchors resolve).
  Runs under `mise run test` / `mise run coverage` (no new CI wiring). No ADR (docs only).

## Not in scope
- Automating incident comms / paging; the runbooks reference the operator's own tooling.
