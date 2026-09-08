# SPEC-11: Admin UI (reference)

**Implements:** FR-ADM-01/02/03 (and FR-ADM-04 render, carried from STORY-12.4), FR-ACC-03 · **Decisions:** ADR-0073, ADR-0003, ADR-0020, ADR-0027, SPEC-02 §4, SPEC-09 §3

The reference Admin UI: a Vite + React + TypeScript single-page app, `go:embed`-ed into `ragctl`
and served same-origin at `/admin`, authenticated by the existing session cookie + CSRF. This spec
covers the epic-wide conventions and details STORY-11.1 (app shell, auth, tenant switcher). Later
stories (11.2–11.7) each add their screens plus the minimal session-authenticated, tenant-scoped
API endpoints they require (ADR-0073).

## 1. Architecture

```
web/                         Vite + React + TS SPA workspace (new)
  src/
    main.tsx, App.tsx        app shell, router
    api/                     fetch wrapper (CSRF, 401 handling), typed clients
    auth/                    AuthProvider, useAuth, RequireAuth guard
    tenant/                  TenantProvider, TenantSwitcher
    routes/                  login, and placeholder pages for 11.2–11.7
  dist/                      build output — go:embed target (placeholder committed)
internal/adminui/            go:embed of web/dist + SPA handler (fallback to index.html)
internal/cp/auth (or api)    GET /v1/auth/me handler + "memberships for user" query
```

- **Serving:** `ragctl serve` mounts the embedded SPA at `GET /admin/{path...}`; unknown sub-paths
  fall back to `index.html` (client-side routing). API stays at `/v1/*` and `/admin/tenants` (the
  platform-admin JSON surface is unchanged; note `/admin/*` JSON routes coexist with the SPA mount —
  the SPA handler serves only non-API `/admin` paths).
- **Same-origin:** no CORS; the HttpOnly `rag_session` cookie is sent automatically; mutations echo
  `X-CSRF-Token` (ADR-0073, SPEC-09 §3).
- **Dev:** Vite dev server (`:5173`) proxies `/v1` and `/admin/tenants` (and other API paths) to a
  running `ragctl serve`. **Build order:** the `build` task builds `web/dist` before `go build`; a
  committed placeholder `web/dist/index.html` keeps `go:embed` compiling when the web build is
  skipped.

## 2. Auth model (SPEC-02 §4, ADR-0073)

- **Identity:** `users` (with `is_platform_admin`); tenant access via `tenant_members`
  (`tenant_role`). A platform admin may act on any tenant (audited as impersonation).
- **Session:** `POST /v1/auth/login` (password) sets `rag_session` (HttpOnly) and returns
  `{csrf_token}`; the OIDC button redirects to `GET /v1/auth/oidc/start`. `POST /v1/auth/logout`
  clears the session.
- **Hydration:** `GET /v1/auth/me` (session-authenticated) returns the current user, admin flag,
  memberships, and the current `csrf_token`, so a reload re-establishes auth state. A 401 → logged
  out → redirect to `/admin/login`.

### 2.1 `GET /v1/auth/me` (new in STORY-11.1)
Session-authenticated (no CSRF — it is a GET). Response:
```json
{
  "user": { "id": "uuid", "email": "a@b.com" },
  "is_platform_admin": false,
  "memberships": [ { "tenant_id": "uuid", "slug": "acme", "name": "Acme Inc", "role": "admin" } ],
  "csrf_token": "…"
}
```
- 401 when no valid session. `memberships` is the set of `tenant_members` rows for the session user,
  joined to `tenants` for slug/name. Requires a new "memberships for user id" query (the existing
  `MembershipService` lists members *of a tenant*, not tenants *of a user*).
- Secrets: never logs/returns the session token; `csrf_token` mirrors the login response contract.

## 3. Tenant switcher (STORY-11.1)

- The switcher lists the tenants the user can act on: **members** → `/me.memberships`; **platform
  admins** → additionally all tenants via the existing `GET /admin/tenants`.
- Selection is held in a `TenantProvider` context and persisted in `localStorage`; it is the
  "current tenant" that tenant-scoped calls in 11.2–11.7 will target (via a path param
  `/admin/tenants/{id}/…` or a documented header — decided per those stories).
- STORY-11.1 only displays and persists the selection; no tenant-scoped data is fetched yet.

## 4. App shell (STORY-11.1)

- **Top bar:** product name, tenant switcher, user menu (email + logout).
- **Left nav:** the EPIC-11 destinations — Sources, Jobs, Documents, Members, Settings, Query, Eval
  — rendered as **placeholder** routes now, filled by 11.2–11.7 (Eval report is the FR-ADM-04 render
  carried from STORY-12.4, over the `ragctl eval report` data contract, ADR-0072).
- **Routing:** React Router; `RequireAuth` gates every route except `/admin/login`, redirecting on a
  401 from `/me`.
- **Data layer:** TanStack Query over a `fetch` wrapper that (a) attaches `X-CSRF-Token` from auth
  state on mutating requests, (b) surfaces a 401 as "logged out" to the AuthProvider.

## 5. Build & toolchain

- `mise-tasks/web` (vite build → `web/dist`), `web-dev` (vite dev server), `web-test` (vitest),
  `web-e2e` (Playwright). Node pinned via mise. The main `build` task depends on `web`.
- CI gains a web job: install, `web-test`, `web` build; a `web-e2e` job (with a browser install)
  runs the Playwright golden path against `ragctl serve` on the live stack.

## 6. Testing (STORY-11.1)

- **Unit (Vitest):** the fetch wrapper — attaches CSRF on mutations, treats 401 as logged-out.
- **Component (Vitest + RTL):** `RequireAuth` redirects when unauthenticated; `TenantSwitcher`
  renders memberships and persists the selection.
- **E2E (Playwright):** login → `/me` hydrate → tenant switch persists across reload → logout →
  guard redirects to `/admin/login`.
- **Backend (Go):** `GET /v1/auth/me` — session → payload; 401 without session; `is_platform_admin`
  reflected; memberships shape (joined slug/name/role).

## 7. Scope

- **STORY-11.1 (this spec's build):** `web/` scaffold + build/embed wiring; `GET /v1/auth/me` + the
  memberships-for-user query; login (password + OIDC button) / logout; app shell + nav placeholders;
  tenant switcher. Artifacts: this spec, ADR-0073, ISSUE-0056.
- **Deferred to 11.2–11.7:** the Sources/Jobs/Documents/Members/Settings/Query/Eval screens and
  their session-authenticated tenant-scoped endpoints (each story pairs UI + endpoints, ADR-0073).

## 8. Constraints checklist (per story)

- Tenant content reached only via the resolver → `*tenant.DB` (ADR-0003); the control-plane pool
  serves registry/auth only.
- Mutations are CSRF-protected (SPEC-09 §3); session tokens never leave the HttpOnly cookie.
- Cross-tenant platform-admin actions are audited as impersonation (SPEC-02 §4, FR-ADM-05).
