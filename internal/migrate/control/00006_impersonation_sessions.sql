-- +goose Up
-- ---------------------------------------------------------------------------
-- Platform admin impersonation grants (STORY-03.8, FR-ACC-07, SPEC-02 §4/§6)
-- ---------------------------------------------------------------------------
-- A platform admin may act as a tenant user for support (FR-ACC-07). Each grant
-- records BOTH the real admin actor (admin_user_id) and the impersonated
-- principal (tenant_id + impersonated_user_id) so every action taken under it
-- stays attributable to the admin — identity is never silently swapped. The grant
-- is time-bounded (expires_at) and revocable (ended_at is stamped on End). This is
-- control-plane-only (C-3): tenant_id/impersonated_user_id are informational copies
-- of control-plane ids and no tenant data is touched. Start/End each write an
-- admin.impersonate(.end) audit event (details.impersonation=true).
create table impersonation_sessions (
    id                   uuid primary key default gen_random_uuid(),
    admin_user_id        uuid not null references users(id) on delete cascade, -- the real platform admin
    tenant_id            uuid not null references tenants(id) on delete cascade, -- impersonated tenant
    impersonated_user_id uuid not null references users(id) on delete cascade, -- impersonated user
    created_at           timestamptz not null default now(),
    expires_at           timestamptz not null,               -- time bound; an expired grant is inactive
    ended_at             timestamptz                          -- set on End; an ended grant is inactive
);
create index on impersonation_sessions (admin_user_id);
create index on impersonation_sessions (tenant_id);
create index on impersonation_sessions (expires_at) where ended_at is null;

-- +goose Down
drop table if exists impersonation_sessions;
