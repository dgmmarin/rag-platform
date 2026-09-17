# ISSUE-0076: Reuse embeddings for unchanged chunks on re-ingest (chunk-level drift)

**Type:** Feature · **Status:** Done · **Priority:** High · **Traces:** FR-SRC-01, SPEC-05 §1/§5, ADR-0008

## Summary
The sink skipped re-embedding an **unchanged document** but re-embedded **every chunk** of a document
that changed at all — a one-line edit in a large page paid to re-embed the whole page. This adds
chunk-level reuse: on a changed document, embed only chunks whose text is new, and reuse the existing
vector for any chunk whose embed-text is byte-identical to one already embedded under the same model,
anywhere in the tenant.

## What was built
- **Schema:** `chunks.content_hash bytea not null` (= `sha256(embed-text)`) and index
  `(content_hash, embedding_model)`, added in place to `00001_initial_schema.sql` (rides the ADR-0076
  wipe-and-re-provision); `schemas/tenant.sql` mirror synced.
- **Service `internal/ingest/embedcache`:** `Lookup(ctx, db, model, hashes)` returns the vector an
  existing chunk already holds for each `(hash, model)` — the reuse rule, owned in one place.
- **Store:** `documents.ChunkInput.ContentHash` persisted per chunk.
- **Sink:** hashes each chunk, looks up hits, embeds only the misses (skips the embed call when all
  hit), reassembles vectors in order, commits in one transaction (ADR-0008). Stats report
  `chunks_embedded` / `chunks_reused`.
- **Wiring:** `embedcache.NewPgCache()` at both production sinks (worker ingest + sync).
- **Metric:** `embed_chunks_reused_total{tenant,provider}`.
- **Decision recorded:** ADR-0077.

## Regression fixed on this branch
Adding the NOT NULL `chunks.content_hash` column broke live-DB e2e fixtures that built `ChunkInput`
without it (`TestDocumentStorePutGoldenPath`, `TestRetrieveEndpointGoldenPath` → NOT NULL violation).
Both fixtures now set `ContentHash: sha256Bytes(<chunk content>)`. `validatePut`'s unit fixture needs
no change (it never reaches the DB).

## Tests
- `internal/ingest/embedcache` unit tests (hit / miss / partial / model-mismatch / empty).
- `internal/ingest/sink` `TestPutReusesKnownChunkEmbeddings`, `TestPutEmitsEmbedChunksReusedMetric`.
- e2e `TestEmbedCacheReuseOnReingest` (one changed chunk → 1 embedded, N-1 reused) against the live
  stack; `go test -tags e2e ./test/e2e/` green.

## Related
ADR-0077, ADR-0076 (VectorChord), ADR-0008 (single-transaction commit).
