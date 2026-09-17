# ADR-0077: Chunk-level drift detection — reuse embeddings for unchanged chunks

**Status:** Accepted · **Date:** 2026-09-17 · **Requirements:** FR-SRC-01, NFR-COST · **Decisions:** SPEC-05 §1/§5, ADR-0008 (single-transaction commit), ADR-0003 (tenant isolation) · **Relates:** ADR-0076 (VectorChord)

## Context
The sink already skips work for an **unchanged document**: it hashes the normalised content and, when
the hash matches the current version, only touches `last_seen_at` — no chunk, no embed (SPEC-05 §1
"hash short-circuit"). Incremental crawls also skip unchanged pages at the HTTP layer with conditional
GET (ETag / Last-Modified → 304).

But when a document **changes at all**, the sink re-chunks it and **re-embeds every chunk**. A
one-line edit in a large page pays to re-embed the whole page, even though most chunks are
byte-identical to what was already embedded. Embedding is the dominant ingest cost, so this is
wasteful — most of all on large corpora with frequent small edits.

## Decision
On a changed document, embed only the chunks whose content is genuinely new. Reuse the existing
embedding vector for any chunk whose embed-text is **byte-identical** to one already embedded under the
**same embedding model**, anywhere in the tenant.

- **Content-address each chunk.** `chunks.content_hash bytea not null` holds `sha256(embed-text)` —
  the exact text sent to the embedder, so the hash and the stored vector always correspond. This is
  the chunk's own hash; it is distinct from `document_versions.content_hash` (the whole-document hash
  the document-level short-circuit uses).
- **A small reuse service — `internal/ingest/embedcache`.** It owns exactly the lookup rule and
  nothing else: `Lookup(ctx, db, model, hashes) → map[hex(hash)]vector`, one query
  `select distinct on (content_hash) … where content_hash = any($1) and embedding_model = $2`. The
  index `chunks (content_hash, embedding_model)` serves it. The rule is testable without the sink.
- **The sink embeds only the misses.** It hashes each chunk, looks the hashes up, calls the embedder
  once on just the absent texts (skipping the call entirely when every chunk is a hit), and
  reassembles vectors in chunk order. The commit is unchanged: `Store.Put` inserts the new version +
  chunks (now each carrying `content_hash`) and flips `current_version` in ONE transaction (ADR-0008).
- **Tenant-wide, exact reuse.** A hash matches across different documents, so shared boilerplate is
  deduplicated too. Reuse stays inside the tenant database (ADR-0003) — no cross-tenant sharing.

### Approach A over a refcounted embedding store
Approach A reuses vectors **from the existing `chunks` rows** — one column and one index on the table
the vectors already live in. The rejected alternative was a separate, refcounted embedding store
(vectors held once, chunks pointing at them). That store buys physical de-duplication of the stored
bytes, but at the cost of a second table, reference counting, and a garbage-collection path — none of
which the goal (skip re-embedding) needs. Approach A skips the embed call with no new storage
lifecycle. Exact (SHA-256) match only; near-duplicate / semantic reuse is a non-goal.

## Consequences
- **Schema:** `internal/migrate/tenant/00001_initial_schema.sql` adds `chunks.content_hash` and the
  `(content_hash, embedding_model)` index. It rides the wipe-and-re-provision already chosen for
  ADR-0076 — no data migration. `schemas/tenant.sql` (the documented mirror) is synced to match, so
  `TestTenantSchemaMatchesMigrations` stays green.
- **Model change is a miss, correctly.** The lookup keys on `embedding_model`, so a hash that exists
  only under a different model re-embeds under the new one. A reused vector already matches the
  tenant's provisioned dimension (same model), so no dimension mismatch is possible.
- **Source status is ignored on read.** The vector for a given (content, model) is identical
  regardless of the owning document's state, so a soft-deleted doc's chunk is a valid reuse source
  until GC removes it.
- **Concurrency is bounded and self-correcting.** Two documents ingesting the same brand-new chunk in
  parallel both embed it (neither is committed, so neither sees the other) — rare, and the next run
  reuses.
- **Failure semantics unchanged.** The circuit breaker, per-document failure recording, and rollback
  (SPEC-05 §5) wrap only the now-smaller embed call and the commit.

## Observability
- `sink.Stats` reports `ChunksEmbedded` and `ChunksReused` (JSON `chunks_embedded` / `chunks_reused`),
  surfaced in `jobs.stats` and the worker `job finished` log.
- Metric `embed_chunks_reused_total{tenant,provider}` sits beside `ingest_chunks_total`, so the reuse
  hit-rate is visible.

## Follow-up
- A boundary-stable chunker would raise the hit-rate: today a mid-document insertion that re-flows
  chunk boundaries re-embeds the reflowed chunks (inherent to content-hash reuse, accepted).
- The reuse hit-rate metric can gate a future decision on whether semantic (near-duplicate) reuse is
  worth its complexity.
