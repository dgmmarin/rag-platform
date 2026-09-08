# ISSUE-0038: Scheduler for cron sources and daily GC

**Type:** Feature · **Status:** Done · **Story:** STORY-09.3 · **Traces:** FR-SRC-11, SPEC-08 §2, ADR-0005, ADR-0060, ADR-0061

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs
> (`docs/adr/`). This issue records STORY-09.3 for traceability; the backlog story is
> the authoritative work item.

## Summary
A leader-elected loop (`internal/worker/scheduler.go`) runs inside `ragctl work` every
30s: it enqueues a `sync_source` for each active cron source whose `next_run_at` is due
(incremental, full every 7th run), advances `next_run_at` from `schedule_cron`, and
enqueues `gc_tenant` daily per active tenant (SPEC-08 §2). A Postgres transaction
advisory lock makes it safe under several worker replicas — no duplicate enqueues.

## Scope
- Migration `00008_sources_sync_run_count.sql`: per-source `sync_run_count` counter;
  `schemas/control_plane.sql` updated in step (drift guard green).
- `internal/worker/scheduler.go`: `Scheduler` (`Run`/`sweep`/`syncSweep`/`gcSweep`), the
  shared `enqueueMirrored` (River `InsertTx` + jobs mirror row, ADR-0060), `nextRun`
  (cron) and `fullSync` helpers. Wired in `internal/cli/worker.go` as a goroutine that
  stops on shutdown before the pool closes.
- `robfig/cron/v3` promoted from indirect to direct (already in the module graph via
  River; no new external dependency).
- Docs: ADR-0061, this issue, backlog.
- Not in scope: cron editing UI/API (sources already carry `schedule_cron`); per-run
  config beyond full-every-Nth; the GC sweep itself (STORY-05.9 handler, wired in 09.1).

## Resolution
- **Leader election:** `pg_try_advisory_xact_lock(schedulerLockKey)` per sweep; a
  non-winner returns immediately. Transaction-scoped, so it releases on commit/rollback
  and never leaks. River uniqueness is a second backstop.
- **Atomic sweep:** the lock, the enqueues, and the `next_run_at`/`sync_run_count`
  updates commit in one transaction — a crash mid-sweep enqueues nothing and advances
  nothing.
- **Full every Nth:** `sync_run_count % 7 == 0` (run 0 full); incremented in-sweep.
- **Cron:** `cron.ParseStandard` → `Next(now)`; an unparseable cron parks the source a
  day out (logged) instead of spinning every tick.
- **Daily GC:** `NOT EXISTS` a `gc_tenant` row for the tenant in the last 24h — no new
  column, idempotent under the lock.

## Tests
- Unit (`internal/worker`, hermetic): `fullSync` fires on run 0/7/14 and not on
  incremental runs; `nextRun` computes hourly/daily fire times and rejects a bad cron.
- e2e (`test/e2e/worker_e2e_test.go`, real Postgres): an armed due cron source is picked
  up by TWO concurrently-running schedulers and yields **exactly one** `sync_source`
  (with mirror row), `next_run_at` advanced into the future, `sync_run_count` = 1, and
  the first run flagged `full` — proving the enqueue, the schedule advance and leader
  election / uniqueness (no duplicate).
- Build/vet green (`-tags e2e`); full unit suite green; drift guard green.
