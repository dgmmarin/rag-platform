# ISSUE-0068: Platform-admin tenants UI — list, enrol, suspend, delete (STORY-11.7)

**Type:** Feature · **Status:** Done · **Story:** STORY-11.7 · **Traces:** FR-TEN-01, FR-TEN-04, FR-TEN-05, ADR-0075

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs. The tenant
> lifecycle backend (endpoints, `RequirePlatformAdmin` gate, CSRF, session auth) is ISSUE-0005.

## Summary
STORY-11.7 gives the admin UI a platform-admin tenants screen: list every tenant, enrol a new one,
suspend/activate an existing one, and schedule one for deletion. Unlike the tenant-scoped stories
(11.2–11.6) this surface is platform-global — it calls `/admin/tenants` (no `{tenantId}` segment),
already session-authenticated and gated by `RequirePlatformAdmin` (STORY-04.6). **No new Go code**:
the entire backend is shipped; 11.7 is the TypeScript client + pages + a conditional nav entry.

## Scope
- **Task 1 — tenants client (web)**: `web/lib/tenants.ts` — `useTenants()` over `GET /admin/tenants`,
  `createTenant` (`POST`), `setTenantStatus` (`PATCH {status:"active"|"suspended"}`), `deleteTenant`
  (`DELETE`, server-default grace). Types mirror `internal/cp/tenants/admin.go` (`PlatformTenant`,
  `TenantPage`, `CreateResult`). ponytail: first page only, matching the tenant switcher.
- **Task 2 — tenants UI (web)**: `web/app/admin/tenants` page (enrol form: slug/name/region/
  embedding_dim; notice banner; inline enrol error) + `TenantsTable` (name, slug, status badge,
  region, created; row actions gated on status — Suspend on active, Activate on suspended, Delete
  unless deleting/deleted, with a confirm on delete). A non-platform-admin visitor gets a clear
  "platform admins only" panel; the server enforces the real gate regardless.
- **Task 3 — nav**: `PLATFORM_NAV_SECTIONS` in `web/lib/nav.ts` + `web/app/admin/layout.tsx` appends
  it to the sidebar only when `me.is_platform_admin`.

## Out of scope (later work)
- Tenant list pagination (follow `next_cursor`), per-tenant detail/edit (connection move, settings),
  and the audit-log view (FR-ADM-05) are not in this story.
- Enrol offers no region picker beyond a free-text field; the server validates.

## Tests / runnable checks
- **web (`web/`, vitest + Testing Library)**: `web/components/TenantsTable.test.tsx` (columns, empty
  state, status-gated actions, deleting row hides actions, suspend/delete wiring, busy disable).
  `cd web && npx vitest run`: **PASS** (87, +6 new); `npm run build`: clean, route `/admin/tenants`
  present; `npm run lint`: clean.
- Backend unchanged — `/admin/tenants` routes and their `router_test.go` coverage were delivered by
  ISSUE-0005.
