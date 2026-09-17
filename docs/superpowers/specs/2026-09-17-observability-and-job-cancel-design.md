# Local observability stack + job-action visibility + cancel/mirror integrity

**Date:** 2026-09-17 · **Status:** Implemented (ADR-0078) · **Traces:** SPEC-08 §3/§4, SPEC-10 §2/§5, ADR-0005, ADR-0067 · **Relates:** ADR-0011 (compose app profile), ISSUE-0077 (detached service tasks)

## Problem

Two related operator gaps surfaced while running the local stack.

1. **A cancelled sync kept showing `running` and blocked re-syncing the source.** Investigation
   showed the cancel itself reached River (the River job was `cancelled`), but the control-plane
   **jobs mirror** row stayed `running`. The mirror's terminal transition is written only by the
   worker's middleware **while a live worker executes the job**. When River finalises a job with no
   live worker running it — an orphan whose worker died, then cancelled or discarded out-of-band — no
   middleware runs, so the mirror never leaves `running`. The unique index
   `jobs_one_active_sync_per_source` (on `source_id` where status in `queued`/`running`) then blocks
   any new sync for that source. This is a **mirror-vs-River drift**, not a broken canceller:
   `internal/cli/enqueue.go` `riverCanceller` is wired at `internal/cli/api_server.go` and calls
   `river.Client.JobCancel` correctly. (The stale comment in `internal/cp/jobs/jobs.go` — "nil until
   EPIC-09 wires it" — predates the wiring and is corrected by this work.)

2. **Job activity is not observable.** The services emit Prometheus metrics (`serve` `/metrics`,
   worker `:9091/metrics`) and structured JSON logs to stdout, but there is nothing local to scrape,
   store, or visualise them, and the worker logs only River-level `job started` / `job finished`
   (kind + River id). An operator cannot answer "what happened to this source's syncs?" without SQL.

## Goals

- The jobs mirror never gets stuck out of step with River: a terminal River job always drives its
  mirror row to the matching terminal state, whether or not a worker finalised it.
- Every job transition is a structured log event carrying enough identity (`job_id`, `river_job_id`,
  `kind`, `tenant_id`, `source_id`, `worker_id`, `status`) to trace one source's history.
- A one-command local observability stack (metrics + logs) shows job throughput, queue depth,
  durations, the embedding-reuse rate, and searchable job logs.

### Non-goals

- No production deployment of the stack; it is a local-dev, opt-in compose profile only.
- No new alerting/paging, no long-term retention or object-store log shipping.
- No change to River, to the cancel API contract, or to the queued/running cancel behaviour that
  already works. This adds reconciliation and visibility around them.
- No move of `serve`/`worker` into containers; they keep running on the host (via `mise`), and the
  log stack tails their files.

## Design

### Part A — Mirror reconciliation (the real cancel fix)

A **reconciler** finalises any mirror row that River has already finalised. It closes the orphan drift
without touching the cancel path that already works.

- **Query.** For mirror rows in a non-terminal status (`queued`/`running`) whose linked `river_job`
  is terminal (`cancelled`/`discarded`/`completed`), set the mirror status to the mapped terminal
  (`cancelled`/`failed`/`succeeded`), stamp `finished_at` if unset, and record a reason. One
  statement, keyed on `jobs.river_job_id = river_job.id`:

  ```sql
  update jobs j
     set status      = case r.state when 'completed' then 'succeeded'
                                     when 'discarded' then 'failed'
                                     else 'cancelled' end,
         finished_at = coalesce(j.finished_at, now()),
         error       = coalesce(j.error, 'reconciled from river state ' || r.state)
    from river_job r
   where r.id = j.river_job_id
     and j.status in ('queued','running')
     and r.state  in ('cancelled','discarded','completed')
  returning j.id, j.river_job_id, j.status;
  ```

  A `river_job_id` that is null (a jobs-row-only kind) is out of scope and left untouched.

- **When it runs.**
  - **On worker startup**, once, before the queue starts fetching — so a restart immediately heals
    drift left by the previous process's crash.
  - **Periodically**, as a River `PeriodicJob` (`reconcile_jobs`, e.g. every 60 s) on the
    `maintenance` queue, so drift that appears while running (orphan cancelled out-of-band) heals
    within a minute. It is idempotent: a run that finalises nothing is a no-op.

- **Reconciled rows emit a `job reconciled` log event** (Part B) so the heal is visible, not silent.

- **Home.** A new `internal/cp/jobs/reconcile.go` with a `Reconciler` that takes the control-plane
  pool and runs the statement; the periodic worker and the startup hook call it. It reads/writes only
  the control-plane `jobs` and `river_job` tables (never a tenant DB).

### Part B — Job lifecycle logging

One structured event per transition, same field set everywhere, so a log query on
`source_id`/`job_id` returns a job's whole story.

- **Fields (every event):** `event` (the transition), `job_id` (mirror uuid), `river_job_id`,
  `kind`, `tenant_id`, `source_id`, `worker_id` (when known), `status`, plus event-specific fields
  (`stats` on finish, `error` on failure, `reason` on reconcile).
- **Events and their sources:**
  - `enqueued` — `jobs`/`documents`/`sources` service enqueue paths (mirror row created).
  - `cancel_requested` — `jobs.Service.Cancel` (the API accepted a cancel).
  - `started`, `finished`, `failed` — worker `logMiddleware` (extended from today's start/finish to
    carry the mirror uuid, tenant, source, and status/stats; a returned `ctx.Canceled` logs
    `cancelled`, not `failed`).
  - `rescued` — the River rescue path / stuck-job handling.
  - `reconciled` — Part A.
- **Constraint (C-3/C-4):** events carry ids and counts only — never embedded text, secrets, or
  document content. `stats` is the existing `sink.Stats` shape.
- The worker and `serve` already log JSON via `slog`; these events use the same logger, so they land
  in the same streams Part C ships.

### Part C — Observability stack (`obs` compose profile)

A new **opt-in** compose profile (mirrors ADR-0011's `app` profile), four services, all local:

- **Prometheus** — scrapes `serve` `/metrics` and the worker `:9091/metrics` (host targets via
  `host.docker.internal`). Config: `deploy/obs/prometheus.yml`. Retention default (local).
- **Loki** — single-binary log store, filesystem storage. Config: `deploy/obs/loki.yml`.
- **Promtail** — tails the host log files the detached service tasks already write
  (`.run/serve.log`, `.run/worker.log`), parses each line as JSON, promotes `service`, `event`,
  `kind`, `status`, `tenant_id`, `source_id` to labels/structured metadata, ships to Loki. Config:
  `deploy/obs/promtail.yml`; the `.run/` dir is bind-mounted read-only. (Rationale: `serve`/`worker`
  run on the host, so there are no container logs to scrape; the files are the seam, and they are the
  same structured JSON stdout carries.)
- **Grafana** — provisioned datasources (Prometheus + Loki) and one **Jobs dashboard**
  (`deploy/obs/grafana/`): queue depth (`jobs_queue_depth`), job throughput by kind/status, job
  duration (`jobs_duration_seconds`), failure rate (`jobs_failed_total`), embedding-reuse rate
  (`embed_chunks_reused_total` vs `ingest_chunks_total`), plus a Logs panel over Loki filtered to job
  events. Anonymous admin (local only), no login.

- **mise tasks:** `obs` (`docker compose --profile obs up -d --wait`) and `obs-down`
  (`docker compose --profile obs down`). `obs` depends on infra being up. Because Promtail tails
  `.run/*.log`, the documented flow is `mise run up` → `mise run services` → `mise run obs`, and the
  Grafana URL is printed.

- **Layout.** All stack config under `deploy/obs/`; compose services added to `docker-compose.yml`
  behind `profiles: ["obs"]` so a plain `mise run up` is unaffected.

## Testing

- **Reconciler (unit, `internal/cp/jobs`):** seed a mirror row `running` whose `river_job` is
  `cancelled` → reconcile → mirror `cancelled`, `finished_at` set; a mirror already terminal is
  untouched; a `completed`/`discarded` River row maps to `succeeded`/`failed`; a null `river_job_id`
  row is ignored. Fake/real pool per existing `internal/cp/jobs` test style.
- **Reconciler (e2e, `-tags e2e`):** against the live stack, force an orphan (insert a River
  `cancelled` job + a `running` mirror row), run the reconciler, assert the mirror heals and the
  source is re-syncable (the unique index no longer blocks).
- **Lifecycle logging (unit):** the worker middleware logs `cancelled` (not `failed`) when the inner
  returns `ctx.Canceled`, and each event carries the required field set. Assert via a captured
  `slog` handler.
- **Stack (smoke script, not CI):** `mise run obs`, then assert Prometheus `/-/ready` and both
  targets `up`, Loki `/ready`, Grafana `/api/health`, and that a known job log line is queryable in
  Loki. Documented in `deploy/obs/README.md`; self-skips without Docker.

## Rollout

Local-dev only. The `obs` profile and the `reconcile_jobs` periodic job ship dark to production
(profile not enabled; the reconciler is safe and idempotent everywhere but adds no production
dependency). No data migration. New branch off `main`
(`epic12-observability-and-job-cancel`); the ISSUE-0077 mise tasks are already its first commit.
