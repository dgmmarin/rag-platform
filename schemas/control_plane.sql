-- Control-plane schema (shared database).
-- Holds tenant registry, users, source definitions, job tracking, and usage.
-- Never holds tenant content. All tenant content lives in the per-tenant databases.

create extension if not exists pgcrypto;  -- gen_random_uuid()

-- ---------------------------------------------------------------------------
-- Tenants
-- ---------------------------------------------------------------------------

create type tenant_status as enum ('provisioning', 'active', 'suspended', 'deleting', 'deleted');

create table tenants (
    id              uuid primary key default gen_random_uuid(),
    slug            text not null unique,               -- url-safe, immutable (e.g. "acme")
    name            text not null,
    status          tenant_status not null default 'provisioning',
    region          text not null default 'eu-central', -- data residency hint
    settings        jsonb not null default '{}',        -- embedding model, chunk size, etc.
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    deleted_at      timestamptz
);

-- Where each tenant's database lives. Separate table so credentials can be
-- locked down with stricter grants than the rest of the control plane.
create table tenant_databases (
    tenant_id       uuid primary key references tenants(id) on delete cascade,
    host            text not null,
    port            int  not null default 5432,
    database_name   text not null,
    username        text not null,
    password_enc    bytea not null,                     -- encrypted at app level (KMS / envelope key)
    ssl_mode        text not null default 'require',
    schema_version  int  not null default 0,            -- last applied tenant migration
    max_conns       int  not null default 5,            -- per-tenant pool cap
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    unique (host, port, database_name)
);

-- ---------------------------------------------------------------------------
-- Users and membership
-- ---------------------------------------------------------------------------

create table users (
    id              uuid primary key default gen_random_uuid(),
    email           citext not null unique,
    display_name    text,
    external_id     text,                               -- IdP subject if using SSO
    is_platform_admin boolean not null default false,
    created_at      timestamptz not null default now(),
    last_login_at   timestamptz
);

create type tenant_role as enum ('owner', 'admin', 'editor', 'viewer');

create table tenant_members (
    tenant_id       uuid not null references tenants(id) on delete cascade,
    user_id         uuid not null references users(id) on delete cascade,
    role            tenant_role not null default 'viewer',
    created_at      timestamptz not null default now(),
    primary key (tenant_id, user_id)
);

-- Machine credentials for programmatic access (scoped to one tenant).
create table api_keys (
    id              uuid primary key default gen_random_uuid(),
    tenant_id       uuid not null references tenants(id) on delete cascade,
    name            text not null,
    key_hash        bytea not null unique,              -- sha256 of the secret; never store plaintext
    key_prefix      text not null,                      -- first 8 chars, for display/lookup
    scopes          text[] not null default '{query}',  -- query, ingest, admin
    created_by      uuid references users(id),
    created_at      timestamptz not null default now(),
    expires_at      timestamptz,
    revoked_at      timestamptz,
    last_used_at    timestamptz
);
create index on api_keys (tenant_id);

-- ---------------------------------------------------------------------------
-- Sources (connector instances)
-- ---------------------------------------------------------------------------

create type source_kind as enum ('upload', 'web_crawl', 'api', 's3', 'sitemap');
create type source_status as enum ('active', 'paused', 'error', 'deleting');

create table sources (
    id              uuid primary key default gen_random_uuid(),
    tenant_id       uuid not null references tenants(id) on delete cascade,
    kind            source_kind not null,
    name            text not null,
    status          source_status not null default 'active',
    config          jsonb not null default '{}',        -- kind-specific: start urls, allowlist, endpoint, mapping
    credentials_enc bytea,                              -- kind-specific secrets, encrypted
    schedule_cron   text,                               -- null = manual only
    next_run_at     timestamptz,
    last_run_at     timestamptz,
    last_success_at timestamptz,
    last_error      text,
    created_by      uuid references users(id),
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    unique (tenant_id, name)
);
create index on sources (tenant_id, status);
create index on sources (next_run_at) where status = 'active' and schedule_cron is not null;

-- ---------------------------------------------------------------------------
-- Jobs (sync, reindex, provision, delete)
-- ---------------------------------------------------------------------------
-- If you adopt River or another Postgres-backed queue, it brings its own tables;
-- this table is then the *history* view you show in the admin UI, not the queue.

create type job_kind as enum ('sync_source', 'reindex_tenant', 'provision_tenant', 'delete_tenant', 'delete_source');
create type job_status as enum ('queued', 'running', 'succeeded', 'failed', 'cancelled');

create table jobs (
    id              uuid primary key default gen_random_uuid(),
    tenant_id       uuid not null references tenants(id) on delete cascade,
    source_id       uuid references sources(id) on delete set null,
    kind            job_kind not null,
    status          job_status not null default 'queued',
    attempt         int not null default 0,
    max_attempts    int not null default 3,
    payload         jsonb not null default '{}',
    stats           jsonb not null default '{}',        -- docs_seen, docs_changed, chunks_written, bytes...
    error           text,
    queued_at       timestamptz not null default now(),
    started_at      timestamptz,
    finished_at     timestamptz,
    worker_id       text
);
create index on jobs (tenant_id, queued_at desc);
create index on jobs (status) where status in ('queued', 'running');
-- Prevent two concurrent syncs of the same source.
create unique index jobs_one_active_sync_per_source
    on jobs (source_id) where kind = 'sync_source' and status in ('queued', 'running');

-- ---------------------------------------------------------------------------
-- Usage and audit
-- ---------------------------------------------------------------------------

-- Lightweight per-tenant counters, aggregated by day. Good enough for billing
-- and dashboards; push raw events to a metrics system if you need more.
create table usage_daily (
    tenant_id       uuid not null references tenants(id) on delete cascade,
    day             date not null,
    queries         bigint not null default 0,
    docs_ingested   bigint not null default 0,
    chunks_embedded bigint not null default 0,
    embed_tokens    bigint not null default 0,
    llm_in_tokens   bigint not null default 0,
    llm_out_tokens  bigint not null default 0,
    primary key (tenant_id, day)
);

create table audit_log (
    id              bigserial primary key,
    tenant_id       uuid references tenants(id) on delete set null,
    actor_user_id   uuid references users(id) on delete set null,
    actor_api_key   uuid references api_keys(id) on delete set null,
    action          text not null,                      -- tenant.create, source.update, key.revoke...
    target_type     text,
    target_id       uuid,
    details         jsonb not null default '{}',
    created_at      timestamptz not null default now()
);
create index on audit_log (tenant_id, created_at desc);

-- ---------------------------------------------------------------------------
-- updated_at trigger
-- ---------------------------------------------------------------------------

create or replace function set_updated_at() returns trigger language plpgsql as $$
begin new.updated_at = now(); return new; end $$;

create trigger tenants_updated_at          before update on tenants          for each row execute function set_updated_at();
create trigger tenant_databases_updated_at before update on tenant_databases for each row execute function set_updated_at();
create trigger sources_updated_at          before update on sources          for each row execute function set_updated_at();
