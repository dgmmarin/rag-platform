# ADR-0004: pgvector in the tenant database for vector storage

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** FR-ING-06, FR-RET-01, NFR-PERF-01, NFR-PORT-01

## Context
Embeddings need a store supporting approximate nearest-neighbour search, metadata filtering and per-tenant isolation. Full-text search is also required for hybrid retrieval.

## Options
1. Dedicated vector database (Qdrant, Weaviate, Pinecone) with one collection per tenant.
2. pgvector inside each tenant's PostgreSQL database.
3. Both: Postgres as source of truth, vector DB as index.

## Decision
Option 2 for v1. Chunks, embeddings (`vector` column with HNSW index) and `tsvector` live in the same tenant database. Hybrid search is a single SQL query merged by reciprocal rank fusion.

## Consequences
- One system to provision, back up, migrate and delete per tenant; isolation story is unchanged.
- Transactional consistency between documents, chunks and embeddings.
- HNSW in pgvector is adequate to low millions of chunks per tenant (NFR-SCAL-02 via dedicated hardware); beyond that, ADR to be revisited with option 3.
- Embedding dimension is fixed per tenant database at provisioning; model change requires reindex (SPEC-05 describes the shadow-table swap).
