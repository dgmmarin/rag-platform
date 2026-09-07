# ADR-0058: Query log and feedback — async best-effort logging through the `QueryLogger` seam, the `/v1/feedback` upsert, and the `/v1/queries` admin listing, all tenant-content via the resolver

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-RET-09, FR-RET-10, SPEC-06 §5.2/§5.4/§6, SPEC-07 §2/§2g, SPEC-09 §2, C-1, C-3, C-4, ADR-0001, ADR-0003 · **Decisions:** ADR-0055, ADR-0056

## Context
FR-RET-09: every query is logged with its retrieved chunk ids, scores, model used,
latency and token counts — **asynchronously**, so logging never degrades the answer
path. FR-RET-10: users can rate an answer (thumbs up/down + optional comment), stored
against the query log. The AC also requires both to be **visible in admin**.

STORY-08.5/08.6 already left the seam: `internal/answer` calls a
`QueryLogger.Log(ctx, QueryRecord)` on BOTH the grounded and refusal paths, and
`internal/query` wired a nil (no-op) logger. The `query_log` and `query_feedback`
tables already exist in `schemas/tenant.sql` / tenant migration `00001` — so this
story needs **no migration**. Scope is the logger implementation + the feedback
endpoint + the admin listing + wiring; not the retrieve/rerank/answer/query core.

## Options / decisions
- **A dedicated `internal/querylog` package fills the seam; the answer/query core is
  untouched.** `querylog.Logger` implements `answer.QueryLogger`; `internal/cli` wires
  it into `answer.Service.Logger` at the composition root. The `QueryRecord` shape
  (retrieved ids+scores, grounded, cited ids, usage, model) is consumed as-is — no new
  fields, so `internal/answer` is not modified. The answer *text* is intentionally NOT
  carried by the seam and stays null in `query_log.answer`; FR-RET-09 does not require
  it and widening the seam would touch the core.

- **Async = best-effort fire-and-forget, one goroutine per query (ponytail).** `Log`
  maps the record synchronously, then spawns a goroutine that opens a **fresh**
  `tenant.DB` from the resolver (pools are cached, so this is cheap) using its OWN
  bounded context — the request context is already cancelled once the response has been
  written, so reusing it would cancel the write. The handle lifecycle problem ("don't
  use a closed/after-response handle") is avoided structurally: the goroutine acquires
  its own handle rather than closing over the request's. A write failure — or a resolve
  failure, a read-only (suspended) tenant, or a panic — is logged **without any query
  content** (C-4) and swallowed; it never reaches the client. `Log` never returns an
  error and never blocks.
  - **Known ceiling:** unbounded goroutine growth under a query flood (no backpressure).
    **Upgrade path:** a bounded worker pool / channel queue that sheds or blocks when
    full. A `Wait()` method drains in-flight writes for a graceful shutdown or a test.

- **`query_log.id` is a uuid; the `q_<uuid>` response id is the same value with the
  prefix stripped.** SPEC-06 §6 mints `q_<uuid>` response ids, but `query_log.id` is a
  native `uuid` (so `query_feedback.query_id` can foreign-key it). The logger strips the
  `q_` prefix on write and the admin listing re-applies it on read, so the id a client
  receives from `POST /v1/query` round-trips straight into `POST /v1/feedback` and back
  out of `GET /v1/queries`, while the stored key stays a uuid.

- **Feedback is an idempotent upsert keyed by `query_id`; ownership is structural.**
  `POST /v1/feedback` (`query` scope, per SPEC-07 §2) validates `rating ∈ {1,-1}`
  (matching the `query_feedback` check constraint) → `400` otherwise. The write goes to
  the tenant's OWN database (resolver + `*tenant.DB`, ADR-0003; the database boundary is
  the tenant boundary, C-1), so a `query_id` from another tenant is simply absent — the
  store checks existence first and returns a clean `404` rather than a foreign-key
  error. `on conflict (query_id) do update` makes a repeat rating last-write-wins.

- **Admin visibility is a new `GET /v1/queries` (`admin` scope), keyset-paginated.**
  SPEC-07 had `POST /v1/feedback` but no query-log listing; this story appends
  `GET /v1/queries` to the route table and adds §2g. It returns each `query_log` row
  (retrieved ids/scores, grounded, model, timings, tokens) LEFT JOINed to its
  `query_feedback`, newest-first with `(created_at, id)` keyset pagination like the
  other admin/list endpoints. It never returns the answer text and, by tenant-DB
  scoping, never another tenant's rows.

- **No migration.** Both tables pre-exist in the tenant schema and mirror; the
  schema-drift guard stays green untouched and `ExpectedTenantVersion` is unchanged.

## Consequences
- Logging cannot slow or fail a query; the worst case under load is bounded-timeout
  goroutines, with a documented upgrade path.
- Tenant isolation is preserved end-to-end: query content, retrieval metadata and
  feedback live only in the tenant database, reached only via the resolver (C-3).
- The `q_`-prefix strip/reapply is the one subtlety a future reader must keep in step
  between the answer id (SPEC-06 §6), the stored uuid, and the two endpoints.
- OpenAPI (`api/openapi.yaml`) gains the two operations from the same code-derived
  route table; the drift and contract guards extend with them (ADR-0028).
