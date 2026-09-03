# ISSUE-0002: Sources endpoints

**Type:** Feature · **Status:** Done · **Story:** STORY-04.3 · **Traces:** FR-SRC-01, FR-SRC-14, SPEC-07 §2, ADR-0029

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-04.3 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Implement the SPEC-07 §2 sources surface — `GET/POST /v1/sources`,
`GET/PATCH/DELETE /v1/sources/{id}`, `POST /v1/sources/{id}/sync`,
`POST /v1/sources/{id}/test` — with the tenant derived from the authenticated
principal (FR-ACC-03), CRUD + pause/resume (FR-SRC-01), manual sync with a 409
concurrent-sync guard and Idempotency-Key, and "test connection" (FR-SRC-14).

## Scope
- New `internal/cp/sources` package: `Store` (control-plane PoolDB), `Service`,
  `Handlers`. Sources are control-plane registry data (C-3); this path never
  opens a tenant DB (ADR-0003).
- Wire the seven routes into `internal/api` (`New` + `liveRoutes()`) behind
  `RequireScopeAdmin -> RateLimit`; regenerate `api/openapi.yaml`.
- 409 on concurrent sync via the existing `jobs_one_active_sync_per_source`
  index; Idempotency-Key replayed via `jobs.payload`; delete marks the source
  `deleting` and enqueues `delete_source`.

## Resolution
- CRUD, list (`?limit&cursor` -> `{items,next_cursor}`), sync and delete are
  fully implemented against the control-plane `sources`/`jobs` tables.
- The connector framework (`Validator`: `ValidateConfig`/`Test`) and the job
  worker are injected seams provided by later epics: EPIC-06 (STORY-06.1) wires
  the `Validator` (until then `/test` returns the not_found seam and create/update
  run generic validation only); EPIC-09 (STORY-09.1/09.6) supplies the worker
  that executes the queued jobs and performs the FR-SRC-12 cascade.
- Credential handling is deferred to EPIC-06 (STORY-06.2): a `credentials` field
  in the body is rejected 400 (fail closed, C-4). No API response ever returns
  credentials (FR-SRC-10).
- Design decisions and the deferrals are recorded in ADR-0029. No schema or
  migration change (the tables and the unique index already existed), so the
  drift guard stays green.

## Verification
`mise run test` (unit: `internal/cp/sources`, `internal/api`), `mise run openapi`
(regenerated the YAML; drift guard green), and `mise run e2e`
(`TestSourcesGoldenPath` over the real control-plane Postgres — create/list/get/
patch/sync/idempotency/409/delete/test-seam; `TestOpenAPIContractGoldenPath` and
`TestAPIRouterGoldenPath` still green). Lint clean on the touched packages. See
the STORY-04.3 delivery note in `docs/backlog/BACKLOG_STATUS.md`.

## Notes / not in scope
- The coverage gate and full `mise run lint` are red in the current local
  environment for reasons that pre-date and are unrelated to this change (a
  golangci-lint/Go-toolchain drift flagging two EPIC-03 files, and env-sensitive
  `internal/cli` "RequiresURL" tests under mise's env injection). Verified
  identical on a clean checkout; no gated package (tenant/ingest/retrieve/
  connector) was touched by this story.
