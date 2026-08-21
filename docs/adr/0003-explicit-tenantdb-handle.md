# ADR-0003: Explicit TenantDB handle instead of pool-in-context

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** FR-ACC-03, NFR-SEC-01

## Context
With one database per tenant, every data access must target the right database. The handle has to travel from the request or job boundary to the code that runs SQL. Two idiomatic options exist in Go.

## Options
1. Store `*pgxpool.Pool` in `context.Context`; retrieve it wherever needed.
2. A `TenantDB` struct (tenant ID + pool, unexported fields) constructed only by the resolver and passed explicitly to stores/services.
3. Hybrid: tenant *ID* in context for observability; `TenantDB` passed explicitly for data access.

## Decision
Option 3. `tenant.DB` is the only type that can execute SQL against tenant data. It is obtainable solely via `tenant.Resolver.Open(ctx, tenantID)`. Services are built per tenant through a factory (`app.ForTenant(db)`). The tenant ID is also placed in context by auth middleware for logging, tracing and metrics only. An `Unsafe()` accessor exposing the raw pool exists for the migration command and is forbidden elsewhere (enforced by lint rule).

## Consequences
- Forgetting the tenant is a compile error; using the wrong tenant in a multi-tenant worker loop is structurally hard.
- One place to add per-tenant query logging, metrics, timeouts and read-only mode for suspended tenants.
- Slight boilerplate: a thin wrapper over pgx and per-tenant service construction.
- Tests construct a `tenant.DB` against a test database directly; no context fixtures.
