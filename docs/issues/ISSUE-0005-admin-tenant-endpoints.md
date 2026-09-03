# ISSUE-0005: Admin tenant endpoints

**Type:** Feature · **Status:** Done · **Story:** STORY-04.6 · **Traces:** FR-TEN-01/05/07, FR-TEN-04/08, SPEC-07 §2/§2d, ADR-0032

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-04.6 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Implement the SPEC-07 §2 platform-admin tenant surface — `POST /admin/tenants`,
`GET /admin/tenants`, `PATCH /admin/tenants/{id}`, `DELETE /admin/tenants/{id}` —
under `/admin` behind `RequireSession → RequirePlatformAdmin` with CSRF on the
mutations. This completes EPIC-04 (18 → 21 pts).

## Scope
- New HTTP layer in `internal/cp/tenants` (`AdminService`, `AdminHandlers`,
  `AdminPoolStore`) that orchestrates the **existing** backend — do not rebuild:
  `provision.Provisioner` (enrol, STORY-02.3), `provision.Lifecycle`
  (suspend/resume/move/schedule-delete, STORY-02.4/02.5), and
  `tenants.SettingsService` (FR-TEN-08).
- Wire the four routes into `internal/api` (`New` + `liveRoutes()`) behind the
  platform-admin guard; regenerate `api/openapi.yaml`.

## Resolution
- **Create** runs `Provisioner.Provision` synchronously (ADR-0016 precedent; async
  River `provision_tenant` execution is EPIC-09) and records a `provision_tenant`
  mirror row, returning `{tenant, job_id}` — `201`.
- **List** — `?limit&cursor` → `{items,next_cursor}` keyset pagination on
  `(created_at, id)` over the control-plane pool (C-3); the view carries no
  connection secrets (C-4).
- **Update** — routes each present sub-change to the owning service: `settings` →
  `SettingsService.Patch` (FR-TEN-08), `connection` → `Lifecycle.Move`
  (FR-TEN-07), `status` → `Lifecycle.Suspend`/`Resume` (FR-TEN-04). Unknown id →
  `404` before any write; illegal transition / immutable settings → `409`. No
  duplicate audit — each sub-service audits its own action.
- **Delete** — `Lifecycle.ScheduleDelete` with `?grace` (default 7 days): status →
  `deleting`, `delete_after` set, `202` (FR-TEN-05).
- Platform scope: NOT tenant-scoped (a platform admin acts across tenants;
  FR-ACC-03 governs the tenant-scoped `/v1` surface). Errors use the SPEC-07 §1
  envelope (ADR-0027). `provision.ErrValidation`/`ErrIllegalTransition` exported
  (additive) so the handlers map failures to the right status with `errors.Is`.
- Design decisions recorded in ADR-0032. No schema or migration change (the
  `tenants`/`tenant_databases`/`jobs` tables, the `provision_tenant`/`delete_tenant`
  `job_kind` values and `delete_after` already existed), so the drift guard stays
  green.

## Verification
`go test ./...` (unit: `internal/cp/tenants`, `internal/api`, `internal/provision`)
and `mise run openapi` (regenerated the YAML; drift guard green). e2e against the
real control-plane Postgres: `TestAdminTenantsGoldenPath` — provision a real
tenant + role through the mounted router, CSRF-less mutation refused (403),
list, PATCH suspend + settings + resume (DB + audit rows verified), DELETE
schedule-with-grace (status → deleting), and a non-admin session refused (403);
`TestOpenAPIContractGoldenPath`, `TestAPIRouterGoldenPath`, `TestSettingsGoldenPath`,
`TestJobsGoldenPath`, `TestSourcesGoldenPath`, `TestTenantLifecycleGoldenPath`,
`TestTenantMoveGoldenPath` and `TestTenantIsolationSuite` (SPEC-01 §9, re-run
because `internal/api` changed) all still green. Lint clean (`golangci-lint`) on
the touched packages.

## Notes / not in scope
- The async River `provision_tenant`/`delete_tenant` job **execution** (and the
  irreversible teardown after the grace window) is EPIC-09 (ADR-0005). Scheduling
  and synchronous provisioning are the complete actions today.
- Settings/members/api-keys `/v1` routes remain later EPIC-04 work.
- Pre-existing, unrelated to this story (reported, not fixed): `internal/cp/audit/pool.go`
  and `test/e2e/audit_e2e_test.go` each trip one `revive` lint finding, and
  `mise run test`/coverage leaks `CONTROL_PLANE_URL`/`PROVISION_DB_URL` into the
  `internal/cli` "RequireURL" tests (they pass in a clean env). Both pre-date this
  change and no coverage-gated package (tenant/ingest/retrieve/connector) was
  touched.
