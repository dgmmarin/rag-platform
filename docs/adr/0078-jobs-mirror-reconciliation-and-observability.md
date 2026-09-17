# ADR-0078: Reconcile the jobs mirror against River, and add a local observability stack

**Status:** Accepted · **Date:** 2026-09-17 · **Requirements:** FR-OBS-01, NFR-REL-02 · **Decisions:** SPEC-08 §3/§4, SPEC-10 §2/§5, ADR-0005, ADR-0067 · **Relates:** ADR-0011, ADR-0077

## Context
A cancelled sync kept showing `running` in the jobs mirror and blocked re-syncing its source. The
mirror is a control-plane table separate from River's own job table; the worker's `logMiddleware`
writes the mirror's terminal status only while a live worker executes the job. When a worker process
dies and River later marks that job `cancelled` or `discarded` with no live worker attached, no
middleware runs, so the mirror row stays `running` forever. The unique index
`jobs_one_active_sync_per_source` then blocks any new sync for that source.

The first read of this symptom suspected a broken canceller. It was not. `internal/cli/enqueue.go`
defines `riverCanceller`, wired at `internal/cli/api_server.go` (`jobsSvc.Canceller = riverCanceller{...}`),
and it calls `river.Client.JobCancel` correctly. A stale comment in `internal/cp/jobs/jobs.go` ("nil
until EPIC-09 wires it") predated this wiring and is corrected by this work. The real fault is mirror
drift: the mirror and River can disagree about a job's state, and nothing closed that gap.

Job activity was also not observable end to end. `serve` and the worker each expose a Prometheus
`/metrics` endpoint and write structured JSON logs to stdout, but nothing local scraped, stored, or
showed them, and the worker logged only `job started` / `job finished` at the River level (kind and
River id, no mirror id, tenant, or source). An operator could not answer "what happened to this
source's syncs?" without a manual SQL query.

## Decision
**Reconcile the mirror against River, instead of making every River finalisation path call back into
the mirror.** A single statement drives any non-terminal mirror row whose linked River job has already
gone terminal to the matching terminal mirror status:

```sql
update jobs j
   set status      = (case r.state when 'completed' then 'succeeded'
                                    when 'discarded' then 'failed'
                                    else 'cancelled' end)::job_status,
       finished_at = coalesce(j.finished_at, now()),
       error       = coalesce(j.error, 'reconciled from river state ' || r.state)
  from river_job r
 where r.id = j.river_job_id
   and j.status in ('queued','running')
   and r.state  in ('cancelled','discarded','completed')
returning j.id, j.river_job_id, j.status
```

A `river_job_id` that is null (a jobs-row-only kind, no linked River job) is out of scope and left
untouched. The statement is idempotent: a run that heals nothing changes nothing.

- **Home:** `internal/cp/jobs/reconcile.go`. `PoolDB.Reconcile` runs the statement against the
  control-plane pool; `Reconciler` wraps it to log one `job reconciled` event per healed row. It reads
  and writes only the control-plane `jobs` and `river_job` tables, never a tenant database (ADR-0003).
- **Cadence:** once on worker startup, before the queue starts fetching, so a restart heals drift left
  by the previous process's crash immediately. Then periodically: a River `PeriodicJob`
  (`ReconcileJobsArgs`, kind `reconcile_jobs`) on the `maintenance` queue, scheduled every 60 seconds
  with `RunOnStart: true`, so drift that appears while the worker is running (an orphan cancelled
  out-of-band) heals within a minute.
- **Why reconcile instead of closing every finalisation path.** River can finalise a job
  (`cancelled`, `discarded`, `completed`) through several routes: a live worker's own completion, an
  operator cancel with no worker attached, River's stuck-job rescue, or a worker process that crashes
  mid-job. Making every one of those routes write the mirror directly means finding and instrumenting
  each route, and a route added later silently reopens the gap. A periodic reconciler is one place
  that answers "does every mirror row match its River job?" for any route, present or future, at the
  cost of at most a 60-second lag.

**Every job transition emits a structured log event**, same field set everywhere: `event`, `job_id`
(mirror id), `river_job_id`, `kind`, `tenant_id`, `source_id` (when known), `status`, plus
event-specific fields (`stats` on finish, `error` on failure, `reason` on reconcile). Events:
`enqueued` (the `jobs`/`documents`/`sources` service enqueue paths), `cancel_requested`
(`jobs.Service.Cancel`, the API accepting a cancel), `started`/`finished`/`failed`/`cancelled` (worker
`logMiddleware`, which now logs `cancelled` rather than `failed` when the inner call returns
`ctx.Canceled`), and `reconciled` (the reconciler). A source's whole job history is then one log
query on `source_id` or `job_id`, across enqueue, execution, and any later heal.

**A new opt-in `obs` compose profile** (mirrors ADR-0011's `app` profile pattern) adds four local
services: Prometheus (scrapes `serve` and the worker's `/metrics` endpoints), Loki (log storage),
Promtail (ships logs to Loki), and Grafana (dashboards over both). `serve` and the worker run on the
host under `mise`, not in containers, so there are no container logs to scrape. Promtail instead tails
the host log files the detached service tasks already write, `.run/serve.log` and `.run/worker.log`,
parsing each JSON line and promoting `service`, `event`, `kind`, `status` to labels. `tenant_id` and
`source_id` stay in the parsed line, queryable with `| json`, but are not promoted to labels, since
their cardinality would bloat the Loki index. File-tail was chosen over container-log scraping
because the log source is the host
filesystem, not a container runtime; the alternative (moving `serve`/`worker` into containers) was
out of scope and rejected as a much larger change for an observability task. Host ports are
overridable (`GRAFANA_PORT`, `PROMETHEUS_PORT`, `LOKI_PORT` in `.env`) so the stack does not collide
with another local stack using the same defaults. `mise run obs` starts the profile and prints the
Grafana URL; `mise run obs-down` stops it.

## Consequences
- **The cancel/mirror bug is fixed by healing drift, not by rewriting the cancel path.** The cancel
  API, `riverCanceller`, and River's own cancel handling are unchanged; this ADR closes the gap that
  formed after a cancel or crash left the mirror unfinalised.
- **A production correctness fix fell out of the e2e test.** `TestReconcileHealsOrphanedMirrorRow`
  ran the reconcile statement against a real control-plane Postgres for the first time (the unit tests
  use a fake store) and it failed: the `CASE` expression assigning to the enum column `jobs.status`
  has no target type, so Postgres rejects it as text against `job_status` (error 42804). The fix casts
  the `CASE` result to `::job_status`. Without this cast the reconciler cannot run against a real
  database at all; the unit tests could not have caught it because the fake store has no enum type to
  reject the assignment.
- **A stuck source now self-heals within 60 seconds** of River finalising its job, without an
  operator having to notice or intervene, and a worker restart heals it immediately.
- **The `obs` profile and the `reconcile_jobs` periodic job ship dark to production**: the profile is
  never enabled there, and the reconciler is a no-op when the mirror already matches River, so it adds
  no production risk or dependency.
- **An operator can now trace one source's job history** (enqueue through terminal status, including
  any reconcile heal) from the Grafana Logs panel or a direct Loki query, without SQL against the
  control-plane database.
- **Grafana's default port 3000 can collide** with another local Grafana or dev server; `GRAFANA_PORT`
  in `.env` resolves it without touching compose files.

## Follow-up
- No alerting or paging is configured; the stack is local-dev visibility only.
- No long-term log retention or object-store shipping; Loki uses local filesystem storage with
  default retention.
