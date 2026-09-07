# ISSUE-0035: Query log and feedback

**Type:** Feature · **Status:** Done · **Story:** STORY-08.8 · **Traces:** FR-RET-09/10, SPEC-06 §5.4, SPEC-07 §2/§2g, SPEC-09 §2, C-1, C-3, C-4, ADR-0003, ADR-0058

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-08.8 for traceability; the backlog
> story remains the authoritative work item.

## Summary
STORY-08.5/08.6 left a `QueryLogger.Log(ctx, QueryRecord)` seam on `internal/answer`,
called on BOTH the grounded and refusal paths, with `internal/query` wiring a nil
no-op. This story implements the real logger and the two user-facing endpoints:
every answered query is logged **asynchronously** to the tenant's `query_log`
(FR-RET-09), users can rate an answer via `POST /v1/feedback` (FR-RET-10), and an
admin can review both via `GET /v1/queries`. This is the last EPIC-08 story —
EPIC-08 is now complete (39/39).

## Scope
- New `internal/querylog` package:
  - `Logger` (implements `answer.QueryLogger`): maps `QueryRecord` → `Record`,
    persists on a background goroutine with its own fresh `tenant.DB` + bounded
    context; failures logged without content (C-4) and swallowed; `Wait()` drains.
  - `Service`: `Feedback` (validate ±1, upsert keyed by `query_id`, unknown → 404)
    and `List` (keyset page of `query_log` + joined `query_feedback`).
  - `TenantStore` (SQL over `*tenant.DB`): `Insert`, `UpsertFeedback`, `List`.
  - `Handlers`: `POST /v1/feedback` (`query` scope), `GET /v1/queries` (`admin` scope).
- `internal/api/router.go`: mount the two routes (`Feedback` query scope, `QueryList`
  admin scope) + `Deps` fields.
- `internal/api/openapi.go`: `feedback` + `queryList` operations added to the
  code-derived route table; `api/openapi.yaml` regenerated.
- `internal/cli/api_server.go`: build the `Logger`/`Service`/`Handlers`, set
  `answer.Service.Logger`, wire the handlers into `Deps`.
- Docs: SPEC-07 §2g + route-table row, SPEC-06 §5.4, ADR-0058, this issue, backlog.
- Not in scope: the retrieve/rerank/answer/query core (untouched beyond wiring the
  logger into the existing seam); a numeric eval harness (EPIC-12 / SPEC-06 §8).

## Resolution
- **Async logging (FR-RET-09):** `Logger.Log` never blocks or fails the response.
  It maps the record synchronously, then a goroutine opens a fresh `tenant.DB` from
  the resolver (pools cached) with its own bounded context — the request context is
  already cancelled once the response returns — and inserts. A resolve/write failure,
  read-only (suspended) tenant, or panic is logged without any query content (C-4)
  and swallowed. **ponytail:** best-effort fire-and-forget, one goroutine per query,
  unbounded; upgrade path is a bounded worker pool/queue with backpressure (ADR-0058).
- **Feedback (FR-RET-10):** `POST /v1/feedback` body `{query_id, rating, comment?}`,
  rating ∈ {1,-1} (matches the `query_feedback` check) → 400 otherwise; idempotent
  upsert on the `query_id` primary key (last write wins); unknown `query_id` → 404.
  Ownership is structural: the write targets the tenant's own database (C-1, ADR-0003).
- **Admin visibility:** `GET /v1/queries?limit&cursor` returns each `query_log` row
  (retrieved ids/scores, grounded, model, timings, tokens) LEFT JOINed to its
  feedback, newest-first with `(created_at, id)` keyset pagination. Never returns the
  answer text or another tenant's rows.
- **Id round-trip:** `query_log.id` is a uuid; the SPEC-06 §6 `q_<uuid>` response id
  is the same value prefixed. The prefix is stripped on write and re-applied on read
  so the id a client receives from `POST /v1/query` feeds straight back into feedback
  and the admin listing.
- **No migration:** `query_log` / `query_feedback` already exist in `schemas/tenant.sql`
  and tenant migration `00001`; the schema-drift guard and `ExpectedTenantVersion`
  are unchanged.

## Tests
- Unit (`internal/querylog`, hermetic — fake store + resolver, a `*tenant.DB` being
  unforgeable): `QueryRecord`→columns mapping; `Log` does not block on a slow write;
  a write / resolve error is swallowed; an unparseable id or empty tenant spawns no
  work; feedback validates the rating and query id, upserts, and maps unknown → 404;
  list paginates (limit+1) and rejects a bad cursor; handler scope guard (no tenant →
  401) and error→status mapping.
- e2e (`test/e2e/querylog_e2e_test.go`, real Postgres, stubbed embedder+LLM): a
  grounded query's `query_log` row appears via a bounded poll with the right question,
  grounded flag, retrieved chunk ids+scores and model; a refusal query is logged
  grounded=false; `POST /v1/feedback` writes and re-writes (last-write-wins) and 404s
  an unknown id; `GET /v1/queries` lists both queries with the feedback joined.
- OpenAPI regenerated; drift + contract guards green. Isolation suite green.
