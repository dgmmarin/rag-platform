# ADR-0026: Rate limiting — an in-process token bucket per API key and per tenant

**Status:** Accepted · **Date:** 2026-08-22 · **Requirements:** NFR-SEC-07, SPEC-07 §1, FR-ACC-03, C-3

## Context
STORY-03.9 adds the rate-limiting middleware the public API needs (NFR-SEC-07:
"Rate limiting SHALL apply per API key and per tenant"). SPEC-07 §1 fixes the
shape: a "token bucket per API key and per tenant (`settings.limits.qps`); `429`
with `Retry-After`". It is the last EPIC-03 story; the enforcement point plugs
into the STORY-04.1 middleware chain, mirroring how the other 03.x
handlers/middleware defer their router wiring.

Rate limiting is a control-plane concern only (C-3): the limiter counts requests
keyed by opaque control-plane ids (an API key id, a tenant id) and the tenant's
`settings.limits.qps` from the control-plane settings JSON — it never touches
tenant data. The limit key is always derived from the authenticated credential +
resolved tenant already in context (FR-ACC-03), never a client-supplied value.

Sub-decisions this ADR records, because the spec fixes the algorithm and the
per-key/per-tenant scope but leaves the store, the burst allowance, and the
fail-closed behaviour open:
1. In-process buckets vs. a shared/distributed store (e.g. Redis).
2. How the single `settings.limits.qps` maps onto two buckets (per key and per
   tenant) so per-key isolation is actually observable.
3. What happens when the limit cannot be resolved, or no tenant is in context.
4. Where the buckets live and how their memory is bounded.

## Decision
- **In-process token buckets, one `ratelimit.Limiter` per process.** The spec does
  not require a shared store, so the simplest correct design is chosen: each API/
  worker replica keeps its own lazily-refilled token buckets (`bucket.allow`
  refills from elapsed time on demand — no per-bucket goroutine, nothing to run
  while idle). A distributed store (Redis) was rejected for this story: it adds an
  operational dependency and a network hop on the hot path for a facility whose job
  is abuse protection, not exact global accounting. The trade-off is explicit and
  documented below.
- **Two buckets from one `qps`, with asymmetric burst.** Both buckets steady-rate
  at the tenant's `settings.limits.qps` (SPEC-07 §1). To make per-key isolation
  meaningful — otherwise the per-tenant bucket would always bite first and the
  per-key bucket would be redundant — the per-tenant bucket is sized *looser* via a
  burst multiplier (`RateLimitTenantBurst`, default 2×) than a single key
  (`RateLimitKeyBurst`, default 1×). A request must pass BOTH buckets. The
  per-tenant bucket is the aggregate ceiling across all of a tenant's keys and
  sessions; the per-key bucket caps any single credential. A session-authenticated
  request carries no key id and is limited by the tenant bucket only.
- **Fail closed, never silently open.** If `settings.limits.qps` cannot be resolved
  (a settings-store error) the middleware returns `429` rather than admitting the
  request — limiting is never disabled by a backing-store hiccup. A missing/
  malformed `qps` in an otherwise-readable document falls back to a configured floor
  (`RateLimitDefaultQPS`, default 10 = the SPEC-02 §5 default), never "unlimited". A
  request with no resolved tenant in context has nothing to key on and is refused
  `401` (it should never reach the limiter, which runs after tenant resolution).
- **`429` carries `Retry-After` plus `RateLimit-*`.** On refusal the middleware sets
  `Retry-After` (whole seconds until the next token, rounded up — the spec's named
  header) and the complementary `RateLimit-Limit` / `RateLimit-Remaining` /
  `RateLimit-Reset` headers, and writes the SPEC-07 §1 error envelope with code
  `rate_limited`. The inner handler is never reached on a `429`. An optional
  Prometheus counter (`Rejected`) records refusals for the SPEC-07 §1 "metrics"
  acceptance criterion; a nil counter is safe (metrics disabled).
- **Buckets are keyed and swept.** The limiter holds one bucket per `key:<id>` /
  `tenant:<id>` in a mutexed map. `Limiter.Run(ctx, interval)` sweeps buckets idle
  beyond `idleTTL` (10 min) so memory does not grow with the count of ever-seen
  keys; a re-created bucket starts full, so eviction only reclaims memory and never
  tightens a limit. The clock is injected so window boundaries are tested
  deterministically with no real sleeps.

## Consequences
- No migration and no schema change: the limit is read from the existing
  `tenants.settings.limits.qps` (SPEC-02 §5), so the drift guard stays green.
- **Per-replica limiting is approximate under horizontal scale.** With N API
  replicas each holding its own buckets, a tenant/key can burst up to ~N× the
  configured `qps` in the worst case (traffic evenly spread across replicas). This
  is acceptable for NFR-SEC-07's abuse-protection intent and is the standard trade
  for an in-process limiter. If a hard *global* cap is ever required, this is the
  single seam to swap: `ratelimit.Limiter` becomes a shared-store implementation
  behind the same `Middleware`/`LimitFunc` interfaces, with the multi-instance
  limitation removed — no change to callers. That is deferred until a requirement
  demands it.
- Configurable via `internal/config`: `RATE_LIMIT_DEFAULT_QPS` (floor, default 10),
  `RATE_LIMIT_KEY_BURST` (default 1), `RATE_LIMIT_TENANT_BURST` (default 2). Defaults
  keep limiting enabled even when nothing is set.
- Router wiring is STORY-04.1, consistent with every other control-plane middleware
  (ADR-0019/0021/0023): the middleware, limiter, and settings-backed `LimitFunc` are
  built and tested (unit with an injected clock + `net/http/httptest`, and a
  real-Postgres e2e driving the real `RequireScope` → rate-limit chain: a request
  over the per-key limit gets `429` with the headers, and a second key of the same
  tenant is unaffected), with only mounting deferred.
- The limiter reads only control-plane ids and settings JSON (C-3); no tenant
  content is ever involved, and the limit key comes from the authenticated
  credential, never a client parameter (FR-ACC-03).
