# ISSUE-0078: Jobs mirror drift heals against River, and job transitions log as structured events

**Type:** Bug/Feature · **Status:** Done · **Priority:** High · **Traces:** SPEC-08 §3/§4, ADR-0078, ADR-0005

## Summary
A cancelled sync kept showing `running` and blocked re-syncing its source. The cause was mirror drift,
not a broken canceller: the control-plane jobs mirror row only reaches a terminal status while a live
worker executes the job and its middleware writes the result. When the worker process that ran a job
dies and River later finalises that job (`cancelled`, `discarded`, `completed`) with no worker
attached, the mirror row never leaves `running`, and the unique index
`jobs_one_active_sync_per_source` then blocks any new sync for that source.

`internal/cli/enqueue.go`'s `riverCanceller`, wired at `internal/cli/api_server.go`, already called
`river.Client.JobCancel` correctly; a stale comment in `internal/cp/jobs/jobs.go` claiming the
canceller was still unwired predated that wiring and is corrected here. This adds a reconciler that
heals the drift, plus a structured log event for every job transition so an operator can trace a
source's job history without SQL.

## What was built
- **Reconciler (`internal/cp/jobs/reconcile.go`).** `PoolDB.Reconcile` runs one statement: for a
  mirror row in `queued`/`running` whose linked `river_job` is `cancelled`/`discarded`/`completed`, it
  sets the mirror status to the matching terminal (`cancelled`/`failed`/`succeeded`), stamps
  `finished_at` if unset, and records a reason. A null `river_job_id` (a jobs-row-only kind) is left
  untouched. `Reconciler` wraps `PoolDB.Reconcile` and logs one `job reconciled` event per row healed.
- **Cadence.** The worker runs the reconciler once on startup, before the queue starts fetching, and
  again every 60 seconds via a River `PeriodicJob` (`ReconcileJobsArgs`, kind `reconcile_jobs`) on the
  `maintenance` queue with `RunOnStart: true`.
- **Lifecycle log events**, one per job transition, same field set: `event`, `job_id`, `river_job_id`,
  `kind`, `tenant_id`, `source_id`, `status`, plus `stats` on finish, `error` on failure, `reason` on
  reconcile.
  - `enqueued` — the `jobs`/`documents`/`sources` service enqueue paths.
  - `cancel_requested` — `jobs.Service.Cancel`, when the API accepts a cancel.
  - `started`, `finished`, `failed`, `cancelled` — worker `logMiddleware`. A job whose context is
    cancelled now logs `cancelled`, not `failed`.
  - `reconciled` — the reconciler, for each row it heals.
- **Production correctness fix.** The reconcile statement's `CASE` expression assigns to the enum
  column `jobs.status` with no target type; Postgres rejected it as text against `job_status` (error
  42804) the first time it ran against a real database. Fixed by casting the `CASE` result to
  `::job_status`. The unit tests (fake store, no real enum) could not catch this; the e2e test running
  against live Postgres did.
- **Correction of record.** The running-job canceller was already wired
  (`riverCanceller` → `river.Client.JobCancel`, `internal/cli/api_server.go`); the "cancelled but still
  running" symptom was mirror drift, not a missing or broken canceller. The stale comment in
  `internal/cp/jobs/jobs.go` claiming the canceller was unwired is removed.

## Tests
- `internal/cp/jobs` unit tests: a `running` mirror row with a `cancelled` `river_job` heals to
  `cancelled` with `finished_at` set; a row already terminal is untouched; `completed`/`discarded`
  River states map to `succeeded`/`failed`; a null `river_job_id` row is ignored.
- `internal/worker` unit tests: `logMiddleware` logs `cancelled` (not `failed`) when the inner call
  returns `ctx.Canceled`; each lifecycle event carries the required field set, captured via a test
  `slog` handler.
- e2e `TestReconcileHealsOrphanedMirrorRow` (`-tags e2e`): inserts a `river_job` in state `cancelled`
  with a linked mirror row still `running`, runs `Reconciler.Reconcile` against real control-plane
  Postgres, asserts the row heals to `cancelled` with `finished_at` and `error` set, and that a second
  run is a no-op. This test is what surfaced the `::job_status` cast bug.

## Related
ADR-0078, ADR-0005 (River job queue), ISSUE-0079 (the observability stack this issue's log events
feed).
