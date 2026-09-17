# ADR-0076: VectorChord (vchordrq) for the tenant vector index, replacing pgvector HNSW

**Status:** Accepted · **Date:** 2026-09-17 · **Requirements:** FR-RET-01/06, NFR-PERF · **Decisions:** ADR-0004 (pgvector), SPEC-03 §2, SPEC-07 §2

## Context
The tenant `chunks.embedding` column used a pgvector **HNSW** index (`vector_cosine_ops`). pgvector's
HNSW (and ivfflat) index is **capped at 2000 dimensions**, and `halfvec` HNSW at 4000. That blocks
strong high-dimensional embedding models — e.g. `qwen3-embedding-8b` outputs **4096** dims, which
cannot be indexed at all under HNSW (provisioning fails with `column cannot have more than 2000
dimensions for hnsw index`). The `vector` column type itself stores up to 16000 dims; only the index
is the limit.

## Decision
Adopt **VectorChord** (extension `vchord`) and index `chunks.embedding` with its **`vchordrq`**
index (RaBitQ quantization + IVF) instead of HNSW.

- **No dimension cap** on the index — 4096-dim (and larger) vectors are indexable. Verified live:
  a tenant provisioned at `vector(4096)` builds a `vchordrq` index successfully.
- **Retrieval SQL is unchanged.** `vchordrq` uses the same `vector` type and the same cosine `<=>`
  operator, so `internal/retrieve` (`hybridSQL`) needs no change. Query recall is tuned per session
  with `SET vchordrq.probes`.
- **Stays in Postgres.** Keeps the per-tenant DB isolation (ADR-0003) and the one-query hybrid
  (vector + `tsvector` BM25) — no separate vector service to run or sync. Considered but rejected for
  now: a dedicated vector DB (Qdrant/Milvus) — warranted only at far larger scale, at the cost of
  another system and losing free hybrid + tenant isolation. Also rejected: MRL-truncating models to
  ≤2000 to fit HNSW — loses embedding quality for no reason once vchordrq is available.

## Consequences
- **Image:** Postgres image is now `tensorchord/vchord-postgres:pg16-v1.1.1` (bundles pgvector +
  vchord) with `shared_preload_libraries=vchord` (`docker-compose.yml`). The seed and the provisioner
  create the `vchord` extension per DB (`deploy/seed/01-control-plane.sh`, `internal/provision/sql.go`).
- **Schema:** `internal/migrate/tenant/00001_initial_schema.sql` builds `chunks_embedding_idx` with
  `vchordrq (embedding vector_cosine_ops)`, `lists = [1]` (a single flat list — safe to build on the
  empty table a tenant is provisioned with, and fast at these corpus sizes). A reindex can raise
  `lists` for a large tenant.
- **Migration:** applied by wiping existing tenant data and re-provisioning fresh (operator choice —
  "delete existing data, no remigration"), not by a data-preserving migration. The control-plane DB
  (users, registry) is preserved across the image swap (same PG16 data volume).
- **Recall tuning:** `lists=[1]` is brute-force RaBitQ (exact-ish, good recall, fine to tens of
  thousands of chunks); larger tenants should reindex with more lists and set `vchordrq.probes`.

## Probes (required)
`vchordrq` **errors on every query** when `vchordrq.probes` is unset (`need N probes, but 0
provided`). Provisioning sets a per-database default — `ALTER DATABASE <db> SET vchordrq.probes = '10'`
(`internal/provision` `databaseSettingsSQL`) — so every session has it with no per-query `SET` and the
retrieval SQL stays unchanged. It applies at connection time, so an already-running serve must
reconnect (restart) to pick it up on a tenant altered after the fact. `10` is ample for the
`lists=[1]` index and scales with a reindex.

## Follow-up
- The reindex job (ISSUE-0013) should rebuild `vchordrq` with a size-appropriate `lists` after bulk
  ingest, and probes can be raised per tenant (`ALTER DATABASE … SET vchordrq.probes`).
- Optionally set `vchordrq.probes` in the retrieval session too (belt-and-suspenders) so a tenant DB
  missing the default cannot 500 every query.
