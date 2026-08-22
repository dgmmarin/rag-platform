# ADR-0027: Public router — a dependency-injected `internal/api` package, credential-keyed rate-limiter-after-auth chain, and one SPEC-07 §1 error envelope

**Status:** Accepted · **Date:** 2026-08-22 · **Requirements:** SPEC-07 §1, FR-ACC-03, NFR-SEC-07, NFR-MNT-01, C-3

## Context
STORY-04.1 stands up the public HTTP server: the router, the middleware chain in
the SPEC-07 §1 order, the health/readiness/metrics endpoints, the JSON error
envelope, and graceful shutdown. It is the integration point every EPIC-02/03
story deferred its "router wiring" to (ADR-0019/0020/0021/0023/0024/0025/0026):
auth handlers + `RequireSession`/`CSRF`, the platform-admin surface
(`RequirePlatformAdmin` → audit read, impersonation), the tenant-scoped surface
(`RequireScope*` → usage) with the rate limiter, all get mounted here.

Three sub-decisions had to be made because SPEC-07 §1 fixes the *set* of concerns
and the error contract but leaves the package boundary, the exact composition
order, and the envelope-uniformity open:

1. Where the router lives and how it is made testable without a database.
2. Where the credential-keyed rate limiter sits relative to authentication —
   SPEC-07 §1 lists "rate limit" abstractly before "auth", but the STORY-03.9
   limiter keys its bucket off the authenticated credential + resolved tenant.
3. Whether every router-mounted middleware/handler already speaks the one
   SPEC-07 §1 error envelope, or several body shapes coexist.

No third-party router was adopted: Go 1.22's `net/http.ServeMux` supports the
method+path patterns the surface needs (`GET /healthz`,
`DELETE /admin/impersonations/{id}`), so the standard library is sufficient and
no new dependency is added (NFR-MNT keeps the dependency surface small).

## Options
- **Router package boundary.** (a) Assemble the mux directly in `internal/cli`
  next to the pool wiring, or (b) a dependency-injected `internal/api` package
  that takes a `Deps` struct of middleware/handler values, with the DB wiring
  kept in `cli.buildAPIServer`. (a) couples route assembly to a live Postgres and
  makes the chain only testable through an e2e; (b) lets the whole chain be
  unit-tested with stubs.
- **Rate-limiter position.** (a) Follow SPEC-07 §1's abstract order literally
  (rate limit outermost, before auth), or (b) place the credential-keyed limiter
  *inside* per-route auth so it has a resolved credential + tenant to key on.
- **Error envelope.** (a) Leave each package's existing body shape (some emitted
  `{"error":"msg"}` strings), or (b) converge every router-mounted
  middleware/handler onto the single SPEC-07 §1 object envelope
  `{"error":{"code","message"}}`.

## Decision
- **A dependency-injected `internal/api` package.** `api.New(d Deps) http.Handler`
  assembles the mux from injected middleware (`RequireSession`, `CSRF`,
  `RequirePlatformAdmin`, `RequireScope*`, `RequireRoleAdmin`, `RateLimit`) and
  handler values (auth, audit, usage, impersonation). The real dependencies are
  built once from a single control-plane pool (never a tenant pool — C-3) in
  `cli.buildAPIServer`; the router itself imports no DB. A nil middleware is a
  pass-through and a nil handler is a not-implemented seam returning the
  `not_found` envelope, so a partially-wired server still boots and fails closed
  rather than nil-panicking. This keeps route assembly unit-testable with stubs
  (`net/http/httptest`) and preserves NFR-MNT-01's "add a route/provider without
  touching unrelated code" property.
- **Auth precedes the credential-keyed rate limiter (divergence from SPEC-07 §1's
  abstract order, recorded here).** The STORY-03.9 limiter keys its token bucket
  off the authenticated API-key id + resolved tenant in context (FR-ACC-03, never
  a client parameter), so it *must* run after the scope middleware that resolves
  them. The per-tenant surface therefore chains
  `scope → rate-limit → handler` (auth outermost, limiter just inside it). This
  realises the same intent as the spec's ordering — abuse protection on
  authenticated traffic — while keeping the limit key credential-derived. A
  request with no resolved tenant never reaches the limiter (the scope middleware
  401s first), matching ADR-0026's fail-closed "no tenant → 401".
