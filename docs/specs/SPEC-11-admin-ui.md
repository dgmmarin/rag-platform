# SPEC-11: Admin UI (reference)

**Implements:** FR-ADM-01/02/03 (and FR-ADM-04 render, carried from STORY-12.4), FR-ACC-03 · **Decisions:** ADR-0073, ADR-0003, ADR-0020, ADR-0027, SPEC-02 §4, SPEC-09 §3

The reference Admin UI: a **Next.js (App Router) + TypeScript** app running as its own Node service,
a **BFF** whose browser boundary is same-origin and which proxies to a session-authoritative
`ragctl` server-side (ADR-0073). This spec covers the epic-wide conventions and details STORY-11.1
(app shell, auth, tenant switcher). Later stories (11.2–11.7) each add their screens plus the minimal
session-authenticated, tenant-scoped API endpoints they require.

## 1. Architecture

```
Browser ──same-origin──> Next.js Node server (:3000) ──server-to-server──> ragctl API (:8080)
  App Router pages       BFF proxy: /v1/*, /admin/*        session cookie + CSRF relayed
```

```
web/                         Next.js App Router + TS app (new Node service)
  app/
    admin/…                  routes: login, dashboard shell, placeholder pages
    api/                     BFF: route handlers proxying /v1/* and /admin/* to ragctl
  lib/                       api client (browser→Next), server proxy helper, auth/tenant state
  Dockerfile, next.config.ts
internal/cp/auth (or api)    GET /v1/auth/me handler + "memberships for user" query (ragctl side)
```

- **Serving:** Next runs as a Node service; the browser loads pages and calls the BFF on the **Next
  origin**. `ragctl serve` is unchanged except for the new `/v1/auth/me` endpoint; it does **not**
  serve the UI (no `go:embed`).
- **BFF proxy:** Next route handlers (or middleware) under `app/api` (or a catch-all) forward
  `/v1/*` and `/admin/*` to `ragctl` (`RAGCTL_API_URL`), passing the incoming `rag_session` cookie
  and `X-CSRF-Token` upstream and relaying `ragctl`'s responses (with `Set-Cookie` Domain rewritten
  to the Next origin). **No CORS** — the browser never calls `ragctl` directly.
- **Dev:** the dev stack runs `next dev` (:3000) alongside `ragctl serve` (:8080); the BFF points at
  `RAGCTL_API_URL=http://localhost:8080`.

## 2. Auth model (SPEC-02 §4, ADR-0073)

- **Identity:** `users` (with `is_platform_admin`); tenant access via `tenant_members`
  (`tenant_role`). A platform admin may act on any tenant (audited as impersonation).
- **Login:** browser → Next login route handler → `POST /v1/auth/login` on `ragctl` (password) which
  sets `rag_session` (HttpOnly) and returns `{csrf_token}`; Next relays the cookie to the browser
  with the Domain rewritten to the Next origin and returns `csrf_token`. The OIDC button navigates
  to the Next-proxied `/v1/auth/oidc/start`; the provider `redirect_uri` targets the Next origin.
  `POST /v1/auth/logout` (via the BFF) clears the session.
- **Hydration:** `GET /v1/auth/me` (session-authenticated, via the BFF) returns the current user,
  admin flag, memberships, and the current `csrf_token`. A 401 → logged out → redirect to
  `/admin/login`.

### 2.1 `GET /v1/auth/me` (new in STORY-11.1, ragctl side)
Session-authenticated (behind `RequireSession`), not platform-admin gated, GET (no CSRF). Response:
```json
{
  "user": { "id": "uuid", "email": "a@b.com" },
  "is_platform_admin": false,
  "memberships": [ { "tenant_id": "uuid", "slug": "acme", "name": "Acme Inc", "role": "admin" } ],
  "csrf_token": "…"
}
```
- 401 when no valid session. `memberships` = the session user's `tenant_members` rows joined to
  `tenants` for slug/name. Requires a new "memberships for user id" query (the existing
  `MembershipService` lists members *of a tenant*, not tenants *of a user*).
- Never logs/returns the session token; `csrf_token` mirrors the login response contract.

## 3. BFF proxy (STORY-11.1)

