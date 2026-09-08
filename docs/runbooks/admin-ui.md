# Runbook: Admin UI (Next.js) service

**Traces:** SPEC-11 §1/§3/§6, STORY-11.1. **Service:** `web/` (Next.js, `output: "standalone"`).

The admin UI is its own Node service, separate from `ragctl serve`. The browser is
same-origin to Next only — every API call goes through Next's BFF route
(`web/app/bff/[...path]/route.ts`, proxying `/v1` + `/admin` to `RAGCTL_API_URL`), so
`ragctl` itself never needs CORS and is never reachable directly from the browser.

## Local dev

- `mise run api` — the `ragctl` API on `:8091` (repo-root `.env`, `RAGCTL_ADDR=:8091`; kept
  off `:8080` so a wgo watcher can hold that port).
- `mise run web-dev` — `next dev`, picking the first free port from 3000 up. `next dev`
  loads `web/.env.development` (committed, non-secret), which sets
  `RAGCTL_API_URL=http://localhost:8091` so the BFF hits the host API above.
- First login: `mise run seed` (after `mise run up` + migrations) bootstraps a platform
  admin and two demo tenants — `admin@example.com` / `devpassword123` (override the
  password via `RAGCTL_ADMIN_PASSWORD`), tenants `demo` ("Demo Tenant") and `acme`
  ("Acme Inc"). Both the bootstrap and the enroll it runs are idempotent, so re-running
  `mise run seed` is always safe.

## Deploying the Next service

- Build: `mise run web` (`npm ci && npm run build`); the standalone output is a
  self-contained server bundle (`web/Dockerfile` copies just that folder + `node`).
- Run: `node web/.next/standalone/server.js` (or `npm start` from a full `npm ci`
  install), listening on `PORT` (default 3000).
- Required env: `RAGCTL_API_URL`, the `ragctl serve` origin the BFF proxies to
  (`web/lib/config.ts`; defaults to `http://localhost:8080`, the container port used by
  docker-compose, for local container-to-container use). Set it to wherever `ragctl serve`
  actually listens in that environment — the BFF is the only thing that reads it; the
  browser never does.
- No CORS configuration is needed or wanted on either service (SPEC-11 §9): put a
  reverse proxy / TLS terminator in front of the Next origin, not in front of `ragctl`.

## OIDC: `redirect_uri` must target the Next origin

`ragctl serve`'s `OIDC_REDIRECT_URL` (STORY-03.2, SPEC-09 §3) is sent as the `redirect_uri`
parameter to the IdP and is also where the IdP redirects the browser back to after login.
Because the browser only ever talks to the Next origin, that URL must be the Next origin's
BFF path, not `ragctl`'s own host:port:

```
OIDC_REDIRECT_URL=https://<next-origin>/bff/v1/auth/oidc/callback
```

Register that exact URL as the allowed redirect URI on the IdP side too. Pointing it at
`ragctl`'s own address instead (e.g. `https://<ragctl-host>/v1/auth/oidc/callback`) sends
the browser somewhere it cannot reach directly (no CORS, and likely no public route to
`ragctl` at all) and the callback fails.

## Verification

- `curl -s https://<next-origin>/v1/auth/me` still 401s cleanly with no session — Next
  itself doesn't answer `/v1/*`; only `/bff/v1/*` is proxied. A guarded route
  (`/admin/<section>`) with no session cookie redirects to `/admin/login`.
- The Playwright golden-path E2E (`web/e2e/shell.spec.ts`, run via `mise run web-e2e`)
  exercises guarded-route → login → shell chrome → tenant switcher (persists across a
  reload) → logout end-to-end against a running Next origin backed by a seeded stack
  (`mise run seed`); it self-skips (clear message, exit 0) without `E2E_BASE_URL` set, so
  an unconfigured runner is never red-walled. CI runs it in the `web-e2e` job
  (`.github/workflows/ci.yml`) against the real compose stack.
