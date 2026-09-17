# ISSUE-0080: Scheduler-enqueued jobs skip the `enqueued` lifecycle event

**Type:** Chore · **Status:** Open · **Priority:** Low · **Traces:** ADR-0078, ISSUE-0078

## Summary
ADR-0078 and ISSUE-0078 added one structured log event per job transition (`enqueued`,
`cancel_requested`, `started`, `finished`, `failed`, `cancelled`, `reconciled`), so an operator can
trace a job's full history by `job_id` or `source_id`. The `enqueued` event fires from the
`jobs`/`documents`/`sources` service enqueue paths, but the worker scheduler's cron-driven syncs
(`internal/worker/scheduler.go`, `enqueueMirrored`) enqueue with a raw `insert into jobs` and bypass
those services, so a scheduled sync's trace starts at `started`, with no `enqueued` event before it.

## Why it matters
A scheduled sync is the most likely source of the orphan mirror row that ADR-0078's reconciler heals
(a worker process dies mid-run, River finalises the job, the mirror is stuck `running`). For exactly
that job, the trace is missing its opening event. Adding `enqueued` logging to `enqueueMirrored` would
close this gap and give scheduled syncs the same complete trace as API-enqueued jobs.

## Proposed change
Log an `enqueued` event inside `enqueueMirrored` (`internal/worker/scheduler.go`), with the same field
set the other lifecycle events use (`event`, `job_id`, `river_job_id`, `kind`, `tenant_id`,
`source_id`, `status`), right after the mirror row insert succeeds. Skip logging when River collapses
the enqueue onto an already-active job (`res.UniqueSkippedAsDuplicate`), since no mirror row is
written in that case.

## Related
ADR-0078 (lifecycle event design), ISSUE-0078 (the lifecycle events this extends).
