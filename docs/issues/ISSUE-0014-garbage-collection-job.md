# ISSUE-0014: Garbage collection job

**Type:** Feature · **Status:** Done · **Story:** STORY-05.9 · **Traces:** SPEC-03 §4, ADR-0008, ADR-0017

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-05.9 for traceability; the backlog
> story remains the authoritative work item. STORY-05.9 completes EPIC-05 (42/42).

## Summary
The per-tenant retention garbage-collection sweep (SPEC-03 §4, the SPEC-08 `gc_tenant`
daily job): remove old non-current document versions, soft-deleted documents past
grace, expired query logs and stale crawl pages, returning the rows removed per class.
The River worker that schedules/drives it daily is STORY-09.1 and is out of scope; this
story exposes the operation for the worker to drive.

## Scope
- `internal/documents/gc.go`: `GCPolicy`, `GCMetrics` and
  `documents.TenantStore.CollectGarbage`, the tenant-side DML reached ONLY through
  `*tenant.DB` (ADR-0003, C-1, C-3). Kept OFF the `documents.Store` interface (ISP,
  the ADR-0038 reindex/sink precedent) — the Service/handlers never GC.
- Not in scope: the River worker / scheduling (STORY-09.1), Prometheus/log emission of
  the returned metrics (the worker's job), and the crawler that would emit a per-source
  sync-generation counter (EPIC-06/07).

## Resolution
- **One operation, four classes.** `CollectGarbage(ctx, db, policy, now)` sweeps, in
  order: non-current `document_versions` older than `VersionRetention` (chunks removed
  by the `chunks.version_id` cascade; a *current* version is excluded via
  `not exists (… documents.current_version = v.id)`, so invariant 2.1 holds),
  `status='deleted'` documents older than `DeletedDocRetention` (their versions and
  chunks cascade on `document_id`), `query_log` older than `QueryLogRetention` (feedback
  cascades on `query_id`), and stale `crawl_pages`.
- **Metrics.** `GCMetrics` reports rows removed per class (`OldVersions`, `DeletedDocs`,
  `QueryLogs`, `CrawlPages`) plus cascaded `Chunks` (informational, excluded from
  `Total`), counted in the same statement snapshot as each delete.
- **Bounded + idempotent.** Every class is a keyset-`LIMIT` CTE delete; `CollectGarbage`
  drains each class in `BatchSize`-bounded statements (default 1000) so a large backlog
  is removed in many short transactions rather than one table-locking delete, and a
  second run over the same `now` removes nothing. `now` is injected for testability.
- **Policy defaults.** `GCPolicy` zero fields fall back to the SPEC-03 §4 day windows
  (30/30/90 d) and a positive batch cap; a negative batch clamps to the default so a bad
  config can never issue an unbounded delete.
- **Crawl-page interpretation.** SPEC-03 §4 states the crawl rule as "not seen in 3
  successful syncs" — a generation count the tenant schema does not record (no per-source
  sync counter exists; the crawler is EPIC-06/07). GC approximates it as "not fetched
  within `CrawlPageStale`", which the driving worker supplies as 3×cadence; a zero window
  **skips** the crawl sweep rather than inventing a threshold, and pages with a null
  `last_fetched_at` (pending frontier, never fetched) are left alone.

## Verification
- TDD: `internal/documents/gc_test.go` pins the pure policy/metrics logic (window
  defaults, negative-batch clamp, crawl-skip, `Total` excludes chunks) — watched **red**
  (undefined `GCPolicy`/consts) then **green**.
- e2e (`test/e2e/gc_e2e_test.go`, build tag `e2e`) against a **real enrolled tenant DB**
  (dim 4), reached only through a resolver + `*tenant.DB`: seeds each of the four classes
  with a collectible row (past its window) *and* a retained row (within it) plus live
  data, then proves each collectible row **and its cascade** is removed (old versions +
  their chunks; deleted-doc B + its versions/chunks; old query logs + feedback; stale
  crawl page) while the current version, `live_chunks`, the within-grace document, the
  recent query log and the fresh + never-fetched crawl pages survive; the per-class counts
  are exact (`{OldVersions:2, DeletedDocs:1, QueryLogs:2, CrawlPages:1, Chunks:4}`); and a
  second run is all-zero. `BatchSize=1` exercises the drain loop. Ran **green in ~9 s**.
- `gofmt -l` clean; `go vet` clean; `go test -count=1 ./internal/ingest/...
  ./internal/documents/...` green; pinned `golangci-lint v2.13.1` **0 issues** on
  `internal/documents/...` (the forbidigo `Unsafe()` ban stays satisfied — GC uses only
  `db.QueryRow`/`Exec`).

## Notes / not in scope
- No schema/migration/OpenAPI change: GC reuses existing `document_versions.created_at`,
  `documents.deleted_at`, `query_log.created_at` and `crawl_pages.last_fetched_at`
  columns and the schema's `ON DELETE CASCADE` FKs, so `schemas/tenant.sql` and the
  tenant migration are unchanged and the drift guard stays green. No ADR was needed for a
  retention sweep (it references SPEC-03 §4, ADR-0008 versioning, ADR-0017 deletion/grace).
- Coverage: `internal/documents` is not a gated package.
- `TestTenantIsolationSuite` could not be executed here: its assertions run through
  `docker compose exec`, which is wedged in this environment (a bare
  `docker compose exec postgres psql -c 'select 1'` did not return within 120 s) while the
  DB is healthy — the GC e2e over the direct control/tenant pool ran fine. The
  isolation-relevant `Unsafe()` ban is lint-enforced (green) and the per-tenant role's
  ability to delete its own rows is proven by the GC e2e.
- Ceiling (ponytail, `gc.go`): crawl-page collection is time-based, not the SPEC's
  literal 3-sync count. Upgrade path: add `crawl_pages.last_seen_sync` + a per-source sync
  counter when the crawler lands, and switch the predicate to a generation delta. A single
  `CollectGarbage` call drains the full eligible backlog in `BatchSize` steps; a
  pathological backlog means many iterations in one call (upgrade: return a
  "more-remaining" flag for the worker to reschedule).
