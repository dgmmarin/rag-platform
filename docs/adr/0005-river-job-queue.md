# ADR-0005: Postgres-backed job queue (River)

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** FR-ING-08, FR-SRC-11, NFR-REL-02

## Context
Syncs, reindexes, provisioning and deletion are long-running asynchronous jobs needing retries, scheduling, uniqueness (one sync per source at a time) and visibility.

## Options
1. Redis-backed queue (asynq).
2. Postgres-backed queue in the control plane (River).
3. Workflow engine (Temporal).

## Decision
Option 2. River runs on the control-plane database. Job arguments carry `tenant_id`; workers open a `tenant.DB` per job. The `jobs` table in the control plane mirrors River state for the admin UI and long-term history. Periodic jobs (cron schedules) are registered from `sources.schedule_cron` by a scheduler loop.

## Consequences
- No extra infrastructure; transactional enqueue with the row that created it.
- Uniqueness and retries with backoff are built in.
- Throughput ceiling is that of Postgres — fine for job counts in the thousands per hour.
- If sync workflows grow multi-step with human approval or very long durations, revisit Temporal.
