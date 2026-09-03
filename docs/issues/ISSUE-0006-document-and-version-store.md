# ISSUE-0006: Document and version store

**Type:** Feature · **Status:** Done · **Story:** STORY-05.1 · **Traces:** FR-ING-02, ADR-0008, SPEC-03, SPEC-05 §5, ADR-0033

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-05.1 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Implement the write side of the tenant document/version store (ADR-0008,
SPEC-05 §5): a `Put` operation that persists a fully-built, embedded document
version and makes it live atomically. This is the persistence half of the
ingestion sink; STORY-04.4 (ADR-0030) built the read/soft-delete side and
deferred this write path here.

## Scope
- Add `TenantStore.Put(ctx, *tenant.DB, PutInput) (PutResult, error)` (and the
  method to the `Store` port) in `internal/documents` — the same tenant content,
  tables and resolver-only access (ADR-0003, C-1, C-3) as the existing read store.
- Honour SPEC-03 §2 invariants and SPEC-05 §5 commit semantics:
  - unchanged content hash → only touch `last_seen_at` (no version, no chunk churn);
  - new/changed hash → insert an immutable `document_versions` row + its chunks and
    flip `documents.current_version` in ONE transaction, so `live_chunks` reflects
    the swap instantly and never a half-built version;
  - a rollback to a prior content hash reuses that version and its chunks.
- Embeddings are a required input at commit (no unembedded staging); the vector is
  written via a pgvector text literal + `$n::vector` (no codec dependency).

## Resolution
- `internal/documents/put.go`: `PutInput`, `ChunkInput`, `PutResult`,
  `TenantStore.Put`, `validatePut`, `vectorLiteral`, `nullableJSON`. `Put` added to
  the `Store` interface (`store.go`) and to the `service_test.go` fake.
- No HTTP route / OpenAPI change (a pure data-layer store) and no schema/migration
  change (`documents`/`document_versions`/`chunks`/`live_chunks` already exist), so
  the drift guard stays green.
- Design decisions (package placement, embedded-at-commit, rollback reuse, vector
  encoding) are recorded in ADR-0033.

## Verification
- TDD: `internal/documents/put_test.go` (unit: `vectorLiteral`, `validatePut`)
  written first and watched fail to compile, then made green.
- `go test ./...` (module-wide unit suite) green.
- `go test -tags e2e -run TestDocumentStorePutGoldenPath ./test/e2e/...` green
  against the real enrolled tenant DB + control-plane Postgres — proves all four
  acceptance bullets plus the rollback edge and that `live_chunks` never exposes a
  non-current version's chunks. `TestTenantIsolationSuite` (SPEC-01 §9) and
  `TestDocumentsGoldenPath` re-run green.
- Coverage gate (`mise run coverage`) PASS: `internal/tenant` 70.7%, `internal/ingest`
  SKIP (not created), `internal/documents` not gated. Lint clean on the touched
  packages (`internal/documents`; the new e2e file has 0 issues).

## Notes / not in scope
- The parser (STORY-05.2/05.3), chunker (STORY-05.4) and embedder (STORY-05.5)
  produce `PutInput`; they are not built here. The sink orchestration + job stats
  (STORY-05.6/05.7) consume `Put` through the `Store` port.
- Pre-existing, unrelated local check noise (documented in ISSUE-0003): a
  golangci-lint finding in `internal/cp/audit`/`audit_e2e_test.go` and an
  env-sensitive `internal/cli` test under mise `.env` injection; a MinIO `:9000`
  port conflict can block `mise run up` (the e2e was run against the already-running
  Postgres, which is all this story needs).