- A server-side proxy in Next forwards `/v1/*` and `/admin/*` to `RAGCTL_API_URL`, copying the
  request method/body/headers, injecting the incoming `rag_session` cookie and `X-CSRF-Token`, and
  relaying the upstream status/body. On a `Set-Cookie` from `ragctl` it rewrites the Domain to the
  Next origin (preserving HttpOnly/Path/Secure/SameSite). A 401 is passed through so the client
  treats it as logged-out.
- **ponytail:** a thin pass-through proxy, not a full session store; ceiling: the browser holds the
  `ragctl` session cookie on the Next origin (no server-side session mapping). Upgrade path: a
  server-side session store in Next if opaque-session isolation is later required.

## 4. Tenant switcher (STORY-11.1)

- Lists the tenants the user can act on: **members** → `/me.memberships`; **platform admins** →
  additionally all tenants via the (BFF-proxied) `GET /admin/tenants`.
- Selection held in client state and persisted in `localStorage`; it is the "current tenant" that
  tenant-scoped calls in 11.2–11.7 will target. STORY-11.1 only displays and persists it.

## 5. App shell (STORY-11.1)

- **Top bar:** product name, tenant switcher, user menu (email + logout).
- **Left nav:** Sources, Jobs, Documents, Members, Settings, Query, Eval — **placeholder** routes
  now, filled by 11.2–11.7 (Eval is the FR-ADM-04 render carried from STORY-12.4, over the
  `ragctl eval report` data contract, ADR-0072).
- **Routing:** App Router; an auth guard (server- or client-side) gates every route except
  `/admin/login`, redirecting on a 401 from `/me`.
- **Data layer:** a browser `fetch` wrapper (same-origin to Next) that attaches `X-CSRF-Token` on
  mutations and surfaces a 401 as logged-out; TanStack Query for cached reads where useful.

## 6. Build, toolchain & ops

- `web/` Next app: `mise-tasks/web` (build), `web-dev` (`next dev`), `web-test` (vitest),
  `web-e2e` (Playwright). Node pinned via mise.
- **Ops (new service):** a `web/Dockerfile`; a compose service for the Next app (`deploy/…` +
  `mise dev`); a CI job (install, `web-test`, build) and a `web-e2e` job (browser install, live
  stack + Next) ; an EPIC-10 deploy/runbook entry for the Next service and the OIDC `redirect_uri`.

## 7. Testing (STORY-11.1)

- **Unit (Vitest):** the browser fetch wrapper — attaches CSRF on mutations, treats 401 as
  logged-out.
- **Route handler (Vitest):** the BFF proxy — forwards `rag_session` + `X-CSRF-Token` upstream,
  rewrites the `Set-Cookie` Domain, relays 401.
- **Component (Vitest + RTL):** the auth guard redirects when unauthenticated; the tenant switcher
  renders memberships and persists the selection.
- **E2E (Playwright):** against the Next origin — login → `/me` hydrate → tenant switch persists
  across reload → logout → guard redirects to `/admin/login`.
- **Backend (Go):** `GET /v1/auth/me` — session → payload; 401 without session; `is_platform_admin`
  reflected; memberships shape (joined slug/name/role).

## 8. Scope

- **STORY-11.1 (this spec's build):** the Next app scaffold + BFF proxy + dev/build/ops wiring;
  `GET /v1/auth/me` + the memberships-for-user query; login (password + OIDC button) / logout; app
  shell + nav placeholders; tenant switcher. Artifacts: this spec, ADR-0073, ISSUE-0056.
- **Deferred to 11.2–11.7:** the Sources/Jobs/Documents/Members/Settings/Query/Eval screens and
  their session-authenticated tenant-scoped endpoints (each story pairs UI + endpoints, ADR-0073).

## 9. Constraints checklist (per story)

- Tenant content reached only via the resolver → `*tenant.DB` (ADR-0003); the control-plane pool
  serves registry/auth only.
- Mutations are CSRF-protected end-to-end (browser→Next→ragctl, SPEC-09 §3); session tokens never
  leave the HttpOnly cookie; the BFF never logs a session token or CSRF secret.
- Cross-tenant platform-admin actions are audited as impersonation (SPEC-02 §4, FR-ADM-05).
- The BFF adds no CORS; the browser talks only to the Next origin.
