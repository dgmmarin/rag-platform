# ADR-0039: Reindex table swap — a resumable, verify-before-drop operation over `*tenant.DB`, and how a dimension change reconciles with `embedding_dim` immutability

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** FR-ING-09, SPEC-05 §7, SPEC-03 §5, SPEC-03 §2 (Invariant 3), C-1, C-3 · **Decisions:** ADR-0003, ADR-0004, ADR-0008, ADR-0022, ADR-0033, ADR-0038

## Context
STORY-05.8 delivers the reindex operation: migrate a tenant's live chunks to a new
embedding model and/or dimension while retrieval keeps serving, resumably, with an
atomic swap and the old table dropped only after a verification flag (SPEC-05 §7,
SPEC-03 §5). STORY-05.1–05.7 built the persistence store (`documents.TenantStore`,
ADR-0033) and the ingestion sink (ADR-0038). The River worker that would *schedule*
and *drive* a reindex job is STORY-09.1 and is explicitly out of scope; this story
exposes the operation as a library the worker will later drive.

Two design facts had to be reconciled:

1. **A dimension change appears to violate ADR-0022.** `settings.embedding_dim` is
   immutable to ordinary settings PATCHes — the settings service rejects a changed
   dimension with `409` and points the caller at *this* reindex path. So the reindex
   is not a violation of the invariant; it is the invariant's sanctioned escape
   hatch. The question is *how* the physical `vector(N)` column and the control-plane
   `embedding_dim` mirror move to the new value without ever desyncing.

2. **The DDL/swap is tenant DDL and must not reach for a raw pool.** ADR-0003 and the
   forbidigo `Unsafe()` ban (ADR-0018) forbid a raw `pgxpool` outside `internal/tenant`
   and `cmd/ragctl`. A reindex is nothing but `CREATE TABLE`/`ALTER`/`INSERT`/`DROP`
   against the tenant database, so it must run through the `*tenant.DB` handle's
   `Exec`/`Query`/`Begin`, exactly like `Put`.

## Options
- **Where the reindex SQL lives.** (a) a new store package — rejected: the reindex
  chunks are the *same* tenant content on the *same* tables as `Put`, so the SQL
  belongs with the existing tenant-document store (the ADR-0033 precedent). (b)
  **`internal/documents/reindex.go` on `TenantStore`** (chosen): `CreateChunksNew`,
  `LiveVersionsAfter`, `VersionChunks`, `InsertChunksNew`, `VerifyCoverage`,
  `SwapChunks`, `DropChunksOld` — reached only through `*tenant.DB` (ADR-0003, C-1,
  C-3). They are **not** added to the `documents.Store` interface: like the sink
  (ADR-0038), the orchestration package declares its own narrow port satisfied
  structurally by `TenantStore`, so the documents Service/handlers keep a minimal
  Store surface (ISP).
- **Where the orchestration lives.** New package `internal/ingest/reindex` (under
  `internal/ingest`, coverage-gate SKIP exactly as ADR-0033/0037/0038 note). It owns
  no SQL — it composes the store, the chunker and the `Embedder` (the same seams the
  sink uses) and sequences Prepare → Step\* → Verify → Swap → DropOld.
