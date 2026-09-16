# ISSUE-0059: Session tenant-scoped sources API + admin UI sources screens (STORY-11.2)

**Type:** Feature · **Status:** Done · **Story:** STORY-11.2 · **Traces:** FR-ADM-01, ADR-0075

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs (ADR-0075).

## Summary
STORY-11.2 gives the session admin UI a way to manage a tenant's sources without a Bearer API
key (a platform admin holds none, and a session user should not need one — ADR-0073/ADR-0075).
The server side (Tasks 1-2) is the reusable `RequireTenantAccess` session middleware and the
`/admin/tenants/{tenantId}/sources…` routes mounted behind it, reusing the EXISTING
`sources.Handlers`/`sources.Service` verbatim (no source CRUD/test logic duplicated between the
Bearer `/v1/sources` surface and this session surface — only the middleware that resolves the
tenant differs), plus the platform-global `GET /admin/connector-kinds` form-schema endpoint. The
admin UI side (Tasks 3-4) is the sources list page and the schema-driven create/edit form with
test-connection that consume those routes.

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

### Task 2: `GET /admin/connector-kinds` schema endpoint (SPEC-11 §10, ADR-0075)
- **`internal/connector/connector.go`** — `FieldSpec{Name, Label, Type, Required}` (Type one of
  `text|url|number|secret|bool`) and a `Fields() []FieldSpec` method on the `Connector` interface,
  implemented by each connector (`upload`, `webcrawl.webCrawlConnector`,
  `webcrawl.sitemapConnector`, `api.apiConnector`) alongside its existing `ValidateConfig` — the
  same file, so a field and its enforcement are reviewed together. `Required:true` fields mirror
  exactly the connector's own JSON-Schema `required` keys (the ONLY thing the drift guard checks);
  the API connector's credential fields (`api_key`/`token`/`username`/`password`/`client_id`/
  `client_secret`, reusing its existing `credKey*` constants) are listed `Type:"secret",
  Required:false` — they live in the separate `credentials` map (SPEC-04 §6), never in `config`,
  so `ValidateConfig` never sees them and they are never drift-guarded as required.
- **`internal/connector/registry.go`** — `Registry.Schemas() []KindSchema{Kind, Label, Fields}`,
  one entry per REGISTERED kind (sorted): `s3` (no connector built yet) is simply absent, same as
  `Lookup`/`ValidateConfig` already defer for it — no code changes elsewhere once it registers
  (NFR-MNT-01).
- **`internal/connector/handler.go`** (new) — `Handlers{Registry}.List` serves
  `{ "kinds": [ {kind,label,fields:[{name,label,type,required}]} ] }`; DESCRIPTORS only, never a
  secret VALUE.
- **`internal/api/router.go`** — `Deps.ConnectorKinds`; `GET /admin/connector-kinds` mounted behind
  `RequireSession` ONLY — platform-global (no `RequireTenantSourcesRead`/`RequirePlatformAdmin`, no
  tenant path segment), no CSRF (a GET), mirroring `GET /v1/auth/me`'s session-only mount.
- **`internal/cli/api_server.go`** — wires `connector.NewHandlers(connector.DefaultRegistry())`, the
  SAME registry `SourcesValidator` resolves against, so a kind's advertised schema and its actual
  enforcement can never point at two different registries.

### Task 3-4: Admin UI sources screens (SPEC-11 §10.1, web)
- **`web/lib/sources.ts`** — typed client over the Task-1 routes (`listSources`/`getSource`/
  `createSource`/`updateSource`/`deleteSource`/`syncSource`/`testSource`) + a `useSources()` query
  hook, all through `apiFetch` (same-origin BFF, CSRF from `useAuth().me.csrf_token` on mutations).
  Carries the `Source`, `SourceInput` (incl. a `credentials` map), `ConnectorKind`/`ConnectorField`
  types and `listConnectorKinds()`.
- **`web/app/admin/sources/page.tsx` + `web/components/SourcesTable.tsx`** — the list page (loading
  skeleton / error / empty states) and a presentational table (name, kind, status, last-sync,
  next-sync, error summary) with sync/test/edit/delete row actions. Replaces the `[section]`
  placeholder for `/admin/sources` (the dynamic route is untouched).
- **`web/components/SourceForm.tsx` + `web/lib/connectorKinds.ts` + `new`/`[id]/edit` route pages**
  — a schema-driven create/edit form: the kind picker renders exactly the selected kind's
  `GET /admin/connector-kinds` fields by type (text/url/number/secret→password/bool→checkbox),
  splits non-secret fields into `config` and `secret` fields into the `credentials` map (SPEC-04
  §6), and offers an inline test-connection (edit mode only — a source id must exist). Secret
  fields are write-only: blank on edit, and a blank secret is omitted so it stays unchanged; the
  kind is immutable (read-only) on edit.

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
- **Task 2 — `internal/connector/registry_test.go`/`handler_test.go`/`kinds_test.go`**:
  `Registry.Schemas()` lists only registered kinds, sorted, with their fields (RED: `Schemas`
  undefined, confirmed, then GREEN); `Handlers.List` serializes the JSON shape; the drift guard
  instantiates the REAL `upload`/`webcrawl` (web_crawl + sitemap)/`api` connectors, builds a valid
  baseline config per kind, and for every `Required:true` `FieldSpec` removes exactly that key and
  asserts `ValidateConfig` now rejects it — verified it has teeth by transiently marking a
  non-enforced field `Required:true` (`web_crawl.max_depth`) and confirming the test fails, then
  reverting. `internal/api/router_test.go`: `TestConnectorKindsRouteSessionOnly` /
  `…SessionRejected` (`RequireSession` only, no platform-admin, no CSRF). `go test
  ./internal/connector/... ./internal/cp/sources/... ./internal/api/`: **PASS**; `mise run lint` —
  10 issues, unchanged (0 new).
- **Task 3-4 — web (`web/`, vitest + Testing Library, TDD RED→GREEN)**:
  `web/components/SourcesTable.test.tsx` (row rendering incl. error summary, empty state, action
  callbacks) and `web/components/SourceForm.test.tsx` (kind picker renders exactly the selected
  kind's fields, required markers, secret→password, config/credentials split on submit, edit
  pre-fill with blank write-only secrets). `cd web && npx vitest run`: **PASS** (33/33);
  `npm run build`: compiles clean, routes `/admin/sources`, `/admin/sources/new`,
  `/admin/sources/[id]/edit` present.

## Out of scope (later stories)
- Mounting `jobs`/`documents`/members/settings behind `RequireTenantAccess` — STORY-11.3–11.6
  reuse the same middleware, added there.
- An `s3` connector (and its `Fields()`) — `KindS3` has no registered connector yet (EPIC-07);
  `Schemas()` will include it automatically once one registers, no code change needed here.
