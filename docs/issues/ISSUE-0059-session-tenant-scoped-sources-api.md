# ISSUE-0059: Session tenant-scoped sources API — `RequireTenantAccess` (STORY-11.2, Task 1)

**Type:** Feature · **Status:** In progress · **Story:** STORY-11.2 · **Traces:** FR-ADM-01, ADR-0075

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs (ADR-0075).

## Summary
STORY-11.2 gives the session admin UI a way to manage a tenant's sources without a Bearer API
key (a platform admin holds none, and a session user should not need one — ADR-0073/ADR-0075).
This is Task 1 of that story: the reusable `RequireTenantAccess` session middleware and the
`/admin/tenants/{tenantId}/sources…` routes, mounted behind it, reusing the EXISTING
`sources.Handlers`/`sources.Service` verbatim (no source CRUD/test logic duplicated between the
Bearer `/v1/sources` surface and this session surface — only the middleware that resolves the
tenant differs).

## Scope (shipped)
- **`internal/cp/auth/tenant_access.go`** — `(*AuthzService).RequireTenantAccess(perm Permission)
  func(http.Handler) http.Handler`. Reads the session (401 if none), parses `{tenantId}` from the
  path as a UUID (invalid → 404), looks up the principal `(role, isPlatformAdmin)` for
  `(tenantId, session.UserID)` via the existing `lookupPrincipal` (shared with `RequireRole`).
  - Platform admin, not a tenant member → allow, cross-tenant access audited
    (`admin.tenant_access`, `details.impersonation=true`, FR-ADM-05) through the same `AuditFunc`
    seam `ImpersonationService.Start` already writes through (`AuthzService.Audit`, wired in
    `api_server.go` to `impSvc.Audit` — one audit sink, not a second `audit.Record` wiring).
  - Platform admin who is ALSO a member → allow, no impersonation audit (not acting across
    tenants in the FR-ADM-05 sense).
  - Member whose role satisfies `perm` (SPEC-02 §4 matrix, `roles.go`) → allow.
  - Member whose role does not satisfy `perm` → 403.
  - Non-member, non-admin, or an unknown/invalid tenant id → 404 (identical response either way,
    so the surface never leaks whether a tenant exists to someone with no claim on it).
  - On allow: injects `tenant.WithTenantID(ctx, id)` — the SAME key `sources.Handlers` already
    read via `tenant.TenantIDFromCtx` (no handler change needed).
  - No new `Permission` constants: read uses the existing `PermQuery` (the SPEC-02 §4 matrix's
    baseline permission every role, including viewer, grants), write uses the existing
    `PermManageSources` (already "manage sources, trigger sync", owner/admin only).
- **`internal/api/router.go`** — `Deps.RequireTenantSourcesRead`/`RequireTenantSourcesWrite`
  (pre-built `Middleware`, mirroring how `RequireRoleAdmin` is one materialized
  `AuthzService.RequireRole(perm)` instance — the `api` package stays free of `cp/auth`'s concrete
  types). Mounts `GET/POST /admin/tenants/{tenantId}/sources`,
  `GET/PATCH/DELETE /admin/tenants/{tenantId}/sources/{id}`,
  `POST /admin/tenants/{tenantId}/sources/{id}/sync`, `POST …/{id}/test` behind
  `RequireSession -> RequireTenantSources{Read,Write}`, CSRF on the mutations
  (`mustCSRF`), none on the GETs (SPEC-09 §3). `{id}` stays the SOURCE id (the handlers' own
  `r.PathValue("id")` is unchanged); the tenant path segment is `{tenantId}` so the two never
  collide.
- **`internal/cli/api_server.go`** — wires the two Deps fields from
  `authz.RequireTenantAccess(auth.PermQuery)` / `authz.RequireTenantAccess(auth.PermManageSources)`,
  and reuses the SAME `sourceHandlers` already built for the Bearer surface.

## Tests / runnable checks
- **`internal/cp/auth/tenant_access_test.go`** (table test over a fake `MembershipDB`, reusing
  `authzMemStore` from `authz_test.go`): no session → 401; invalid tenant id → 404; unknown
  tenant/non-member → 404; member role satisfies write → 200 + ctx tenant id + no audit; member
  role lacks write (viewer) → 403; platform-admin non-member → 200 + ctx tenant id + one
  `admin.tenant_access` audit event with `impersonation=true`; platform-admin who is also a member
  → 200, no audit; audit sink unwired on the cross-tenant path → 500 (fail closed, not a silent
  unauditable grant). RED (`RequireTenantAccess` undefined) confirmed, then GREEN, both captured.
  `go test ./internal/cp/auth/ -run TenantAccess -v`: **PASS** (9/9).
- **`internal/api/router_test.go`**: `TestTenantSourcesRoutesChain` (all 7 routes, session ->
  read/write gate order, correct handler reached) + `TestTenantSourcesCSRF` (mutation blocked
  without CSRF, GET unaffected). `go test ./internal/api/`: **PASS**.
- **Build/lint**: `mise run build` PASS; `go build ./...`, `go vet ./...`, `go test ./...` all
  PASS; `mise run lint` — 10 issues, unchanged from baseline (0 new).

## Out of scope (later STORY-11.2 tasks / later stories)
- The admin UI screens that call these routes, and the `GET /admin/connector-kinds` schema
  endpoint (ADR-0075) — later tasks in this story.
- Mounting `jobs`/`documents`/members/settings behind `RequireTenantAccess` — STORY-11.3–11.6
  reuse the same middleware, added there.
