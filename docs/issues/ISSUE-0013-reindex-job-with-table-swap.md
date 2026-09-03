# ISSUE-0013: Reindex job with table swap

**Type:** Feature · **Status:** Done · **Story:** STORY-05.8 · **Traces:** FR-ING-09, SPEC-05 §7, SPEC-03 §5, ADR-0039

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-05.8 for traceability; the backlog
> story remains the authoritative work item.

## Summary
The resumable reindex operation (SPEC-05 §7): migrate a tenant's live chunks to a
new embedding model and/or dimension while retrieval keeps serving from the current
`chunks` table, with an atomic swap (SPEC-03 §5) and the old table dropped only after
a coverage verification. The River worker that schedules/drives it is STORY-09.1 and
is out of scope; this story exposes the operation for the worker to drive.

## Scope
- `internal/ingest/reindex` (new orchestration package) and
  `internal/documents/reindex.go` (the tenant-side DDL/DML on `TenantStore`, reached
  only through `*tenant.DB`, ADR-0003).
- Exported `chunk.WithContext` so the reuse path reconstructs the SPEC-05 §3 embed
  context line without re-chunking.
- Not in scope: the River worker/scheduling (STORY-09.1), the GC job (STORY-05.9),
  connectors (EPIC-06/07), and the control-plane `settings.embedding_dim` mirror
  write (the worker's finalize step, ADR-0039).

## Resolution
- **Operation.** `reindex.Reindexer` sequences `Prepare` → `Step(cursor)`\* →
  `Verify` → `Swap` → `DropOld`. It owns no SQL — it composes the store, the chunker
  and the `Embedder`.
- **Build off the hot path.** `CreateChunksNew` builds an empty `chunks_new` at the
  new `vector(N)` (`LIKE chunks` + `ALTER … TYPE vector(N)` on the empty column +
  explicit PK/unique/FKs/indexes; the dimension is an interpolated positive-int, as a
  typmod cannot be a bind parameter). `live_chunks` still reads `chunks`, so queries
  are undisturbed while `chunks_new` is populated.
- **Resumable.** `Step` reads live versions after a `document_id` cursor in id order,
  re-embeds one batch with the new model, and returns the advanced cursor for the
  caller to persist. `InsertChunksNew` is atomic + idempotent per version
  (delete-then-insert in one tx), so a crash/retry resumes rather than restarts or
  duplicates.
- **Verify-before-swap/drop.** `VerifyCoverage(table)` counts live versions
  represented in a target table. `Swap` refuses (`ErrNotVerified`) unless `chunks_new`
  covers every live version; `DropOld` re-checks the now-live `chunks` table before
  dropping `chunks_old` — a durable-state check that holds across a crash between
  swap and drop.
- **Atomic swap.** `SwapChunks` runs SPEC-03 §5 in one DDL transaction: drop
  `live_chunks`, rename `chunks`→`chunks_old` and `chunks_new`→`chunks`, recreate
  `live_chunks`. No query sees a half-swapped state.
- **Dimension change vs ADR-0022.** The reindex is the sanctioned path the
  immutability points at. The physical `vector(N)` moves in the swap; the
  control-plane `settings.embedding_dim` mirror (and configured model) is moved by the
  driving worker as the job's finalize step, so the mirror never desyncs from the live
  column (ADR-0039).

## Verification
- TDD: unit tests in `internal/ingest/reindex/reindex_test.go` over a fake store +
  fake embedder; the verify-before-swap and verify-before-drop gates were watched go
  **red** (with the coverage guard disabled both `TestSwapRefusesWhenNotCovered` and
  `TestDropRefusesWhenNotCovered` fail) then **green**. Covers Prepare/Step batching
  and cursor, resume-not-restart, reuse embed-text reconstruction, the re-chunk
  branch, and dimension/model preconditions.
- e2e (`test/e2e/reindex_e2e_test.go`, build tag `e2e`) against a **real enrolled
  tenant DB** (dim 768 → 384): ingests 3 docs via the sink, then proves live_chunks
  keeps serving the old `vector(768)` table during a partial build, a refused swap
  when only 1/3 versions are covered, resume-from-cursor completing the remaining 2
  docs, the atomic swap flipping `live_chunks` to `vector(384)` with the new model,
  and `chunks_old` dropped only after the post-swap verification. Ran green in ~76 s.
- `gofmt -l` clean; `go vet` clean; `go test -count=1 ./internal/ingest/...
  ./internal/documents/...` green; pinned `golangci-lint v2.13.1` **0 issues** on
  `internal/ingest/reindex/...`, `internal/documents/...` and `internal/ingest/chunk/...`
  (the forbidigo `Unsafe()` ban stays satisfied — the reindex uses only
  `db.Exec`/`Query`/`Begin`).

## Notes / not in scope
- No schema/migration/OpenAPI change: `chunks_new`/`chunks_old` are transient tables
  created and dropped through `*tenant.DB`; `schemas/tenant.sql` and the tenant
  migration are unchanged, so the drift guard stays green.
- Coverage: `internal/ingest/reindex` sits under `internal/ingest` (gate SKIP; the
  subpackage is not gated).
- `TestTenantIsolationSuite` could not be executed in this environment: every
  assertion runs through `docker compose exec`, which is currently wedged here (a bare
  `docker compose exec postgres psql -c 'select 1'` did not return within 120 s),
  while the DB itself is healthy — the reindex e2e over the direct control pool ran
  fine. The isolation-relevant guarantee (the `Unsafe()` ban) is enforced by lint,
  which is green; the per-tenant role's ability to create/swap/drop its own tables is
  proven by the reindex e2e.
- Ceilings (ponytail, ADR-0039): the `chunks_new` DDL mirrors `schemas/tenant.sql`; a
  *second* reindex would collide on `chunks_new_*` names (upgrade: canonicalize in
  `DropChunksOld`); HNSW is built incrementally; `Rechunk` re-parses stored markdown,
  not original bytes.
