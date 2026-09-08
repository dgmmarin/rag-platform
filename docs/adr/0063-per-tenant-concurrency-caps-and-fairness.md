# ADR-0063: Per-tenant concurrency caps — a snooze-based worker middleware, since OSS River has no partition concurrency

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** SPEC-08 §1 · **Decisions:** ADR-0005, ADR-0060

## Context
SPEC-08 §1: each tenant has a concurrency cap (default 2 ingest jobs) so one tenant
flooding the queue with syncs cannot occupy every worker and starve the others. The spec
suggested "River's unique/partition features", but partition-scoped concurrency is a
River **Pro** feature; the OSS River this platform uses (ADR-0005) has no per-partition
worker cap. So the cap is implemented in our own worker middleware.

## Options / decisions
- **A `tenantLimiter` WorkerMiddleware caps concurrent ingest jobs per tenant by
  SNOOZING the excess.** Before a job runs, the limiter reads `tenant_id` from the job
  args; if that tenant already holds its cap of running ingest jobs, the job is
  `river.JobSnooze`d for a short window rather than run. Snoozing (not blocking) is the
  key choice: a blocked middleware would hold its River worker goroutine, so N+1 jobs of
  one tenant would occupy N+1 goroutines and *cause* the starvation we are preventing. A
  snooze returns the goroutine immediately, freeing it to fetch another tenant's job, and
  re-queues the capped job to retry shortly. Snooze bumps `max_attempts`, so it never
  counts against the retry budget.
- **The limiter is OUTERMOST, before the mirror middleware.** River applies the
  first-registered middleware outermost, so the cap decision (and its snooze) happens
  before the mirror marks the job `running` — a yielded job stays `queued` in the mirror,
  never flickers to running.
- **Ingest queue only; per-process counter.** The cap applies to the ingest queue (where
  SPEC-08 §1 puts it); maintenance/platform pass through. The counter is an in-memory
  `map[tenant]int` guarded by a mutex.
  - **ponytail:** the counter is per worker PROCESS, so with R replicas the effective
    global cap is `cap*R`, not `cap`. That still bounds any one tenant and preserves
    fairness within a process — enough for the SPEC-08 §1 goal (no tenant monopolises a
    worker). **Upgrade path:** a shared (DB/Redis) counter for a strict cluster-wide cap.
- **Configurable.** `--ingest-per-tenant-cap` / `RAGCTL_INGEST_PER_TENANT_CAP`
  (default 2) plumbs through to `worker.Deps.IngestPerTenantCap`.

## Consequences
- A tenant with many queued syncs runs at most `cap` at once per worker; its excess
  snoozes and yields, so other tenants' jobs are fetched and served promptly.
- No blocked worker goroutines: the snooze design keeps the worker pool available for
  other tenants, which is what makes the cap *fair* rather than merely a throttle.
- The cap is soft under multiple replicas (cap×R) — acceptable for v1, with a documented
  path to a hard cluster-wide cap.
- The limiter reads only `tenant_id` from the args; it needs no tenant DB and adds no
  per-job query.
