# ADR-0073: Admin UI is a React/TypeScript SPA, embedded and served same-origin, session-authenticated

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** FR-ADM-01/02/03, FR-ACC-03, SPEC-02 §4 · **Decisions:** ADR-0003, ADR-0020 (OIDC), ADR-0027 (router/middleware), SPEC-07, SPEC-09 §3

## Context
EPIC-11 delivers the reference Admin UI backing FR-ADM-01/02/03 (list a tenant's sources, jobs,
documents/chunks) plus members/settings/query/eval screens. The repo has no UI yet: no
`package.json`, no `web/` dir, no Node in the toolchain. The API, however, already anticipates the
admin UI — session-cookie auth (`rag_session`, HttpOnly), CSRF double-submit (`X-CSRF-Token`),
OIDC start/callback and password `login`/`logout`, and `RequireSession`/`RequireRoleAdmin`/`CSRF`
middleware (`internal/api/router.go`, `internal/cp/auth`). SPEC-02 §4 defines the model the UI
rides on: `users` (with `is_platform_admin`), `tenant_members` (per-tenant `tenant_role`), and
"session auth for the admin UI". This ADR records the architecture decisions taken during
brainstorming so the whole epic builds to one shape.

## Options / decisions
- **Stack = Vite + React + TypeScript SPA.** Chosen (by the maintainer) over a Go-native
  server-rendered option (`html/template`/`templ` + htmx). React is the conventional choice for a
  *reference* admin UI others read and extend; TypeScript for safety. Trade-off accepted: a Node
  toolchain and build step now exist in a previously Go-only repo. Routing via React Router; server
  state via TanStack Query over a thin `fetch` wrapper; styling via CSS Modules with a small design
  token set (no Tailwind / heavy component library — YAGNI, easy to revisit).

- **Embedded and served same-origin.** The SPA builds to `web/dist/`, which is `go:embed`-ed into
  `ragctl` and served by `serve` at `GET /admin/{path...}` with SPA fallback to `index.html` for
  client-side routes. Same-origin is the crux: the HttpOnly `rag_session` cookie and the
  `X-CSRF-Token` double-submit work with **no CORS** and no token-in-JS storage of the session.
  - **Build ordering:** `go:embed web/dist` fails if the directory is absent, so a committed
    placeholder `web/dist/` (a stub `index.html`) keeps `go build` working, and the `build` mise
    task runs the web build first. **ponytail:** placeholder-dir approach; ceiling: a stale
    placeholder could ship if the web build is skipped — the `build` task ordering prevents it in
    the normal path; upgrade path: a build tag that fails loudly on a placeholder in release builds.

- **Auth = session cookie + CSRF; a new `GET /v1/auth/me` hydrates the SPA.** Login uses the
  existing `POST /v1/auth/login` (sets the cookie, returns `{csrf_token}`) or an OIDC-redirect
  button (`/v1/auth/oidc/start`); logout uses `POST /v1/auth/logout`. Because the session cookie is
  HttpOnly and the CSRF token is only returned once at login, a page reload cannot recover auth
  state — so this epic adds **`GET /v1/auth/me`** (session-authenticated) returning
  `{user{id,email}, is_platform_admin, memberships[{tenant_id,slug,name,role}], csrf_token}`. The
  SPA calls it on load; a 401 means "logged out" and routes to `/admin/login`.

- **Each story pairs UI with its backing session-authenticated endpoints.** The tenant-scoped data
  surface (`/v1/sources`, jobs, documents) is today **Bearer/API-key** authenticated with the
  tenant derived from the key (FR-ACC-03) — unusable from a session-cookie admin UI where a
  platform admin acts across tenants. Rather than build the whole session-admin API up front, each
  EPIC-11 story ships its screens **and** the minimal session-authenticated, tenant-scoped
  endpoints they need (authorized by `tenant_members` role / `is_platform_admin`, CSRF on
  mutations, platform-admin cross-tenant action audited as impersonation per SPEC-02 §4). STORY-11.1
  needs only `GET /v1/auth/me` and reuses `GET /admin/tenants` (platform admins) + `/me.memberships`
  (members) for the tenant switcher.

- **Testing = Vitest (+ RTL) for the JS side, Playwright for browser E2E, Go tests for endpoints.**
  Unit (fetch wrapper: attaches CSRF, treats 401 as logged-out) and component (auth guard, tenant
  switcher) tests run under Vitest/React Testing Library in jsdom. A Playwright golden-path E2E
  drives the built SPA served by `ragctl serve` against the live stack (login → `/me` hydrate →
  tenant switch persists → logout → guard redirect), as its own `web-e2e` mise task + CI job.
  Session endpoints get Go table tests. Chosen over rod (Go) for auto-waiting and SPA ergonomics,
  accepting a second (Node) test toolchain that Vite already introduced.

## Consequences
- A Node toolchain (pinned via mise), a `web/` workspace, and web build/test/e2e CI jobs now exist;
  `ragctl` gains an embedded `/admin` SPA and a session `GET /v1/auth/me` endpoint.
- The same-origin embed keeps auth simple (no CORS, no session token in JS) and ships the UI in the
  single `ragctl` binary.
- EPIC-11 is explicitly not frontend-only: every story grows the session-authenticated admin API
  surface alongside its screens. The full design and per-story surface live in SPEC-11.
- STORY-12.4's deferred eval-report render (ADR-0072) becomes a normal EPIC-11 screen over the
  existing `ragctl eval report` data contract.
