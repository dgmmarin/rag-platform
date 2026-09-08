# ADR-0061: Scheduler for cron sources and daily GC — a leader-elected advisory-lock sweep enqueuing sync_source and gc_tenant transactionally

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-SRC-11, SPEC-08 §2 · **Decisions:** ADR-0005, ADR-0060

## Context
SPEC-08 §2: a leader-elected loop runs every ~30s, selects active sources whose
`next_run_at` is due, enqueues a `sync_source` (incremental, full every Nth run),
recomputes `next_run_at` from `schedule_cron`, and enqueues `gc_tenant` daily per
active tenant — with no duplicate enqueues when several worker replicas run the loop.
The queue, the mirror and the enqueue pattern already exist (ADR-0005, ADR-0060);
this story adds the producer that fires them on a schedule.

## Options / decisions
- **Leader election via a Postgres transaction advisory lock, per sweep.** Each tick
  opens a transaction and calls `pg_try_advisory_xact_lock(key)`; a replica that does
  not win the lock returns immediately, so exactly one replica sweeps at a time. The
  lock is transaction-scoped, so it releases on commit/rollback and cannot leak across
  a crash. This is simpler than a held session lock (which pgxpool's connection reuse
  makes awkward) and needs no leases table. River's per-source/per-tenant uniqueness is
  a second backstop against duplicates.
- **Everything in one transaction.** The lock, the `sync_source`/`gc_tenant` enqueues
  (River `InsertTx` + the jobs mirror row, the ADR-0060 pattern via a shared
  `enqueueMirrored`), and the `next_run_at` / `sync_run_count` updates all commit
  together. A crash mid-sweep rolls back the whole thing — no job is enqueued without
  its schedule advancing, and none is enqueued twice.
- **`sync_run_count` column for "full every Nth run".** Migration 00008 adds a
  per-source counter; the scheduler reads it (full when `count % 7 == 0`, so the first
  scheduled run is full) and increments it in the sweep transaction. Deriving fullness
  from the jobs history would be a scan per source per tick; a counter is O(1) and
  survives restarts.
- **Cron parsing with `robfig/cron/v3`.** Already in the module graph (River's periodic
  jobs depend on it), promoted to a direct dependency — no new external dependency
  (`cron.ParseStandard` on the 5-field `schedule_cron`, then `Next(now)`). An
  unparseable cron parks the source a day out (logged) rather than spinning the sweep on
  it every tick.
- **Daily GC gated by the jobs history, not a new column.** `gc_tenant` is enqueued for
  an active tenant only when no `gc_tenant` row exists for it within the last 24h — a
  `NOT EXISTS` against the mirror table, so no `tenants.last_gc_at` column is needed and
  the cadence is naturally idempotent under the leader lock.
- **The scheduler runs inside `ragctl work`.** It is launched as a goroutine from
  `runWorker` on the worker's own River client, and stops on the shutdown context before
  the pool closes. River's static `PeriodicJobs` were not used: the per-source cron set
  is dynamic (sources come and go at runtime), which a DB-driven sweep handles and a
  static list does not.

## Consequences
- Cron sources sync on their schedule and every active tenant is garbage-collected
  daily, with several worker replicas safe to run — one sweeps, the rest no-op.
- The schedule state (`next_run_at`, `last_run_at`, `sync_run_count`) is control-plane
  data advanced atomically with the enqueue, so the admin view and the queue never
  disagree about when a source last ran.
- A bad `schedule_cron` degrades to a parked source (a day out) an admin can fix, never
  a hot loop.
- The migration is additive (a defaulted column); the drift guard moves in step and
  River's own migrations stay separate (ADR-0059).
- One shared `enqueueMirrored` now expresses the ADR-0060 transactional-enqueue pattern
  for the scheduler; the HTTP producers still carry their own copies (documents/sources)
  — a future tidy could converge them, out of scope here.
