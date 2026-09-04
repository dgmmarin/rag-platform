-- +goose Up
-- ---------------------------------------------------------------------------
-- Connector state (generic per-source key/value scratch space)
-- ---------------------------------------------------------------------------
-- A connector's durable key/value store for one source (SPEC-04 §1 StateStore,
-- STORY-07.7). The API connector persists its incremental cursor (last max
-- updated_at) and its last-full-sync marker here. It is generic KV, distinct
-- from the crawler's dedicated crawl_pages table. No tenant_id: the database
-- boundary is the tenant boundary (C-1). source_id is an informational copy of
-- a control-plane sources.id (SPEC-03 §2 invariant 4); no cross-database FK.

create table connector_state (
    source_id  uuid not null,
    key        text not null,
    value      text not null,
    updated_at timestamptz not null default now(),
    primary key (source_id, key)
);

-- +goose Down
drop table if exists connector_state;
