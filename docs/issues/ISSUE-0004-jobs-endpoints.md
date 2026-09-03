# ISSUE-0004: Jobs endpoints

**Type:** Feature · **Status:** Done · **Story:** STORY-04.5 · **Traces:** FR-ADM-02, SPEC-07 §2, SPEC-08 §3/§4, ADR-0031

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-04.5 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Implement the SPEC-07 §2 jobs surface — `GET /v1/jobs`, `GET /v1/jobs/{id}`,
`POST /v1/jobs/{id}/cancel` — with the tenant derived from the authenticated
principal (FR-ACC-03), list with status/kind/source filters and keyset
pagination, get with status/duration/statistics, and cancel per SPEC-08 §4
(FR-ADM-02).

## Scope
- New `internal/cp/jobs` package: `Store` (control-plane `PoolDB`), `Service`,
  `Handlers`. Jobs are the control-plane history/mirror view (C-3, ADR-0005);
  this path uses the control-plane pool and never opens a tenant DB (ADR-0003),
  mirroring `internal/cp/sources` (ADR-0029).
- Wire the three routes into `internal/api` (`New` + `liveRoutes()`) behind
  `RequireScopeAdmin -> RateLimit`; regenerate `api/openapi.yaml`.

## Resolution
- List (`?status&kind&source&limit&cursor` -> `{items,next_cursor}`), get, and
  **cancel of a queued job** are fully implemented against the control-plane
  `jobs` table. A queued job is cancelled in one guarded SQL statement
  (`... where status='queued'`), effective now — the mirror row is authoritative
  when no worker holds it (SPEC-08 §4). `duration_ms` is computed for finished
  jobs; `stats`, `attempt` and timing are surfaced (FR-ADM-02).
- **Cancel state machine:** queued → 200 cancelled; running → 202 (cancellation
  requested; worker finalises) when the `Canceller` is wired, else the
  `not_found` seam today; succeeded/failed → 409 conflict; already-cancelled →
  idempotent 200.
- The running-job cancel is a cooperative River operation (SPEC-08 §4) and River
  is EPIC-09, so it is an injected `Canceller` seam (nil today → `not_found` seam
  envelope, mirroring STORY-04.3 `/test` and STORY-04.4 upload). The worker-side
  honouring (observe `ctx.Done()` between documents, write `running`→`cancelled`
  per SPEC-08 §3) is EPIC-09 (STORY-09.1/09.4).
- Design decisions and the deferral are recorded in ADR-0031. No schema or
  migration change (the `jobs` table, its enums, and the
  `(tenant_id, queued_at desc)` index already existed), so the drift guard stays
  green.

## Verification
`mise run test` (unit: `internal/cp/jobs`, `internal/api`, `internal/cli`),
`mise run openapi` (regenerated the YAML; drift guard green), and `mise run e2e`
(`TestJobsGoldenPath` over the real control-plane Postgres — list/filter/get/
cancel-queued/cancel-terminal-409/cancel-running-seam + FR-ACC-03 cross-tenant
isolation; `TestOpenAPIContractGoldenPath`, `TestAPIRouterGoldenPath`,
`TestSourcesGoldenPath`, `TestDocumentsGoldenPath` and `TestTenantIsolationSuite`
— SPEC-01 §9, re-run because `internal/api` changed — all still green). Lint clean
(`golangci-lint`) on the touched packages. See the STORY-04.5 delivery note in
`docs/backlog/BACKLOG_STATUS.md`.

## Notes / not in scope
- The job worker (River) that executes jobs, honours a running-job cancel signal,
  and writes the mirror-row transitions is EPIC-09 (ADR-0005, SPEC-08 §3).
- Auditing the cancel action (FR-ADM-05) rides EPIC-09 with the rest of the job
  lifecycle, consistent with the STORY-03.6 plan ("job.cancel in EPIC-09").
- Pre-existing, unrelated to this story: `test/e2e/audit_e2e_test.go` trips one
  `revive` context-as-argument lint finding, and the `internal/cli` mise coverage
  run leaks env; both pre-date this change and no gated package
  (tenant/ingest/retrieve/connector) was touched.