- **The global chain, outer → inner, is
  `request-id/logging/tracing/metrics (obs.Middleware) → recovery → CORS →
  [route]`.** `obs.Middleware` is outermost so it stamps `X-Request-Id` on every
  response (including errors from inner layers) and correlates logs; `Recover`
  sits just inside it so every downstream panic is caught, logged server-side
  with the request id, and turned into a `500` envelope with no panic value or
  stack leaked to the client (fail closed); `cors()` answers preflight `OPTIONS`
  `204` and sets the browser-UI headers. Per-route auth/scope/CSRF/rate-limit run
  inside this global chain, per route group.
- **One error envelope everywhere (SPEC-07 §1).** `api.WriteError` writes
  `{"error":{"code","message","request_id"}}` with the request id pulled from
  context, and the router-mounted auth, audit, and usage middleware/handlers were
  converged onto the same object shape (their `writeError` now emits
  `{"error":{"code":errorCodeForStatus(status),"message":msg}}`). This closes a
  real bug the e2e caught: an unauthenticated `/admin/audit` previously returned
  `{"error":"authentication required"}` (a bare string) from
  `auth.RequireSession`, which no SPEC-07 client could parse. The auth-layer
  `writeError` has no `*http.Request` to read the body `request_id` from, so
  correlation on those errors is carried by the `X-Request-Id` response header
  (guaranteed by the outermost `obs.Middleware`); unifying every `writeError`
  signature to thread the request is a follow-up, not required for the contract.

## Consequences
- No new dependency: the router is standard-library `net/http` + Go 1.22
  `ServeMux` patterns. No migration and no schema change (this story only wires
  existing control-plane services), so the drift guard stays green.
- The chain is unit-tested in `internal/api` with stub middleware/handlers
  (envelope shape + request id, `chain` order, `Recover` panic→500 and
  pass-through, healthz/readyz open, login reaches its handler, audit guarded by
  platform-admin with 403 + envelope, the usage `scope → rate-limit → handler`
  order, over-limit `429`, unauthenticated `401`, unknown route `404` envelope,
  and the seam route groups returning `404`). The golden path is an e2e over the
  **real** control-plane Postgres (`test/e2e/api_router_e2e_test.go`): the
  assembled router served over a real listener drives healthz/readyz open,
  anon `/admin/audit` → `401` object envelope with `X-Request-Id`, seed a real
  `settings.update` audit row, login through the mounted route → session cookie +
  CSRF, platform admin reads the audit log through the full chain, and a
  non-admin is refused `403`. `internal/api` is not in the
  `.ci/coverage-packages.txt` gate list, so no coverage threshold is enforced on
  it (consistent with the other control-plane HTTP packages).
- Sources/documents/jobs/settings/members/api-keys and the admin-tenant routes
  are **seams, not stubs**: they are intentionally unregistered, so an
  unregistered path yields the `not_found` envelope their handlers slot into in
  later EPIC-04 stories (04.3–04.6). Session-based `/v1` tenant resolution (a
  `tenant.Resolver` on the request path) is likewise deferred — the only mounted
  tenant-scoped route (`GET /v1/usage`) derives its tenant from the API key, so
  no resolver is on the STORY-04.1 hot path.
- `ragctl serve` is the sole entrypoint (ADR-0009): it builds the router, starts
  the rate-limiter idle sweep and the usage-counter flush loop on the
  signal-cancelled context, and shuts the HTTP server down gracefully — no
  separate flag-parsing path.
- The rate-limiter-after-auth ordering is documented as a deliberate,
  intent-preserving divergence from SPEC-07 §1's abstract sequence; SPEC-07 §1 is
  the higher layer and is not contradicted (the concern is present and enforced),
  only realised in the position the credential-keyed limiter requires.
