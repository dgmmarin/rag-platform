-- Seed one TENANT database with a synthetic corpus for the retrieval load test
-- (STORY-10.7, NFR-PERF-01). Mirrors the bulk-load path of
-- test/e2e/retrieve_bench_test.go (ADR-0051): real documents + versions + chunks so
-- live_chunks and the HNSW/full-text indexes behave as at production scale.
--
-- Run against EACH of the 4 tenant databases (connect as the tenant role), e.g.:
--   psql "$TENANT_DB_URL" \
--     -v source_id="'11111111-1111-1111-1111-111111111111'" \
--     -v n_docs=100000 -v chunks_per_doc=10 -v dim=1536 \
--     -f test/load/seed-tenant.sql
-- n_docs * chunks_per_doc = total chunks (100000 * 10 = 1,000,000).
-- `dim` MUST equal the tenant's configured embedding dimension (fixed at provisioning).
-- source_id is an informational copy of a control-plane id (C-4) — no FK — so any UUID works.

\set ON_ERROR_STOP on

begin;

insert into documents (id, source_id, external_id, title, uri, status, metadata)
select md5('doc'||d)::uuid, :source_id::uuid, 'ext-'||d, 'Doc '||d,
       'https://load.example.com/'||d, 'active', '{}'::jsonb
from generate_series(0, :n_docs - 1) d;

insert into document_versions (id, document_id, content_hash, content, char_count, parser)
select md5('ver'||d)::uuid, md5('doc'||d)::uuid, sha256(('c'||d)::bytea),
       'doc '||d||' body', 10, 'synthetic'
from generate_series(0, :n_docs - 1) d;

insert into chunks (document_id, version_id, source_id, position, content, token_count, embedding, embedding_model)
select md5('doc'||d)::uuid, md5('ver'||d)::uuid, :source_id::uuid, c,
       'chunk ' || (d*:chunks_per_doc+c) || ' about topic ' || ((d*:chunks_per_doc+c) % 500) ||
       ' error code E' || ((d*:chunks_per_doc+c) % 97) || ' widget maintenance guide',
       20,
       ('[' || array_to_string(array(
           select ((((d*:chunks_per_doc+c)*2654435761 + i*40503) % 100000)::float8 / 100000 - 0.5)
           from generate_series(1, :dim) i), ',') || ']')::vector,
       'load-model'
from generate_series(0, :n_docs - 1) d, generate_series(0, :chunks_per_doc - 1) c;

update documents doc set current_version = dv.id
from document_versions dv
where dv.document_id = doc.id and doc.current_version is null;

commit;

\echo 'seeded' :n_docs 'documents x' :chunks_per_doc 'chunks'