- **Building `chunks_new`.** SPEC-03 §5 step 1 wants a side table at the new
  dimension with indexes. `create table chunks_new (like chunks including defaults
  including generated)` copies the columns, NOT-NULLs, defaults and the generated
  `tsv`; the dimension is then moved on the still-index-free (empty) `embedding`
  column with `alter column … type vector(N)` (a typmod change a bind parameter
  cannot express — the integer is interpolated after a positive-int guard, exactly as
  the migration runner substitutes `EMBEDDING_DIM`, ADR-0015); finally the primary
  key, the `(version_id, position)` unique, the two `on delete cascade` foreign keys
  (so GC's version-delete still cascades chunks after the swap) and the btree/GIN/HNSW
  indexes are added explicitly with `chunks_new_*` names. Building the HNSW index
  incrementally as rows are inserted (rather than after load) is the simple, resumable
  choice (ponytail ceiling noted; upgrade path is a build-after-load for very large
  corpora, SPEC-03 §3).
- **Queries keep serving during the build.** `chunks_new` is a *separate* table; the
  `live_chunks` view still reads `chunks`, so retrieval is unaffected while the side
  table is populated. The e2e asserts `live_chunks` count and the physical
  `vector(768)` type are unchanged mid-build.
- **Resumability.** `Step(ctx, cursor)` reads live versions (active documents'
  `current_version`) in **`document_id` order** after the cursor, re-embeds one
  batch, and returns the advanced cursor; the caller (worker) persists it in the job
  payload. Two properties make a crash/retry resume rather than restart or duplicate:
  the cursor skips already-indexed documents, and `InsertChunksNew` is **atomic and
  idempotent per version** (it `delete`s that version's `chunks_new` rows then inserts
  in one transaction), so re-doing the in-flight version after a crash is clean rather
  than a `(version_id, position)` collision.
- **Re-chunk vs re-embed.** SPEC-05 §7 re-chunks *only if chunking settings changed*.
  Default (`Rechunk=false`): re-embed the stored chunk text verbatim (same
  position/heading_path/content), reconstructing the SPEC-05 §3 context line via the
  exported `chunk.WithContext` so the new vectors match how the sink embedded — exact
  and cheap. `Rechunk=true`: re-split the stored normalised markdown
  (`document_versions.content`) with the chunker. The re-chunk path re-parses the
  *stored markdown*, not the original bytes (ponytail ceiling; upgrade path re-fetches
  `raw_ref`).
- **Verify-before-swap and verify-before-drop.** `VerifyCoverage(table)` counts how
  many live versions are represented in a target table. `Swap` refuses with
  `ErrNotVerified` unless `chunks_new` covers every live version; `DropOld` re-runs
  the same check against the *now-live* `chunks` table (post-swap) before dropping
  `chunks_old`. The drop gate reads durable DB state, not an in-memory flag, so it
  holds even across a crash between Swap and DropOld — the old table is never dropped
  before the new one is proven complete (SPEC-05 §7).
- **The atomic swap.** `SwapChunks` runs SPEC-03 §5 step 3 in ONE transaction: drop
  `live_chunks`, rename `chunks`→`chunks_old` and `chunks_new`→`chunks`, recreate
  `live_chunks` over the new `chunks`. Postgres DDL is transactional, so no query ever
  sees a half-swapped state (a reader blocks on the brief rename lock, then sees the
  new table). The view is dropped-and-recreated because a view binds to the table by
  OID: left alone, `live_chunks` would keep pointing at the renamed old table.

## Decision
Add `internal/ingest/reindex` (`Reindexer`, `Config`, `Progress`, the `Store`/
`LocalParser` ports, `ErrNotVerified`) and the seven `TenantStore` methods in
`internal/documents/reindex.go`. The physical `vector(N)` column moves to the new
dimension entirely within the tenant database: `chunks_new` is built at `NewDim` and
the swap renames it to `chunks`. **The control-plane `settings.embedding_dim` mirror
(and the configured embedding model) is moved to the new value by the driving worker
as the finalize step of the same reindex job** — the worker holds the control pool;
this operation holds only a `*tenant.DB` and cannot (and must not, C-3) reach control-
plane state. This keeps ADR-0022 honest: the mirror is immutable to ordinary PATCHes
precisely so the change flows through the reindex, and the mirror is only ever moved
in lockstep with a completed physical swap, so `settings.embedding_dim` can never
desync from the live `vector(N)` column. Writing that mirror is the sanctioned
dimension-change write, exactly as provisioning writes `jsonb_build_object(
'embedding_dim', N)` (ADR-0022); it is a one-line control-plane write deferred to the
worker (STORY-09.1), not built here.

## Consequences
- The worker (STORY-09.1) builds one `Reindexer` per reindex job: resolve the tenant
  DB, pick the new-model `Embedder` (ADR-0037), `Prepare`, loop `Step` persisting the
  cursor, `Verify`, `Swap`, `DropOld`, then write the new `embedding_dim`/model into
  `tenants.settings` and mark the job done. `*SnoozeError`-style provider-quota
  handling is the worker's (the operation surfaces the embed error; the worker
  classifies it).
- Invariant 3 (SPEC-03 §2) holds: during the reindex the old `chunks` carry the old
  model and `chunks_new` the new; the swap makes the new model live in one step, and
  the worker's mirror update makes the configured model agree.
- **Schema/migration change:** none. `chunks_new`/`chunks_old` are transient tables
  the operation creates and drops through `*tenant.DB`; `schemas/tenant.sql` and the
  tenant migration are unchanged, so the drift guard stays green. No OpenAPI change
  (no HTTP surface in this story).
- **Ceilings (ponytail):** (1) the `chunks_new` DDL mirrors the `chunks` shape in
  `schemas/tenant.sql`; if that shape changes, update `reindex.go` too. (2) After a
  swap the live table carries `chunks_new_*` index/constraint names, so a *second*
  reindex would collide on those names in `CreateChunksNew`; upgrade path is to rename
  them back to the canonical `chunks_*` names inside `DropChunksOld`, where
  `chunks_old` has freed them. (3) HNSW is built incrementally during the load rather
  than after it. (4) `Rechunk` re-parses the stored markdown, not the original bytes.
  None affect isolation, and none can leave a partially-swapped or partially-dropped
  state.
- Tests: hermetic unit tests over a fake store + fake embedder (Prepare/Step batching
  and cursor, resume-not-restart, reuse embed-text reconstruction, re-chunk branch,
  and the verify-before-swap/drop gates watched red first) and a DB e2e over a real
  enrolled tenant (`test/e2e/reindex_e2e_test.go`) proving queries serve the old table
  during the build, resume-from-cursor, the verify-refused swap, the atomic swap to a
  new dimension, and verify-then-drop of `chunks_old`.
