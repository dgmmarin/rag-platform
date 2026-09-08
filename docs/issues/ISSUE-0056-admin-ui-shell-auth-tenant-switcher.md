# ISSUE-0056: Admin UI shell, auth and tenant switcher (STORY-11.1)

**Type:** Feature · **Status:** Done · **Story:** STORY-11.1 · **Traces:** FR-ADM-01..03,
FR-ACC-03, SPEC-11, ADR-0073

> The *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs (ADR-0073).

## Summary
STORY-11.1 stands up the reference Admin UI: a **Next.js (App Router) + TypeScript** app running
as its own Node service, with a **BFF proxy** so the browser stays same-origin to Next while Next
relays session-cookie + CSRF auth to `ragctl` server-to-server (ADR-0073, SPEC-11). The story ran
as 8 tasks; all 8 are shipped and STORY-11.1 is done (`docs/backlog/BACKLOG_STATUS.md`). Beyond
the original shell scope, the story also delivered `ragctl admin bootstrap` + `mise run seed`
onboarding (ISSUE-0057, ADR-0074) and a Tailwind redesign of the login page and app shell.

## Scope (Task 1 of 6 — shipped)
- **`web/`** — the Next.js app scaffold (`create-next-app --ts --app --no-tailwind --eslint
  --no-src-dir --import-alias "@/*"`), trimmed to a minimal `app/page.tsx` (`<main>admin</main>`)
  and a plain `app/layout.tsx`; demo assets (page CSS module, SVG logos, generated `AGENTS.md`)
  removed as unused.
- **`web/lib/config.ts`** — `RAGCTL_API_URL` (env override, defaults to `http://localhost:8080`).
- **`web/app/bff/[...path]/route.ts`** — the BFF proxy: a catch-all route handler forwarding
  method/headers/body to `${RAGCTL_API_URL}/<path>`, passing through the inbound `rag_session`
  cookie and `X-CSRF-Token`, and relaying the upstream response with `Set-Cookie` `Domain`
  stripped (host-only on the Next origin), status pass-through including 401. `/bff/*` (not
  `/admin/*`) so the browser prefix never collides with `ragctl`'s `/admin/tenants` API path.
- **Vitest** (jsdom, globals, RTL/jest-dom wired for later component tests) —
  `web/vitest.config.ts`, `web/vitest.setup.ts`, `web/app/bff/[...path]/route.test.ts`.
- **`web/Dockerfile`** — multi-stage (`node:24-bookworm-slim` build → `next build` with
  `output: "standalone"` → slim runtime, non-root `node` user), `web/.dockerignore`.
- **`mise-tasks/web`** (`npm ci && npm run build`), **`mise-tasks/web-dev`** (`npm run dev`);
  `mprocs.yaml` gains a `web` proc (`mise run web-dev`, `autostart: false` to match `api`/`worker`
  — bring it up alongside `api` since the BFF needs `RAGCTL_API_URL` reachable); `mise.toml` pins
  `node = "24"`. `mise-tasks/dev` itself needs no change — it only `exec mprocs`, and the service
  list lives in `mprocs.yaml`.

## Decisions (ADR-0073, SPEC-11)
- BFF browser prefix is `/bff/*`, distinct from the `/admin/*` **UI** routes later tasks add
  (dashboard shell, nav) — avoids colliding with `ragctl`'s own `/admin/tenants` API path. SPEC-11
  §1/§3 describe `/v1/*`+`/admin/*` proxying; this repo's concrete routing uses one catch-all under
  `/bff/*` that forwards `<path>` verbatim to `RAGCTL_API_URL/<path>`, so the browser calls
  `/bff/v1/auth/me`, `/bff/admin/tenants`, etc.
- `output: "standalone"` in `next.config.ts` so the Docker runtime stage is just the trimmed
  server bundle + `node`, no `npm install` at runtime — parity with the ragctl image's small,
  non-root runtime (ADR-0002).
- **ponytail:** `mise run dev` orchestrates `web` as a second mprocs process alongside `ragctl
  serve`, no compose profile yet; ceiling: no health-gating between them (the BFF will just get
  connection-refused until `api` is up); upgrade path: a compose `app`-profile service for `web`
  once EPIC-10 ops picks up the Next service (ADR-0073 "Deployment/ops").

## Tests / runnable checks
- **BFF proxy (Vitest, TDD)**:
  - RED — `cd web && npx vitest run app/bff` before `route.ts` existed:
    `Error: Failed to resolve import "./route" ... Does the file exist?` (1 failed suite, no
    tests ran).
  - GREEN — same command after implementing `route.ts`: `Test Files 1 passed (1)` /
    `Tests 2 passed (2)` — forwards `cookie`/`x-csrf-token` upstream and asserts the target URL
    contains `/v1/auth/me`; asserts the relayed `set-cookie` no longer contains
    `Domain=api.internal`; asserts a 401 passes through unchanged.
- **Full suite**: `cd web && npm run build && npx vitest run` — Next build succeeds (typecheck +
  static/dynamic route generation: `/`, `/_not-found` static, `/bff/[...path]` dynamic); Vitest:
  2/2 passing.
- **Docker**: `docker build -f web/Dockerfile web` succeeds; `docker run` the built image and
  `curl localhost:3000/` returns `200`.
- **Shell syntax**: `bash -n mise-tasks/web mise-tasks/web-dev` OK.

## Deferred to later STORY-11.1 tasks
- `GET /v1/auth/me` (ragctl side) + the "memberships for user" query (Task 2, Go-only —
  `internal/api`, `internal/cp/auth`; no conflict with this task's web-only diff).
- Login (password + OIDC button) / logout route handlers, the browser fetch wrapper (CSRF on
  mutations, 401 → logged-out), the auth guard, app shell (top bar, left nav placeholders) and the
  tenant switcher (`/me.memberships` + `/admin/tenants` for platform admins, `localStorage`
  persistence) — SPEC-11 §2, §4, §5 (Tasks 3–6).
- CI wiring (`web-test`/`web-e2e` jobs, Playwright) and the EPIC-10 deploy/runbook entry for the
  Next service — SPEC-11 §6 (later task in this story).
