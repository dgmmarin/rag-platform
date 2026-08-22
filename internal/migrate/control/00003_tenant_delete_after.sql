-- +goose Up
-- ---------------------------------------------------------------------------
-- Scheduled tenant deletion grace deadline (SPEC-01 §8, FR-TEN-05)
-- ---------------------------------------------------------------------------
-- delete_after records when a scheduled deletion becomes eligible for the
-- irreversible drop: it is set (now + grace, default 7 days) when a tenant moves
-- to 'deleting', cleared on cancellation, and gates the delete_tenant handler so
-- the drop cannot run before the grace period elapses. A partial index lets the
-- (future) scheduler cheaply find tenants past their grace window.

alter table tenants add column if not exists delete_after timestamptz;

create index if not exists tenants_delete_after_idx
    on tenants (delete_after) where status = 'deleting';

-- +goose Down
drop index if exists tenants_delete_after_idx;
alter table tenants drop column if exists delete_after;
