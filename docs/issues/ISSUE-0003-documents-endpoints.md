# ISSUE-0003: Documents endpoints

**Type:** Feature · **Status:** Done · **Story:** STORY-04.4 · **Traces:** FR-SRC-02, FR-ADM-03, SPEC-07 §2, ADR-0030

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-04.4 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Implement the SPEC-07 §2 documents surface — `POST /v1/documents` (multipart
upload), `GET /v1/documents` (filter by source, status, q), `GET /v1/documents/{id}`
(current-version metadata, optional `?content=true`), `DELETE /v1/documents/{id}`
(soft delete) and `GET /v1/documents/{id}/chunks` (admin debugging) — with the
tenant derived from the authenticated principal (FR-ACC-03).

## Scope
- New `internal/documents` package (tenant content, so **not** under
  `internal/cp`): a `Store` (`TenantStore` over `*tenant.DB`), a `Service`
  (resolver + control-plane `JobEnqueuer` + optional `Storage` seam), `Handlers`.
- Documents/versions/chunks are tenant content (C-3); the read/delete path reaches
  the tenant database ONLY through a `tenant.DB` from the resolver (ADR-0003), with
  no `tenant_id` column and no control-plane access.
- Wire the five routes into `internal/api` (`New` + `liveRoutes()`): `ingest` scope
  for upload/delete, `query` for list/get, `admin` for chunks; regenerate
  `api/openapi.yaml`. Wire the resolver + documents handlers into `buildAPIServer`
  (finally using the reserved startup cipher).

## Resolution
- List (`?source&status&q&limit&cursor` → `{items,next_cursor}`), get (with
  current-version metadata and `?content=true`), the chunks debug endpoint (never
  returns the embedding vector), and the soft delete are fully implemented against
  the tenant schema via the resolver.
- `POST /v1/documents` validates the upload (FR-SRC-02 type allowlist +
  configurable size ceiling, default 50 MB via `MAX_UPLOAD_BYTES`) and enqueues a
  real `ingest_document` job in the control-plane `jobs` table; the
  `Idempotency-Key` is replayed via `jobs.payload`.
- Object storage is the EPIC-06 seam: the `Storage` port is nil today, so the
  upload returns the not_found seam envelope (fail closed) until STORY-06.x wires
  it. No document row is created on upload — the row + first version are built by
  the ingest worker/document store (STORY-05.1, ADR-0008) in one transaction, so a
  partial `active` document is never visible (SPEC-03 §2 invariant 1); the `202`
  response carries the queued job.
- Design decisions and deferrals are recorded in ADR-0030. No schema/migration
  change (the `documents`/`document_versions`/`chunks` tables and the
  `ingest_document` job kind already existed), so the drift guard stays green. A
  new `MaxUploadBytes` config knob realises FR-SRC-02's "configurable size".

## Verification
`go test ./...` (unit: `internal/documents`, `internal/api`, `internal/config`,
`internal/cli`), `mise run openapi` (regenerated the YAML; drift guard green), and
`go test -tags e2e ./test/e2e/...` (`TestDocumentsGoldenPath` over a real enrolled
tenant DB + control-plane Postgres — list/get/`?content`/chunks/filter/delete +
the real `ingest_document` enqueue; `TestOpenAPIContractGoldenPath`,
`TestAPIRouterGoldenPath`, `TestSourcesGoldenPath` and `TestTenantIsolationSuite`
still green). Lint clean on the touched packages. See the STORY-04.4 delivery note
in `docs/backlog/BACKLOG_STATUS.md`.

## Notes / not in scope
- Object storage (EPIC-06), the document/version store + upload connector
  (STORY-05.1 + EPIC-06), and the ingest worker (EPIC-09) are injected seams, not
  built here.
- The full `mise run test`/`mise run lint`/`mise run coverage` tasks are red in
  the current local environment for reasons that pre-date and are unrelated to
  this change: a golangci-lint/Go-toolchain drift flagging two EPIC-03 audit files
  (`internal/cp/audit/pool.go`, `audit_e2e_test.go`), and env-sensitive
  `internal/cli` "RequiresURL" tests under mise's `.env` injection (they pass when
  the leaked `CONTROL_PLANE_URL`/age-key env is cleared). No gated package
  (tenant/ingest/retrieve/connector) was touched by this story.
