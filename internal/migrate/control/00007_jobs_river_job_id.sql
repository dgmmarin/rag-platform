-- +goose Up
-- ---------------------------------------------------------------------------
-- Link a jobs mirror row to its River queue job (STORY-09.2, FR-ADM-02, SPEC-08 §3)
-- ---------------------------------------------------------------------------
-- ADR-0005 makes River authoritative and the control-plane `jobs` table its
-- mirror. river_job_id is the id of the River queue job a producer enqueued in the
-- SAME transaction as this row (ADR-0060); the worker's mirror middleware locates
-- the row to transition (queued->running->succeeded/failed/cancelled) by it.
-- Nullable: a row for a kind not yet enqueued through River — and any pre-migration
-- row — simply carries no link and is left untouched by the middleware. The partial
-- unique index keeps one River job mapped to at most one mirror row.
alter table jobs add column river_job_id bigint;
create unique index jobs_river_job_id_key on jobs (river_job_id) where river_job_id is not null;

-- +goose Down
drop index if exists jobs_river_job_id_key;
alter table jobs drop column if exists river_job_id;
