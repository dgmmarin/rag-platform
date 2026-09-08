# ADR-0064: Delete-source job — a tenant-DB cascade removing a source's content, driven by a real River handler

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-SRC-12, SPEC-08 §1 · **Decisions:** ADR-0003, ADR-0005, ADR-0060

## Context
FR-SRC-12 / SPEC-08 §1: deleting a source removes its documents, versions, chunks and
crawl state, with stats reported. STORY-04.3 marks a source `deleting` and enqueues a
`delete_source` job; until now that job's handler was a registered-but-TODO worker
(fail-loud) and the producer wrote only a jobs row. This story builds the handler and
switches the producer to enqueue the real River job.

## Options / decisions
- **A `TenantStore.DeleteSource` does the removal in one tenant-DB transaction.** It
  deletes `documents where source_id = $1` (cascading to `document_versions` and `chunks`
  via the schema's `ON DELETE CASCADE`), then `crawl_pages`, `connector_state` and
  `products` for the source — the full footprint of source-scoped tenant content. Chunks
  are counted before the cascade so the stats are exact. It runs on the tenant database
  reached per-job through the resolver (ADR-0003, C-3); the control plane is never
  touched by the delete (the `sources` row and its lifecycle stay STORY-04.3's job).
- **Idempotent, so the job is safe to retry.** Re-running deletes already-gone rows as a
  no-op, which matches the `delete_source` retry budget (5). One transaction means a
  source is either fully removed or untouched.
  - **ponytail:** single unbatched DELETEs, so a pathologically large source holds row
    locks for the transaction. **Upgrade path:** batch like `CollectGarbage` and report a
    more-remaining flag for the worker to reschedule.
- **The handler reports stats through the mirror sink.** `deleteSourceWorker` opens the
  tenant DB, runs `DeleteSource`, and calls `worker.Stats(ctx).Set(...)` with the
  `{documents, chunks, crawl_pages}` counts, so they land in `jobs.stats` (SPEC-08 §3). A
  malformed tenant/source id is a permanent `JobCancel` (never succeeds on retry).
- **The producer now enqueues `delete_source` through River.** The sources `SyncQueue`
  seam gains `EnqueueDeleteSourceTx`; `EnqueueJob` routes both `sync_source` and
  `delete_source` through the ADR-0060 transactional enqueue (River `InsertTx` + mirror
  row). A River duplicate of an active delete is idempotent — the existing mirror row is
  returned (unlike a `sync_source` duplicate, which is the 409 "one active sync"
  conflict).

## Consequences
- Deleting a source now actually reclaims its tenant content asynchronously, with the
  removed counts visible in the admin jobs view.
- The `delete_source` kind is no longer a fail-loud TODO; the remaining TODO workers
  (reindex, provision/delete tenant) are unchanged.
- Products and connector state are cleaned up alongside the FR-SRC-12 set, so a re-created
  source never inherits stale state.
- The control-plane `sources` row removal remains the responsibility of the delete
  lifecycle (STORY-04.3), not this job — this job clears tenant content only.
