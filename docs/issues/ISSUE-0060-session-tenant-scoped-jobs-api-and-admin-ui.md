# ISSUE-0060: Session tenant-scoped jobs API + admin UI jobs screens (STORY-11.3)

**Type:** Feature · **Status:** Done · **Story:** STORY-11.3 · **Traces:** FR-ADM-02, ADR-0075, ADR-0031

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs
> (ADR-0075 session tenant-scoped API, ADR-0031 jobs cancel semantics).

## Summary
STORY-11.3 gives the session admin UI a jobs view: list jobs with status, duration and
statistics, open one for detail, and cancel a queued job (FR-ADM-02). It reuses the
`RequireTenantAccess` middleware and the EXISTING `jobs.Handlers`/`jobs.Service` verbatim — the
same reuse pattern STORY-11.2 established for sources (ISSUE-0059), so no jobs CRUD/cancel logic is
duplicated between the Bearer `/v1/jobs` surface and this session surface. No new ADR: this applies
ADR-0075 (auth) + ADR-0031 (cancel semantics).

## Scope
- **Task 1 — server mount** (`internal/api/router.go` + `router_test.go`): mount
  `GET /admin/tenants/{tenantId}/jobs`, `GET /admin/tenants/{tenantId}/jobs/{id}`,
  `POST /admin/tenants/{tenantId}/jobs/{id}/cancel` behind `RequireSession -> RequireTenantAccess`,
  reusing the existing permission gates — reads via `RequireTenantSourcesRead` (`PermQuery`, any
  role), cancel via `RequireTenantSourcesWrite` (`PermManageSources`, owner/admin) with CSRF. `{id}`
  is the job id; `{tenantId}` is the tenant path segment. No api_server wiring change — `JobList`/
  `JobGet`/`JobCancel` Deps are already populated for the Bearer surface.
- **Task 2 — jobs list (web)**: `web/lib/jobs.ts` client + `useJobs()` hook; `web/app/admin/jobs`
  page + `JobsTable` (status, kind, source, duration, attempt, queued/finished, cancel action on a
  queued job); filters by status/kind.
- **Task 3 — job detail + cancel (web)**: `web/app/admin/jobs/[id]` detail page (stats, error,
  timing) with a cancel action for a cancellable job.

## Out of scope (later stories)
- Documents/members/settings behind `RequireTenantAccess` — STORY-11.4–11.6.
- Live job progress streaming — the list/detail read the control-plane mirror (C-3); refresh is a
  refetch, not a socket.

## Tests / runnable checks
- **`internal/api/router_test.go`**: `TestTenantJobsRoutesChain` (all 3 routes: session → read/write
  gate order, correct handler reached) + `TestTenantJobsCSRF` (cancel blocked without CSRF, GET
  unaffected). RED confirmed (404, routes unmounted) then GREEN. `go test ./internal/api/`: **PASS**;
  `go build ./...`: **PASS**.
- **web (`web/`, vitest + Testing Library, TDD)**: `web/components/JobsTable.test.tsx` (columns,
  empty state, cancel-visibility on queued/running only, row link + cancel wiring, busy disable) and
  `web/components/JobDetail.test.tsx` (cancel wiring, cancel hidden on terminal, busy disable,
  known+unknown stats, `errors[]`). `cd web && npx vitest run`: **PASS** (43/43, +10 new); `npm run
  build`: clean, routes `/admin/jobs` + `/admin/jobs/[id]` present.
