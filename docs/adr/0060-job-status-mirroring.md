# ADR-0060: Job status mirroring — River authoritative, the `jobs` table a middleware-driven mirror linked by `river_job_id`, producers enqueuing transactionally

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-ADM-02, SPEC-08 §3, C-3, ADR-0005 · **Decisions:** ADR-0010, ADR-0059

## Context
ADR-0005 makes River (on the control-plane database) the authoritative queue and the
control-plane `jobs` table its mirror "for the admin UI and long-term history".
FR-ADM-02 / SPEC-08 §3: worker middleware writes the `jobs` row transitions
(queued→running→succeeded/failed/cancelled) with `worker_id`, `attempt`, `stats` and
`error`, and the admin UI reads **only** `jobs`, never River internals.

Until this story the two halves were disconnected: producers (the documents and
sources HTTP handlers) wrote a `jobs` row directly and enqueued **no** River job;
STORY-09.1 built the worker but only tests inserted River jobs. STORY-09.2 joins them —
producers enqueue the River job transactionally with its mirror row, and a worker
middleware keeps that row current. Scope is the linkage + the transactional enqueue
for the two live kinds (`ingest_document`, `sync_source`) + the mirror middleware; not
the running-job cancel *signal* (STORY-09.4), and not the kinds whose handlers are
still TODO (they keep the jobs-row-only path until their story).

## Options / decisions
- **Link the mirror row to its River job with a `river_job_id` column on `jobs`.**
  Migration 00007 adds `jobs.river_job_id bigint` with a partial unique index
  (non-null). The alternative — carrying the `jobs.id` uuid inside the River args —
  needs no migration but couples the args to the mirror and complicates uniqueness
  hashing; the column keeps `jobs.id` the stable admin-facing key, lets the middleware
  find the row by River's own always-present job id, and makes an unlinked River job
  (an e2e's direct Insert, a future scheduler-only kind) simply match no row. The
  column is nullable so pre-migration rows and not-yet-River kinds are valid.

- **Producers enqueue the River job and the mirror row in ONE transaction.** In
  `ragctl serve` an INSERT-ONLY River client (no workers, never started) is injected
  into the documents and sources producers behind a narrow seam (`IngestQueue` /
  `SyncQueue`, implemented in `internal/cli` so the domain packages never import
  `worker` and no import cycle forms). The producer does `InsertTx(tx, args)` →
  `INSERT jobs (... river_job_id) RETURNING id` → commit: either failure rolls back
  both, so there is never an orphan row or an orphan job (ADR-0005's "transactional
  enqueue with the row that created it"). A nil seam keeps the old jobs-row-only path,
  which hermetic unit tests use.
  - **Uniqueness/idempotency:** River's `UniqueSkippedAsDuplicate` result is honoured.
    For `ingest_document` a duplicate returns the existing mirror row (idempotent);
    for `sync_source` a duplicate is the same conflict the partial unique index
    `jobs_one_active_sync_per_source` catches, mapped to `ErrActiveSyncExists` (409)
    with nothing inserted. The DB partial index stays as a backstop for a legacy row
    River cannot see. `delete_source` (handler is STORY-09.6) and the platform kinds
    keep the jobs-row-only path until their story wires a River handler.

- **A `river.WorkerMiddleware` owns every transition; it never fails a job.** Global
  middleware wraps each worked job: `running` (with `worker_id`, `started_at`,
  `attempt`) before the handler, then exactly one terminal transition after, chosen
  from the handler's error and River's own `Attempt >= MaxAttempts` rule (the same test
  River uses to discard vs retry): nil → `succeeded` + stats; a retryable error →
  back to `queued` (job_status has no `retrying`; SPEC-08 §3 maps retrying→queued);
  the final attempt → `failed` + error; a `river.JobCancel` → `cancelled` (the terminal
  cancel mapping — the running-job cancel *signal* is STORY-09.4). Each write runs on a
  short **detached** context (`context.WithoutCancel`) so a terminal transition is
  still recorded when the job's own context is already cancelled (drain/cancel), and a
  mirror failure is logged, never propagated — the mirror is a side view, not the
  queue's authority. `running` and the non-cancel terminals are guarded
  `WHERE status <> 'cancelled'` so a cancel that already landed on the row is never
  overwritten.

- **Stats reach the mirror through a context sink.** The middleware puts a `StatsSink`
  in the job context; a handler that produces stats (`sync_source` and
  `ingest_document` already build a `sink.Stats` whose JSON tags match `jobs.stats`)
  calls `worker.Stats(ctx).Set(...)`; the middleware writes it into `jobs.stats` on
  success. A handler that reports nothing leaves `{}`. This keeps handlers decoupled
  from the `jobs` table — the middleware alone owns the mirror. The stored `error` is
  length-capped (**ponytail:** a blunt byte cap; handlers keep secrets/content out of
  error strings, C-4).

- **Admin reads only `jobs` — unchanged.** `internal/cp/jobs` already reads the
  control-plane `jobs` table exclusively (C-3) and never touches River tables; this
  story feeds that view rather than changing it.

## Consequences
- The admin jobs view now reflects real execution: a job it shows as `running` is
  being worked, `succeeded`/`failed` carry timings and stats, and `worker_id` says
  where it ran — all without the UI knowing River exists.
- River stays authoritative: a mirror write can fail or lag without affecting the
  queue, and the worker never fails a job because of the mirror.
- One subtlety for a future reader: `jobs.status` has no `retrying` — a pending retry
  shows as `queued` (SPEC-08 §3), and `attempt` is what distinguishes a first queue
  from a re-queue.
- The migration is additive (a nullable column + partial index); the drift guard
  (`schemas/control_plane.sql`) is updated in step and `ExpectedControlVersion` needs
  no bump (control has no version constant; River's own migrations stay separate,
  ADR-0059).
- The running-job cancel signal (STORY-09.4) will reuse the `cancelled` terminal this
  story already writes; the queued→cancelled path (the API flipping the row) is
  unchanged.
