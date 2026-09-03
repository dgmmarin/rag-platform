# ADR-0033: Document/version write store — Put in the documents package, embedded-at-commit, atomic swap

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** FR-ING-02, FR-ING-07, NFR-REL-02, C-1, C-3, SPEC-03 §2, SPEC-05 §5 · **Decisions:** ADR-0003, ADR-0008, ADR-0030

## Context
STORY-05.1 delivers the persistence half of the ingestion sink: given a
fully-built, embedded document version, write it and make it live. ADR-0008 fixes
the model (`documents` pointer + immutable `document_versions` + `chunks`, swap by
one pointer flip); SPEC-05 §5 fixes the commit semantics (one transaction inserts
the version and all chunks, then `UPDATE documents SET current_version=…,
last_seen_at=now(), status='active'`; the transaction is not opened until
embeddings exist). STORY-04.4 (ADR-0030) built the READ/soft-delete side over the
same tenant tables in `internal/documents` and explicitly deferred the WRITE side
here. The remaining pipeline stages (parse 05.2/05.3, chunk 05.4, embed 05.5) and
the sink orchestration + job stats (05.6/05.7) are separate stories.

## Options
- **Where the write store lives.** (a) a new `internal/ingest` package — rejected
  for this story: `internal/ingest` is coverage-gated at 70% (NFR-MNT-03,
  `.ci/coverage-packages.txt`), but a pure-SQL store can only be exercised against
  a real tenant DB (a `tenant.DB` is unforgeable — ADR-0003), and the coverage
  task runs without Postgres, so the gate could not be met without contrived
  filler; birthing it half-empty also fights the gate before its real inhabitant
  (the 05.6 sink orchestration, which *is* unit-testable) arrives. (b) extend
  `internal/documents` (chosen): the write store is the same tenant content
  (`documents`/`document_versions`/`chunks`, C-3), the same tables, the same
  `*tenant.DB` port and the same e2e-tested-SQL discipline as the existing
  read/delete methods — one cohesive tenant-document store, not gated, no new
  package surface (the lazy, consistent choice). The sink *orchestration* is free
  to land in `internal/ingest` at 05.6 and consume this store through its `Store`
  port.
- **Embeddings at commit.** SPEC-05 §5 opens the transaction only once the version
  is embedded, so `Put` takes chunks that already carry their `embedding` and
  `embedding_model`; `validatePut` rejects an empty embedding or model. There is no
  unembedded chunk staging — producing the vectors is the caller's job (the
  Embedder, STORY-05.5). This keeps Invariant 3 (live chunks carry the tenant's
  model) enforceable and matches ADR-0008's "no partially-updated document".
- **Change detection.** `Put` compares the input `content_hash` to the *current*
  version's hash only (ADR-0008 step 2). Equal → touch `last_seen_at`, return
  `Changed=false`, no version, no chunk churn, no embedding cost. Otherwise the
  changed/new path runs.
- **Rollback to a prior content (A→B→A).** The new hash differs from B (current)
  but equals A (a retained, non-current version). Because `document_versions` is
  unique on `(document_id, content_hash)` and chunks are immutable (Invariant 2),
  `Put` reuses version A and its existing chunks and only re-points
  `current_version` — a pointer change, exactly ADR-0008's "rollback is a pointer
  change". No duplicate version, no re-embedding.
- **Writing the vector without a codec.** No pgvector Go codec is a dependency;
  `Put` renders `[]float32` to the pgvector text literal (`[a,b,c]`) and casts
  `$n::vector`. The column type validates the dimension. Avoids a new dependency
  for one column (the lazy choice); metadata is sent as `$n::jsonb` text (a raw
  `[]byte` would encode as `bytea`, which cannot cast to `jsonb`).

## Decision
Add `TenantStore.Put(ctx, *tenant.DB, PutInput) (PutResult, error)` to
`internal/documents` (and to the `Store` port). `PutInput` carries the document
identity `(source_id, external_id)` + metadata, the version fields
(`content_hash`, normalised `content`, `char_count`, `parser`, optional `raw_ref`
/`raw_json`) and the embedded `Chunks`. `Put`:
1. looks up the document by `(source_id, external_id)` and its current version's
   hash;
2. if the hash is unchanged, updates only `documents.last_seen_at` and returns
   `Changed=false`;
3. otherwise, in ONE transaction (`db.Begin`): upserts the identity (reactivating
   and touching it — SPEC-05 §5), inserts the new immutable version (or reuses a
   prior version with that exact hash), inserts its chunks (only for a genuinely
   new version), then flips `documents.current_version` (+`last_seen_at`,
   `status='active'`) and commits — so a reader of `live_chunks` sees the old
   version until commit and the new one instantly, never a half-built version.
Reached only through a `*tenant.DB` from the resolver (ADR-0003, C-1, C-3).

## Consequences
- The STORY-05.1 acceptance contract holds and is e2e-tested against a real
  enrolled tenant DB (`TestDocumentStorePutGoldenPath`): unchanged hash touches
  only `last_seen_at`; a changed hash inserts a version; `current_version` flips in
  the same transaction as the chunks; `live_chunks` reflects the swap instantly and
  never exposes a non-current version's chunks; a rollback reuses the prior version.
- No schema/migration change: `documents`/`document_versions`/`chunks` and
  `live_chunks` already exist (schemas/tenant.sql), so the drift guard stays green.
- `internal/documents` gains the write half of the tenant-document store; the
  ingest sink orchestration (parse→normalise→hash→chunk→embed) and job stats stay
  for STORY-05.2..05.7 and consume `Put` through the `Store` port (NFR-MNT-01).
- The soft-delete + GC lifecycle (SPEC-03 §4, FR-ING-07 `Sink.Complete`) is
  unchanged; `Put`'s reactivation on reappearance complements it.
