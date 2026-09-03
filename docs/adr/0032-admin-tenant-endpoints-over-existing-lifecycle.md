# ADR-0032: Admin tenant endpoints — thin HTTP layer over the existing lifecycle

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** FR-TEN-01/05/07, FR-TEN-04/08, FR-ACC-03, C-3, C-4, SPEC-07 §1/§2 · **Decisions:** ADR-0003, ADR-0005, ADR-0016, ADR-0017, ADR-0022, ADR-0027, ADR-0028

## Context
STORY-04.6 must deliver the SPEC-07 §2 platform-admin tenant surface:
`POST /admin/tenants` (enrol), `GET /admin/tenants` (list),
`PATCH /admin/tenants/{id}` (status, db connection, settings) and
`DELETE /admin/tenants/{id}` (schedule deletion with grace). It traces
FR-TEN-01/05/07.

The backend already exists and is deliberately not to be rebuilt:

- **Provisioning** — `provision.Provisioner.Provision` (STORY-02.3, ADR-0016)
  creates the least-privilege role + database, records the encrypted connection,
  applies the tenant migrations and sets the tenant active. It is idempotent and,
  per ADR-0016, runs **synchronously** because the async River `provision_tenant`
  job is EPIC-09; `ragctl enroll` is the sole caller today and runs it directly.
- **Lifecycle** — `provision.Lifecycle` (STORY-02.4/02.5, ADR-0017) owns
  `Suspend`/`Resume` (FR-TEN-04), `ScheduleDelete`/`CancelDelete`/`RunDelete`
  with a grace window (FR-TEN-05) and `Move` (FR-TEN-07). STORY-02.5 explicitly
  deferred the `PATCH /admin/tenants/{id}` HTTP route to STORY-04.6.
- **Settings** — `tenants.SettingsService.Patch` (STORY-03.x, ADR-0022) validates
  the settings document against its JSON Schema and holds embedding.dim immutable
  (FR-TEN-08).

Each of these already writes its own audit event (`tenant.create`/`tenant.provision`,
`tenant.suspend`/`tenant.resume`, `tenant.move`, `tenant.delete.schedule`,
`settings.update`) into the control-plane audit log (C-3).

The routers, the SPEC-07 §1 error envelope and the code-derived OpenAPI already
exist (STORY-04.1/04.2, ADR-0027/0028), and the platform-admin surface already
sits behind `RequireSession → RequirePlatformAdmin` with CSRF on mutations.

## Decision
Add a thin HTTP layer in `internal/cp/tenants` (`AdminService` + `AdminHandlers`
+ `AdminPoolStore`) that orchestrates the existing provisioner, lifecycle and
settings service — no lifecycle logic is re-implemented. The four routes are
wired into `internal/api` (`New` + `liveRoutes()`) behind the existing
platform-admin guard, with CSRF on the mutations, and `api/openapi.yaml` is
regenerated.

1. **Platform scope, not tenant-scoped.** These routes live under `/admin`,
   require `is_platform_admin`, and take the tenant as a route/body value. A
   platform admin operates *across* tenants, so there is no per-request tenant to
   derive — FR-ACC-03 governs the tenant-scoped `/v1` surface, not the platform
   surface. The registry reads use the control-plane pool and open no tenant
   database (ADR-0003, C-3). The returned tenant view carries no connection
   details or secrets (C-4).

2. **`POST /admin/tenants` runs the synchronous provisioner and records a
   `succeeded` mirror row.** Following the ADR-0016 precedent exactly (no second
   execution path is invented), provisioning runs synchronously; the tenant is
   active before the response returns. To satisfy SPEC-07 §2 ("returns tenant +
   job id") a `provision_tenant` row is written to the control-plane `jobs`
   table — the history/mirror view (ADR-0005) — as `succeeded`, because the work
   is genuinely done. A perpetually-`queued` placeholder that no worker will run
   was rejected as a fake (AGENTS.md Integrity). When EPIC-09 wires River, this
   becomes a real async enqueue (a `queued` row consumed by a `provision_tenant`
   worker) and the synchronous call is removed.

3. **`PATCH` fans out to the owning service.** `settings` → `SettingsService.Patch`,
   `connection` → `Lifecycle.Move`, `status` → `Lifecycle.Suspend`/`Resume`. The
   tenant is resolved by id first, so an unknown id is `404` before any write. No
   audit is added — each sub-service already audits its own action.

4. **`DELETE` schedules with grace.** `Lifecycle.ScheduleDelete` (default 7 days,
   `?grace` overrides) is the complete action today; the irreversible teardown is
   the EPIC-09 River `delete_tenant` job (ADR-0005), so `DELETE` does not enqueue
   one prematurely.

5. **Exported error sentinels for HTTP status mapping.** `provision.ErrValidation`
   and `provision.ErrIllegalTransition` are exported (aliases of the existing
   unexported sentinels) so the handlers map a lifecycle failure to the right
   SPEC-07 §1 status (400 vs 409) with `errors.Is`, without string matching. This
   is additive and changes no existing behaviour.

## Consequences
- The lifecycle keeps a single implementation and a single audit trail; the HTTP
  layer is orchestration only. Unit-testable with fakes for provisioner/lifecycle/
  store (no real Postgres), plus an e2e golden path that provisions a real tenant
  through the mounted router.
- The `AdminService` carries a configurable `SSLMode` (from `TENANT_DB_SSLMODE`),
  mirroring `ragctl enroll --db-ssl-mode`, so a local non-TLS cluster provisions
  with `disable` while production defaults to `require`.
- No schema or migration change: the `tenants`/`tenant_databases`/`jobs` tables,
  the `provision_tenant`/`delete_tenant` `job_kind` values and `delete_after`
  already exist, so the schema-drift guard stays green.
- The **only** deferred behaviour is the async River provision/delete *execution*
  (EPIC-09). Every one of the four routes reaches its DoD today.

## Alternatives considered
- **Write a `queued` provision job instead of `succeeded`** — rejected: with no
  worker and provisioning already done synchronously, a queued row that never
  runs is a lie about system state.
- **A separate `internal/api` handler package** — rejected: the provisioner,
  lifecycle and settings service all live behind `internal/cp/tenants`/`provision`,
  so co-locating the admin surface with the settings surface (same package) keeps
  the wiring minimal and consistent with STORY-04.3/04.5 (`internal/cp/sources`,
  `internal/cp/jobs`).
- **Enqueue a `delete_tenant` job on `DELETE`** — deferred: `ScheduleDelete` is
  the complete action now; the teardown enqueue belongs with the EPIC-09 worker.
