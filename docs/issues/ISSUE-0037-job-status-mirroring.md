# ISSUE-0037: Job status mirroring to the `jobs` table

**Type:** Feature · **Status:** Done · **Story:** STORY-09.2 · **Traces:** FR-ADM-02, SPEC-08 §3, C-3, ADR-0005, ADR-0059, ADR-0060

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-09.2 for traceability; the backlog
> story remains the authoritative work item.

## Summary
River is authoritative (ADR-0005); the control-plane `jobs` table is its mirror. This
story connects the two halves STORY-09.1 left apart: producers now enqueue the River
job transactionally with its `jobs` mirror row (linked by a new `river_job_id`), and a
worker middleware writes the row's transitions (queued→running→succeeded/failed/
cancelled) with `worker_id`, `attempt`, `stats` and `error`. The admin surface already
reads only `jobs` (`internal/cp/jobs`), so it now shows real execution without knowing
River exists.

## Scope
- Migration `00007_jobs_river_job_id.sql`: `jobs.river_job_id bigint` + partial unique
  index; `schemas/control_plane.sql` updated in step (drift guard green).
- `internal/worker/mirror.go`: `mirrorMiddleware` (a `river.WorkerMiddleware`) + the
  `mirrorStore` seam + `StatsSink` (ctx-carried, `worker.Stats(ctx)`); `mirror_store.go`:
  `jobsMirror` SQL impl on the control-plane pool + `newWorkerID`. Registered globally
  in `worker.New`; `worker.NewInsertClient` builds the producers' insert-only client.
- Producers switched to transactional enqueue behind a seam (no worker import from the
  domain packages): `documents.ControlJobs.WithQueue(IngestQueue)` and
  `sources.PoolDB.WithQueue(SyncQueue)`; the bridges live in `internal/cli/enqueue.go`
  and are wired in `ragctl serve` (`api_server.go`).
- Stats: `sync_source` and `ingest_document` report their `sink.Stats` to the mirror
  (`worker.Stats(ctx).Set`).
- Docs: ADR-0060, this issue, backlog.
- Not in scope: the running-job cancel *signal* / `Canceller` (STORY-09.4 — this story
  writes only the terminal `cancelled` mapping); River enqueue for kinds whose handlers
  are still TODO (`delete_source` STORY-09.6, platform kinds) — they keep the
  jobs-row-only path; the admin read path (already reads only `jobs`, untouched).

## Resolution
- **Linkage (ADR-0060):** `river_job_id` on `jobs`; the middleware finds the row to
  transition by River's own job id. Nullable + partial-unique so an unlinked River job
  (e2e direct Insert, future scheduler-only kind) matches no row and is a harmless
  no-op.
- **Transactional enqueue (ADR-0005):** producer does `InsertTx(tx, args)` → `INSERT
  jobs(... river_job_id)` → commit; either failure rolls back both. River's
  `UniqueSkippedAsDuplicate` is honoured — `ingest_document` duplicate → existing row
  (idempotent); `sync_source` duplicate → `ErrActiveSyncExists` (409), nothing
  inserted; the `jobs_one_active_sync_per_source` partial index stays as a backstop.
- **Transitions:** `running` before the handler; after it, nil → `succeeded` + stats,
  retryable (`attempt < max`) → `queued` (no `retrying` enum value; SPEC-08 §3),
  final → `failed` + error, `river.JobCancel` → `cancelled`. Terminal writes run on a
  **detached** context so drain/cancel still records them; a mirror failure is logged,
  never propagated (the job's outcome is unchanged). `running`/non-cancel terminals are
  guarded `WHERE status <> 'cancelled'`.
- **Stats:** a ctx-carried `StatsSink` the handler fills and the middleware persists on
  success; handlers stay decoupled from the `jobs` table.
- **worker_id:** `hostname#short-uuid` per worker process (SPEC-08 §3).

## Tests
- Unit (`internal/worker`, hermetic — fake `mirrorStore`, no DB): the happy path marks
  running→succeeded carrying the ctx-reported stats; no stats → `{}`; a retryable error
  → retrying, the final attempt → failed, a `river.JobCancel` → cancelled; a
  mirror-store failure never changes the job's returned error; the error cap. Producer
  legacy (nil-queue) paths keep their existing `documents`/`sources` unit tests green.
- e2e (`test/e2e/worker_e2e_test.go`, real control-plane Postgres, stubbed
  embedder+storage): a producer-style transactional enqueue (River job + linked `jobs`
  row) is consumed and the mirror row is driven queued→running→**succeeded** with
  `worker_id`, `started_at`/`finished_at`, and `chunks_written` in `jobs.stats`. The
  STORY-09.1 ingest + drain e2e still pass (unlinked jobs are a mirror no-op).
- Build/vet green (`-tags e2e` included); full unit suite green; drift guard green.
