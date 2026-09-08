# Runbook: A stuck or failing job

**Traces:** SPEC-08 §1/§3, ADR-0005. **Alerts:** `JobFailuresHighPerKind`,
`JobQueueDepthHigh` ([observability](observability.md#job-failures)).

Jobs run on River (a Postgres-backed queue); the control-plane `jobs` table is the mirrored
history view, not the queue itself (ADR-0005). "Stuck" means a job is not making progress —
wedged running, repeatedly retrying, or a queue backing up behind it.

## Identify

1. From the alert label and the jobs dashboard, note the **kind**, **queue**, and **tenant**.
2. Read the job in the control-plane `jobs` mirror: its state, `attempt`, `worker_id`, and
   last error. Cross-check the running `ragctl work` logs (structured, carry `job_id`).
3. Distinguish the case:
   - **Repeatedly failing** (attempts climbing, same error) — a bad input/source or a
     downstream outage.
   - **Wedged running** (claimed by a worker, no progress, no heartbeat) — a crashed or hung
     worker.
   - **Backlog only** (jobs available, few running) — under-provisioned or paused workers.

## Act

- **Repeatedly failing:** fix the root cause. If it is a provider, see
  [Provider outage](provider-outage.md). If it is one source's config/credentials, correct
  the source. Then let it retry, or cancel + re-enqueue a fresh run.
- **Cancel a job:** cancelling a queued job drops it from River immediately (the worker never
  claims it); cancelling a running job signals River, which stops the handler between
  documents and lets the mirror record the terminal state (STORY-09.4). Ingestion commits per
  document (SPEC-05 §5), so a cancel/crash never leaves a partially-updated document visible.
- **Wedged worker:** drain and restart the worker (`ragctl work` stop is graceful — it waits
  for in-flight jobs; SPEC-08 §3). River re-makes an abandoned job available for another
  worker; no manual unlock needed.
- **Backlog:** scale worker concurrency (`RAGCTL_INGEST_CONCURRENCY` /
  `RAGCTL_MAINTENANCE_CONCURRENCY` / `RAGCTL_PLATFORM_CONCURRENCY`); the per-tenant ingest cap
  keeps one noisy tenant from starving the others (SPEC-08 §1). See
  [Queue depth](observability.md#queue-depth).

## Verify

- The job reaches a terminal state (completed/cancelled/failed) in the mirror; the queue
  drains; the alert clears. For a re-run, confirm the tenant's documents/chunks updated as
  expected (ingestion dashboard).
