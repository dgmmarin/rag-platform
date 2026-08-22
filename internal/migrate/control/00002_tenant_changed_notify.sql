-- +goose Up
-- ---------------------------------------------------------------------------
-- tenant_changed LISTEN/NOTIFY (SPEC-01 §3)
-- ---------------------------------------------------------------------------
-- The resolver LISTENs on `tenant_changed`; this trigger publishes the tenant id
-- on every registry change so a suspension or connection move invalidates the
-- resolver's cache within ~1s instead of on the 30s TTL.

-- +goose StatementBegin
create or replace function notify_tenant_changed() returns trigger language plpgsql as $$
begin
    perform pg_notify('tenant_changed', coalesce(new.id, old.id)::text);
    return null;
end $$;
-- +goose StatementEnd

create trigger tenants_notify_changed
    after insert or update or delete on tenants
    for each row execute function notify_tenant_changed();

-- +goose Down
drop trigger if exists tenants_notify_changed on tenants;
drop function if exists notify_tenant_changed();
