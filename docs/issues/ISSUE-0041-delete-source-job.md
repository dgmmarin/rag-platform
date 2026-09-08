# ISSUE-0041: Delete-source job

**Type:** Feature · **Status:** Done · **Story:** STORY-09.6 · **Traces:** FR-SRC-12, SPEC-08 §1, ADR-0003, ADR-0005, ADR-0060, ADR-0064

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs
> (`docs/adr/`). This issue records STORY-09.6 for traceability.

## Summary
Builds the real `delete_source` handler (was a fail-loud TODO worker): it opens the
tenant DB per job (ADR-0003) and removes the source's documents, versions, chunks, crawl
state, connector state and products, reporting the counts to `jobs.stats` (FR-SRC-12).
The sources producer now enqueues the `delete_source` River job transactionally with its
mirror row.

## Scope
- `internal/documents/delete_source.go`: `TenantStore.DeleteSource` (one tenant-DB
  transaction; documents cascade to versions+chunks, then crawl_pages/connector_state/
  products) + `DeleteSourceStats`.
- `internal/worker/delete_source.go`: `deleteSourceWorker`; registered in `worker.New`
  in place of the `todoWorker[DeleteSourceArgs]`.
- Producer: `sources.SyncQueue` gains `EnqueueDeleteSourceTx`; `EnqueueJob` routes
  `delete_source` through the transactional River enqueue (dup → existing row,
  idempotent). Implemented in `internal/cli/enqueue.go`.
- Docs: ADR-0064, this issue, backlog.
- Not in scope: removing the control-plane `sources` row (that stays the STORY-04.3
  delete lifecycle); the other TODO handlers (reindex, provision/delete tenant).

## Resolution
- **Cascade in one transaction:** `delete from documents where source_id` cascades to
  versions and chunks; crawl_pages, connector_state and products are deleted by
  source_id. Chunks are counted before the cascade for exact stats. Idempotent, so the
  5-retry budget is safe.
- **Handler:** validates the ids (bad → `JobCancel`), runs `DeleteSource`, reports
  `{documents, chunks, crawl_pages}` via the mirror stats sink.
- **Producer:** `delete_source` now enqueues a real River job (linked mirror row) so the
  worker consumes it; a duplicate active delete returns the existing row.

## Tests
- e2e (`test/e2e/worker_e2e_test.go`, real Postgres): a document is ingested for a source
  (creating chunks) and crawl state seeded; a `delete_source` is enqueued and consumed;
  the mirror row reaches `succeeded`; documents/chunks/crawl_pages for the source are all
  gone and `jobs.stats` reports non-zero documents, chunks and crawl_pages.
- Build/vet green (`-tags e2e`); full unit suite green.
