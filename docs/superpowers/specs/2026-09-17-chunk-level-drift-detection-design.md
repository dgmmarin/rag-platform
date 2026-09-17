# Chunk-level drift detection — reuse embeddings for unchanged chunks

**Date:** 2026-09-17 · **Status:** Implemented (ADR-0077) · **Traces:** SPEC-05 §1/§5, FR-SRC-01, ADR-0008 · **Relates:** ADR-0076 (VectorChord)

## Problem

The platform already skips work for an **unchanged document** on re-ingest: the sink hashes the
normalised content and, when the hash matches the current version, only touches `last_seen_at` — no
chunk, no embed (`internal/ingest/sink/sink.go`, "SPEC-05 §1 hash short-circuit"; reported as
`DocsUnchanged`). Incremental crawls also skip unchanged pages at the HTTP layer via conditional GET
(ETag/Last-Modified → 304).

But when a document **changes at all**, the sink re-chunks it and **re-embeds every chunk**
(`Embedder.Embed(embedTexts(chunks))`). A one-line edit in a large page pays to re-embed the whole
page, even though most chunks are byte-identical to what was already embedded. Embedding is the
dominant ingest cost, so this is wasteful — especially on large corpora with frequent small edits.

## Goal

On a changed document, embed only the chunks whose content is genuinely new. Reuse the existing
embedding vector for any chunk whose embed-text is **byte-identical** to one already embedded under
the **same embedding model**, anywhere in the tenant (so identical boilerplate/shared sections across
different pages are also deduplicated). Behaviour, correctness, and the single-transaction commit are
otherwise unchanged.

### Non-goals
- Near-duplicate / semantic matching. Reuse is exact (SHA-256) only.
- A separate physical embedding store or cross-tenant sharing (tenant isolation, ADR-0003).
- Changing the chunker or the document-level short-circuit (both stay as the fast paths).
- Reducing embedding for a document whose chunk **boundaries shift** — only byte-identical chunks
  reuse; a mid-document insertion that re-flows boundaries will re-embed the reflowed chunks. This is
  inherent to content-hash reuse and accepted.

## Design

### The service — `internal/ingest/embedcache`

A small package whose one job is the reuse rule. It owns exactly the lookup SQL and nothing else, so
the rule can be understood and tested without the sink.

```go
package embedcache

// Cache returns, for chunk-content hashes, the embedding an existing chunk already
// holds for (hash, model) — so the caller can skip re-embedding those chunks.
type Cache interface {
    // Lookup maps each hex(content_hash) that already has an embedding for `model`
    // to that vector. Hashes with no existing embedding are absent from the map.
    Lookup(ctx context.Context, db *tenant.DB, model string, hashes [][]byte) (map[string][]float32, error)
}
```

Production implementation runs one query:

```sql
select distinct on (content_hash) content_hash, embedding
from chunks
where content_hash = any($1) and embedding_model = $2;
```

`distinct on (content_hash)` returns one vector per hash. The result is keyed by `hex(content_hash)`
for the caller. A nil/empty `hashes` returns an empty map without querying.

### Schema (tenant DB)

Two additions to `chunks` in `internal/migrate/tenant/00001_initial_schema.sql` (edited in place — the
operator is wiping and re-provisioning, so no new migration; consistent with the ADR-0076 approach):

- `content_hash bytea not null` — `sha256(embed-text)` for the chunk (the exact text sent to the
  embedder, so the hash and the vector always correspond).
- `create index on chunks (content_hash, embedding_model)` — the reuse lookup.

`content_hash` is the chunk's own text hash (per chunk); it is distinct from `document_versions.content_hash`
(the whole-document hash used by the document-level short-circuit).

### Data flow — sink changed-document path

Only the changed-document branch (`sink.Put` steps 4–6) changes; steps 1–3 (parse, normalise,
document hash short-circuit) are untouched.

1. Chunk the document (unchanged): `chunks := chunk.Document(norm, cfg.Chunk)`.
2. For each chunk compute `content_hash = sha256(embedText(chunk))`.
3. `hits := cache.Lookup(db, model, hashes)` — vectors already known for these hashes.
4. **Embed only the misses:** collect the chunks absent from `hits`, call `Embedder.Embed` once on
   just those texts. If every chunk is a hit, skip the embed call entirely.
5. Reassemble `vectors []` in chunk order: a hit's vector from `hits`, a miss's vector from the embed
   result (by position). Assert the final count equals `len(chunks)`.
6. Commit (unchanged): `Store.Put` inserts the new version + chunks (now each carrying `content_hash`)
   and flips `current_version` in one transaction (ADR-0008). `putInput` gains the per-chunk hashes.

Circuit-breaker, per-document failure recording, and rollback semantics (SPEC-05 §5) are unchanged —
they wrap only the (now smaller) embed call and the commit.

### Observability

- `sink.Stats` gains `ChunksEmbedded` and `ChunksReused` (JSON `chunks_embedded` / `chunks_reused`),
  surfaced in `jobs.stats` and the worker `job finished` log.
- Metric `embed_chunks_reused_total` (labels: tenant, provider) alongside the existing
  `ingest_chunks_total`, so the reuse hit-rate is visible.

### Edge cases

- **Model change:** the lookup keys on `embedding_model`, so a hash that only exists under a different
  model is a miss → re-embedded under the new model. Correct.
- **Source status:** reuse from **any** chunk row with the hash (no document-status filter) — the
  vector for a given (content, model) is identical regardless of the owning document's state, so a
  soft-deleted doc's chunk is a valid source until GC removes it.
- **Concurrency:** two documents ingesting the same brand-new chunk in parallel both embed it (neither
  is committed yet, so neither sees the other) — rare, bounded, and self-correcting on the next run.
- **Dimension:** a reused vector already matches the tenant's provisioned dimension (same model), so
  no dimension mismatch is possible.

## Testing

- `embedcache` unit tests: hit, miss, partial, model-mismatch-is-miss, empty input no-query
  (fake `tenant.DB`/store).
- `sink` test: a changed document with N byte-identical + M changed chunks calls the embedder for
  **exactly M** texts and reuses N vectors; the committed chunks carry the right `content_hash`; stats
  report `ChunksReused=N`, `ChunksEmbedded=M`.
- e2e (`-tags e2e`): re-ingest a document with one changed section against the live stack and assert
  most chunks are reused (embedder call count drops) and retrieval still returns the changed content.

## Rollout

Ships with the current wipe-and-re-provision (the schema edit rides `00001`). No data migration. No
connector changes. Existing document-level short-circuit and conditional fetch continue to run first,
so chunk-level reuse only engages for documents that actually changed.
