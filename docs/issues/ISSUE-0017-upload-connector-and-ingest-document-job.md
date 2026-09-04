# ISSUE-0017: Upload connector and ingest_document job

**Type:** Feature · **Status:** Done · **Story:** STORY-06.3 · **Traces:** FR-SRC-02, SPEC-04 §5, SPEC-05, SPEC-07 §2, ADR-0042

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-06.3 for traceability; the backlog
> story remains the authoritative work item. STORY-06.3 completes EPIC-06 (13/13).

## Summary
Make the upload path real end to end (SPEC-04 §5, FR-SRC-02): a file uploaded to
`POST /v1/documents` is written to object storage, an `ingest_document` job is
enqueued, and when that job runs it becomes a queryable document; re-uploading the
same filename creates a new version. The story fills the STORY-04.4 `Storage` seam
with a real S3-compatible backend (MinIO locally), registers the `upload` connector,
adds MIME sniffing and a per-tenant size limit, resolves the implicit upload source,
and builds the `ingest_document` handler that drives the STORY-05 pipeline. The
River worker/dispatch loop is explicitly NOT built (EPIC-09).

## Scope
- `internal/objectstore`: S3-compatible client (`aws-sdk-go-v2/service/s3`, reusing
  the vendored SDK core) with `Put`/`Get`, path-style addressing, custom endpoint,
  bucket-ensure. Fail-closed on missing config.
- `internal/connector/upload`: the `upload` connector — `ValidateConfig` (any object),
  `Test` (no-op success), `Sync` (`ErrNotScheduled`); registered via `init()`.
  Blank-imported at the composition root.
- `internal/documents`: `sniffUpload` (byte sniff + extension allowlist, never trust
  the client Content-Type); `UploadSource`/`UploadLimits` ports; `MaxBytesForTenant`;
  implicit-source resolution in `Ingest`; `ControlUploadSource` (upsert on the
  existing `sources` table) and `SettingsUploadLimits` (reads
  `settings.limits.max_upload_mb`); handler now sniffs bytes and uses the per-tenant
  ceiling.
- `internal/ingest/ingestdoc`: the `ingest_document` handler — `Ingestor.Dispatch`/
  `Run` (fetch bytes → settings → embedder → single-document INCREMENTAL sink → commit),
  `JobFromPayload`, `parseSettings`. The document ROW + first version are created here,
  atomically (ADR-0008), not by the HTTP handler.
- `internal/config`, `.env.example`: `OBJECT_STORE_*` settings.
- `internal/cli/api_server.go`: wire the object store, implicit-source resolver and
  per-tenant limit into the documents service; register the upload connector.
- Not in scope: the River worker / queue dispatch loop and the production
  `EmbedderFactory` with provider keys (EPIC-09 STORY-09.1); crawl/sitemap/api
  connectors (EPIC-07).

## Resolution
- **Object storage.** `aws-sdk-go-v2/service/s3` reuses the already-vendored SDK core
  (no `minio-go`); one client path for local MinIO and production (ADR-0042). Body is
  buffered (bounded by the upload ceiling) so SigV4 has a length-known payload
  (`ponytail:` upgrade path noted).
- **Doc-row-creation boundary (SPEC tension resolved).** SPEC-04 §5 says the upload
  path "creates a document row," but ADR-0008 + SPEC-03 §2 invariant 1 require an
  active document to have a non-null `current_version` with no pending status. The
  invariant wins (README-vs-ADR convention): the HTTP handler creates **no row**; the
  `ingest_document` handler creates the row **and** its first version together in the
  one `TenantStore.Put` transaction, so no half-built/version-less document is ever
  visible. Documented in ADR-0042 and SPEC-04 §5.
- **Upload not scheduled.** The connector's `Sync` returns `ErrNotScheduled`; documents
  arrive one `ingest_document` job at a time. Ingest runs INCREMENTAL (no full-sync
  Complete), so ingesting one upload never soft-deletes the tenant's other documents.
- **Re-upload → new version.** Identity is `(implicit upload source, filename)`; new
  content hashes to a new immutable version and flips `current_version` (STORY-05.1).
- **Size from settings.** `settings.limits.max_upload_mb` per tenant, failing safe to
  the global `MAX_UPLOAD_BYTES`.
- **MIME sniffing.** Extension allowlist AND `http.DetectContentType` on the bytes; a
  mislabelled/hostile file (executable as `.txt`, non-PDF as `.pdf`, non-zip `.docx`)
  is rejected `400`.
- **No migration.** The implicit upload source is an ordinary `sources` row (kind
  `upload`, `(tenant_id, name)` unique — both already exist). No schema change; the
  drift guard stays green.
- **No OpenAPI change.** The route and its response already exist (STORY-04.4); no
  request/response schema changed, so `api/openapi.yaml` is unchanged and its drift
  guard stays green.

## Verification
- TDD throughout (tests watched red before implementation): `sniffUpload`
  (match/mismatch/unknown-ext); service implicit-source resolution and per-tenant
  limit; handler byte-sniff rejection and per-tenant oversize; upload connector
  (kind/validate/test/sync/registration); objectstore fail-closed; `ingestdoc`
  (`parseSettings`, `JobFromPayload`, `Run` creates-version / skips-unchanged);
  `SettingsUploadLimits` extraction.
- `mise run test`: unit packages green except the pre-existing `internal/cli` env-only
  reds (mise `.env` injection leaks `CONTROL_PLANE_URL`/age-key; pass with clean env).
- `mise run lint` / `mise run coverage`: see the backlog delivery note for the
  pre-existing toolchain-drift reds unrelated to this change; new packages covered.
- e2e (`test/e2e/upload_ingest_e2e_test.go`) against the real stack (real MinIO, real
  control-plane Postgres, a real enrolled tenant DB; embedding provider stubbed): the
  bytes land in MinIO (read back via the S3 client), a real `ingest_document` job is
  enqueued, `Ingestor.Dispatch` creates the document + first version + chunks, and a
  re-upload of the same filename creates a second version and flips `current_version`.
  Assertions use the pgxpool / S3 client directly, never `docker compose exec`
  (ISSUE-0014).
