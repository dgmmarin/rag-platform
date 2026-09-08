# ADR-0075: Session-admin tenant-scoped API — `/admin/tenants/{id}/…` reusing the existing tenant-scoped services

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** FR-ADM-01/02/03, FR-ACC-03 · **Decisions:** ADR-0073 (admin UI BFF), ADR-0003, ADR-0027 (router/middleware), SPEC-02 §4, SPEC-09 §3

## Context
ADR-0073 established that each EPIC-11 admin-UI story adds the minimal session-authenticated,
tenant-scoped API endpoints its screens need, because the tenant-scoped data surface (`/v1/sources`,
jobs, documents) is Bearer/API-key authenticated with the tenant derived from the key (FR-ACC-03) —
unusable from a session admin UI where a platform admin acts across tenants. STORY-11.2 (sources) is
the first such story, so it fixes the **pattern** every later tenant story (jobs 11.3, documents
11.4, members/settings 11.5, query/eval 11.6–11.7) reuses.

Key fact: the existing tenant-scoped handlers (e.g. `internal/cp/sources/handlers.go`) already read
their tenant from the request context via `tenant.TenantIDFromCtx` — the API-key scope middleware
injects it. Nothing about those handlers is bound to API-key auth; only the middleware that sets the
context is.

## Options / decisions
- **New session routes under `/admin/tenants/{id}/…`, reusing the existing handlers/services verbatim.**
  A new session middleware — `RequireTenantAccess` — resolves the tenant from the `{id}` path
  segment, authorizes the session principal, and injects the tenant into the context using the SAME
  key the handlers already read (`tenant.WithTenantID`/`TenantIDFromCtx`). The existing
  `sources.Handlers` (and later `jobs`, `documents`, … handlers) are then mounted unchanged behind
  it. No business logic is duplicated between the Bearer `/v1/*` surface and the session `/admin/*`
  surface — the difference is purely which middleware set the tenant.
  - Rejected: **dual-authing `/v1/sources`** (accept session-or-Bearer, tenant from key-or-header) —
    mixes two auth models and two tenant-sources-of-truth on one surface. Rejected: the BFF minting
    per-tenant API keys — a platform admin holds none; against the session model (ADR-0073).

- **Authorization = platform-admin OR the session user's `tenant_members` role for `{id}`.** Reads
  (GET) require any membership role (or platform-admin); writes (POST/PATCH/DELETE) require the
  role the operation needs per the SPEC-02 §4 role matrix (admin/owner for source mutations), or
  platform-admin. An unknown/again-inaccessible tenant is 404 (not 403) so the surface does not leak
  tenant existence to a non-member. A **platform admin acting on a tenant they are not a member of**
  is audited with `details.impersonation=true` (SPEC-02 §4, FR-ADM-05), reusing the existing audit
  path.

- **Path uses the tenant UUID (`{id}`), matching the existing `/admin/tenants/{id}`.** The admin UI
  already holds `tenant_id` (from `/v1/auth/me` and `/admin/tenants`) and selects the current tenant
  in `TenantProvider`; the BFF forwards `/admin/tenants/{id}/…` unchanged.

- **CSRF on mutations** (SPEC-09 §3), like every other session-cookie mutation; GETs carry no CSRF.
  All of this rides the BFF (browser → Next `/bff/admin/tenants/{id}/…` → `ragctl`), so the browser
  boundary stays same-origin (ADR-0073).

- **Connector-kind form schema is a session, platform-global endpoint** (`GET /admin/connector-kinds`,
  not tenant-scoped): each kind (`upload/web_crawl/sitemap/api/s3`) returns its form field
  descriptors (name/label/type/required/secret). Descriptors are co-located with each connector and
  a test guards them against `SourcesValidator.ValidateConfig` so the rendered form and server
  validation cannot silently drift. The UI renders create/edit forms from it; secret fields are
  write-only.

## Consequences
- One reusable session middleware (`RequireTenantAccess`) + a mount block per tenant story; the
  tenant-scoped business logic is written once and served on both the Bearer `/v1/*` and session
  `/admin/tenants/{id}/*` surfaces.
- STORY-11.2 mounts `sources.Handlers` there and adds the connector-kinds schema endpoint; 11.3–11.6
  mount `jobs`/`documents`/members/settings handlers behind the same middleware.
- Cross-tenant platform-admin actions are auditable as impersonation, satisfying SPEC-02 §4 without a
  separate impersonation ceremony per request.
- No schema migration; no change to the existing Bearer `/v1/*` routes.
