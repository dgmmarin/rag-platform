-- +goose Up
-- Tenant (data-plane) schema. One database per company, identical schema.
-- Applied per tenant by `ragctl migrate tenants` via goose (SPEC-01 §7); the
-- applied version is tracked in goose_db_version inside the tenant DB and
-- mirrored to control_plane.tenant_databases.schema_version by the runner.
-- No tenant_id column anywhere: the database boundary is the tenant boundary.
--
-- This file is the documented shape of a provisioned tenant database at the
-- default embedding dimension (1536). The authoritative DDL is the goose
-- migration set in internal/migrate/tenant/*.sql, which uses a dimension
-- placeholder the runner substitutes per tenant; a drift test proves the two
-- never disagree (TestTenantSchemaMatchesMigrations).
--
-- Required extensions (vector, pgcrypto, pg_trgm) are installed by the privileged
-- provisioning connection when the database is created (SPEC-01 §6), NOT here:
-- tenant migrations run as the least-privilege per-tenant role (SPEC-09), which
-- cannot CREATE EXTENSION. This migration assumes they already exist.

-- ---------------------------------------------------------------------------
-- Documents
-- ---------------------------------------------------------------------------
-- A document is the unit of content from a source: a file, a web page, an API
-- record. Identity is (source_id, external_id). Content lives in versions so a
-- sync can build the new version fully before atomically pointing at it.

create type document_status as enum ('active', 'deleted');

create table documents (
    id              uuid primary key default gen_random_uuid(),
    source_id       uuid not null,                      -- control-plane sources.id (no FK across DBs)
    external_id     text not null,                      -- url, file path, API record id
    title           text,
    uri             text,                               -- canonical link shown in citations
    mime_type       text,
    status          document_status not null default 'active',
    current_version uuid,                               -- -> document_versions.id
    metadata        jsonb not null default '{}',        -- source-specific fields, tags
    first_seen_at   timestamptz not null default now(),
    last_seen_at    timestamptz not null default now(), -- touched on every sync that sees it
    deleted_at      timestamptz,
    unique (source_id, external_id)
);
create index on documents (source_id, status);
create index on documents (last_seen_at);
create index on documents using gin (metadata jsonb_path_ops);

create table document_versions (
    id              uuid primary key default gen_random_uuid(),
    document_id     uuid not null references documents(id) on delete cascade,
    content_hash    bytea not null,                     -- sha256 of normalised text
    content         text not null,                      -- normalised markdown/text
    raw_ref         text,                               -- object-storage key of original bytes, if kept
    raw_json        jsonb,                              -- original API payload, if any
    char_count      int not null,
    parser          text,                               -- which parser/version produced content
    created_at      timestamptz not null default now(),
    unique (document_id, content_hash)
);
create index on document_versions (document_id, created_at desc);

alter table documents
    add constraint documents_current_version_fk
    foreign key (current_version) references document_versions(id) deferrable initially deferred;

-- ---------------------------------------------------------------------------
-- Chunks
-- ---------------------------------------------------------------------------
-- Embedding dimension is fixed per tenant database at provisioning time from
-- tenants.settings.embedding_dim. Changing models => reindex (new table swap).

create table chunks (
    id              uuid primary key default gen_random_uuid(),
    document_id     uuid not null references documents(id) on delete cascade,
    version_id      uuid not null references document_versions(id) on delete cascade,
    source_id       uuid not null,                      -- denormalised for filtering
    position        int  not null,                      -- order within document
    heading_path    text[] not null default '{}',       -- ["Install", "Linux"]
    content         text not null,
    token_count     int  not null,
    embedding       vector(EMBEDDING_DIM),              -- dim replaced at provisioning
    embedding_model text not null,
    tsv             tsvector generated always as (to_tsvector('simple', coalesce(content, ''))) stored,
    metadata        jsonb not null default '{}',
    created_at      timestamptz not null default now(),
    unique (version_id, position)
);
create index on chunks (document_id);
create index on chunks (source_id);
create index chunks_tsv_idx on chunks using gin (tsv);
-- HNSW is the default; switch to ivfflat for very large tables if build time matters.
create index chunks_embedding_idx on chunks using hnsw (embedding vector_cosine_ops)
    with (m = 16, ef_construction = 64);

-- View used by retrieval: only chunks of the current version of active documents.
create view live_chunks as
select c.*
from chunks c
join documents d on d.id = c.document_id
where d.status = 'active' and d.current_version = c.version_id;

-- ---------------------------------------------------------------------------
-- Crawl state (web_crawl / sitemap connectors)
-- ---------------------------------------------------------------------------

create table crawl_pages (
    source_id       uuid not null,
    url             text not null,
    normalized_url  text not null,
    depth           int  not null default 0,
    last_fetched_at timestamptz,
    last_status     int,                                -- http status
    etag            text,
    last_modified   text,
    content_hash    bytea,
    error           text,
    primary key (source_id, normalized_url)
);
create index on crawl_pages (source_id, last_fetched_at);

-- ---------------------------------------------------------------------------
-- Optional structured product data (FR-ING-12)
-- ---------------------------------------------------------------------------

create table products (
    id              uuid primary key default gen_random_uuid(),
    document_id     uuid references documents(id) on delete set null,
    source_id       uuid not null,
    external_id     text not null,
    name            text not null,
    sku             text,
    category        text,
    price_amount    numeric(14,2),
    price_currency  char(3),
    attributes      jsonb not null default '{}',
    updated_at      timestamptz not null default now(),
    unique (source_id, external_id)
);
create index on products using gin (name gin_trgm_ops);
create index on products (sku);

-- ---------------------------------------------------------------------------
-- Query log and feedback
-- ---------------------------------------------------------------------------

create table query_log (
    id              uuid primary key default gen_random_uuid(),
    request_id      text,
    api_key_id      uuid,                               -- control-plane id, informational
    user_id         uuid,
    question        text not null,
    filters         jsonb not null default '{}',
    retrieved       jsonb not null default '[]',        -- [{chunk_id, score, rank}]
    answer          text,
    citations       jsonb not null default '[]',
    embedding_model text,
    llm_model       text,
    retrieval_ms    int,
    generation_ms   int,
    in_tokens       int,
    out_tokens      int,
    grounded        boolean,                            -- did we answer or decline
    created_at      timestamptz not null default now()
);
create index on query_log (created_at desc);

create table query_feedback (
    query_id        uuid primary key references query_log(id) on delete cascade,
    rating          smallint not null check (rating in (-1, 1)),
    comment         text,
    user_id         uuid,
    created_at      timestamptz not null default now()
);

-- ---------------------------------------------------------------------------
-- Evaluation harness (FR-ADM-04)
-- ---------------------------------------------------------------------------

create table eval_cases (
    id              uuid primary key default gen_random_uuid(),
    question        text not null,
    expected_answer text,
    expected_doc_ids uuid[] not null default '{}',      -- documents that should be retrieved
    tags            text[] not null default '{}',
    created_at      timestamptz not null default now()
);

create table eval_runs (
    id              uuid primary key default gen_random_uuid(),
    config          jsonb not null,                     -- models, top_k, reranker...
    started_at      timestamptz not null default now(),
    finished_at     timestamptz,
    summary         jsonb                               -- recall@k, grounded %, mean latency
);

create table eval_results (
    run_id          uuid not null references eval_runs(id) on delete cascade,
    case_id         uuid not null references eval_cases(id) on delete cascade,
    retrieved_doc_ids uuid[] not null default '{}',
    recall_hit      boolean,
    answer          text,
    judged_correct  boolean,
    latency_ms      int,
    primary key (run_id, case_id)
);

-- +goose Down
drop table if exists eval_results;
drop table if exists eval_runs;
drop table if exists eval_cases;
drop table if exists query_feedback;
drop table if exists query_log;
drop table if exists products;
drop view if exists live_chunks;
drop table if exists chunks;
drop table if exists crawl_pages;
alter table if exists documents drop constraint if exists documents_current_version_fk;
drop table if exists document_versions;
drop table if exists documents;
drop type if exists document_status;
