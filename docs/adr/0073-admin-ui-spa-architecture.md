# ADR-0073: Admin UI is a Next.js (App Router) app on its own Node server, a BFF proxying a session-authoritative ragctl

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** FR-ADM-01/02/03, FR-ACC-03, SPEC-02 §4 · **Decisions:** ADR-0003, ADR-0020 (OIDC), ADR-0027 (router/middleware), SPEC-07, SPEC-09 §3

> Revises the initial same-day draft of this ADR (a Vite + React SPA `go:embed`-ed into `ragctl` and served same-origin). That draft was never implemented; the maintainer chose a full Next.js Node server instead, which this ADR records.

## Context
EPIC-11 delivers the reference Admin UI backing FR-ADM-01/02/03 (a tenant's sources, jobs,
documents/chunks) plus members/settings/query/eval screens. The API already provides the auth the
UI needs: session-cookie auth (`rag_session`, HttpOnly), CSRF double-submit (`X-CSRF-Token`),
password `login`/`logout` and OIDC start/callback, and `RequireSession`/`RequireRoleAdmin`/`CSRF`
middleware (`internal/api/router.go`, `internal/cp/auth`). SPEC-02 §4 defines the identity model:
`users` (`is_platform_admin`), `tenant_members` (`tenant_role`), "session auth for the admin UI".

The maintainer chose **Next.js (App Router) running as its own Node server** for the UI, rather than
a Go-embedded static SPA. That breaks the single-binary, same-origin model the first draft relied
on, so this ADR records how auth, serving, and deployment are shaped instead.

## Options / decisions
- **Stack = Next.js (App Router) + TypeScript, running as a Node service.** Chosen by the maintainer
  over a Vite SPA (`go:embed`) and over Next static-export. Routing/SSR via the App Router; server
  state via TanStack Query on the client where needed. Trade-off accepted: a long-running Node
  service now exists alongside `ragctl`, with its own build, deploy, health, and CI.

- **Styling = Tailwind CSS (v4, `@tailwindcss/postcss`), superseding the initial "CSS Modules, no
  Tailwind (YAGNI)" note.** STORY-11.1 Task 7 found the CSS-Modules placeholder wouldn't carry a
  cohesive, high-end look across the growing screen count without hand-rolled token duplication;
  the maintainer approved adding Tailwind as a build-time PostCSS dependency (no CDN, no heavy
  component library) for a Modern SaaS (Linear/Vercel-like) design system: one desaturated
  indigo/violet accent, one cool-tinted neutral gray scale, light + dark via Tailwind's `class`
  strategy (system default, small toggle, guarded `localStorage`), Geist Sans/Mono via `next/font`.
  Tokens are defined once as CSS variables (`app/globals.css`) and mapped into Tailwind's theme, so
  both themes stay a single source of truth. All CSS Modules from 11.1's earlier tasks are removed.

- **BFF topology; the browser is same-origin to Next only.**
  `Browser ──same-origin──> Next (Node, :3000) ──server-to-server──> ragctl API (:8080)`.
  The browser never talks to `ragctl` directly, so **no CORS** is introduced and the cookie + CSRF
  model still holds at the browser↔Next boundary. Next carries a **BFF proxy** (App Router route
  handlers / middleware) for `/v1/*` and `/admin/*` that forwards the request to `ragctl`
  server-side.

- **`ragctl` stays the authentication authority; Next relays.** The existing auth is reused, not
  reimplemented:
  - **Login:** browser → a Next route handler → `POST /v1/auth/login` on `ragctl` → `ragctl` returns
    `Set-Cookie: rag_session` (HttpOnly) + `{csrf_token}`. Next relays the cookie to the browser
    **with the Domain rewritten to the Next origin** (and Path/Secure/SameSite preserved), and
    returns `csrf_token` to the client.
  - **Authenticated calls:** the browser sends `rag_session` (Next origin) + `X-CSRF-Token` to Next;
    the BFF proxy forwards both to `ragctl` on the upstream request.
  - **OIDC:** Next proxies `/v1/auth/oidc/start|callback`; the provider's `redirect_uri` points at
    the **Next origin** (a config/runbook item, not a code change).
  - **Hydration:** a new session-authenticated **`GET /v1/auth/me`** on `ragctl` returns
    `{user{id,email}, is_platform_admin, memberships[{tenant_id,slug,name,role}], csrf_token}`,
    consumed by Next (SSR or client) on load; a 401 means "logged out".
  Rejected: Next owning auth (Auth.js/OIDC) with `ragctl` as a token-verifying resource server — it
  duplicates the OIDC `ragctl` already implements and forces new bearer/JWT middleware on the Go
  admin surface. The BFF-relay keeps auth in one place.

- **Each story pairs UI with its backing session-authenticated endpoints.** The tenant-scoped data
  surface (`/v1/sources`, jobs, documents) is today Bearer/API-key authenticated with the tenant
  derived from the key (FR-ACC-03) — unusable from a session admin UI where a platform admin acts
  across tenants. Each EPIC-11 story ships its screens **and** the minimal session-authenticated,
  tenant-scoped endpoints they need (authorized by `tenant_members` role / `is_platform_admin`, CSRF
  on mutations, cross-tenant platform-admin action audited as impersonation per SPEC-02 §4).
  STORY-11.1 needs only `GET /v1/auth/me` and reuses `GET /admin/tenants` (platform admins) +
  `/me.memberships` (members) for the tenant switcher.

- **Deployment/ops: a first-class Node service.** A `web/` Next app with its own Dockerfile and a
  compose service; the dev stack runs `next dev` alongside `ragctl serve`; CI builds/tests/e2es it;
  the EPIC-10 deploy/runbooks gain a Next-service entry (including the OIDC `redirect_uri` note).

- **Testing = Vitest (+ RTL) for components, a route-handler test for the BFF proxy, Go tests for
  endpoints, Playwright for browser E2E.** Unit/component tests run under Vitest/RTL. The BFF proxy
  is unit-tested (forwards cookie + `X-CSRF-Token`, rewrites the `Set-Cookie` Domain, maps 401). The
  Playwright golden path drives the browser against the **Next origin** (login → `/me` hydrate →
  tenant switch persists across reload → logout → guard redirect). Session endpoints get Go table
  tests.

## Consequences
- Two runtime services (Next + `ragctl`), a Node toolchain, a `web/` workspace, and web
  build/test/e2e + Node-service deploy in CI. `ragctl` gains a session `GET /v1/auth/me`; it does
  **not** serve the UI (no `go:embed`).
- Because the browser is same-origin to Next and Next relays to `ragctl` server-side, the
  session-cookie + CSRF security model is preserved without CORS; the cost is the BFF proxy layer and
  the cookie-domain rewrite.
- EPIC-11 is explicitly not frontend-only: every story grows the session-authenticated admin API
  surface alongside its screens. The full design and per-story surface live in SPEC-11.
- STORY-12.4's deferred eval-report render (ADR-0072) becomes a normal EPIC-11 screen over the
  existing `ragctl eval report` data contract.
- The extra service/ops complexity is a deliberate maintainer choice, accepted over the lighter
  embedded-SPA option for an internal reference admin UI.
