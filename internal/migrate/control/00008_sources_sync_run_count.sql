-- +goose Up
-- ---------------------------------------------------------------------------
-- Scheduler run counter for cron sources (STORY-09.3, FR-SRC-11, SPEC-08 §2)
-- ---------------------------------------------------------------------------
-- The leader-elected scheduler enqueues an incremental sync_source each time a
-- source's cron is due and a FULL sync every Nth run (SPEC-08 §2). sync_run_count is
-- that per-source counter: the scheduler reads it to decide full vs incremental and
-- increments it in the same transaction as the enqueue and the next_run_at update, so
-- the decision is deterministic and survives restarts. Starts at 0, so the first
-- scheduled run is a full sync.
alter table sources add column sync_run_count integer not null default 0;

-- +goose Down
alter table sources drop column if exists sync_run_count;
