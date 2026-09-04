# ADR-0042: Object-storage backend (S3-compatible, aws-sdk-go-v2) and the upload → ingest_document path

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-SRC-02, C-3, C-4, SPEC-04 §5, SPEC-05, SPEC-07 §2 · **Decisions:** ADR-0003, ADR-0008, ADR-0030, ADR-0033, ADR-0037, ADR-0038, ADR-0040

## Context
STORY-06.3 makes the upload path real: a file uploaded to `POST /v1/documents`
must land in object storage, produce an `ingest_document` job, and — when that job
runs — become a queryable document; a re-upload of the same filename must create a
new version (SPEC-04 §5, FR-SRC-02).

The seams already exist. STORY-04.4 (ADR-0030) shipped the documents API with a
nil `Storage` port (upload returns the `not_found` seam) and, deliberately,
**creates no document row on upload**. STORY-05.1/05.6 (ADR-0008/0033/0038) shipped
the document/version store (`documents.TenantStore.Put`, atomic version + chunks +
`current_version` flip) and the ingestion sink that composes parse→chunk→embed→commit.
STORY-06.1 (ADR-0040) shipped the connector framework and registry. What remained:
the object-storage backend, the concrete upload connector, the per-tenant size
ceiling and implicit source, and the `ingest_document` job handler that wires the
sink to run one uploaded document.

Out of scope (per the story boundary): the River worker / queue dispatch loop
(EPIC-09 STORY-09.1) — the handler is a plain, directly-callable function it will
later dispatch to; and the crawl/sitemap/api connectors (EPIC-07).

## Options and decisions

- **Object-storage client.** (a) Add `minio-go` — rejected: a wholly new dependency
  when the aws-sdk-go-v2 **core is already a direct dependency** (used by the KMS
  provider, ADR-0012). (b) Add `aws-sdk-go-v2/service/s3` (chosen): it reuses the
  existing SDK core, credentials and config machinery, speaks the S3 API MinIO
  implements, and is the same client a production S3 deployment uses — one code path
  for local MinIO and prod. Path-style addressing is forced (`UsePathStyle`, MinIO
  does not do virtual-host buckets) and a custom `BaseEndpoint` is honoured. The new
  package is `internal/objectstore`; adding/replacing the backend touches nothing
  outside it (NFR-MNT-01/02).
- **Streaming vs buffering the body.** `Storage.Put` takes an `io.Reader`; SigV4
  needs a seekable, length-known payload. (a) Add `feature/s3/manager` for streaming
  multipart — rejected as extra surface for bounded inputs. (b) Buffer the object in
  memory (chosen), bounded by the upload ceiling (FR-SRC-02), marked `ponytail:` with
  the manager.Uploader upgrade path for very large objects.
- **Where the document ROW is created (the SPEC tension).** SPEC-04 §5 loosely says
  the upload path "creates a document row." ADR-0008 + the SPEC-03 §2 invariant 1
  say an *active* document must have a non-null `current_version` and there is **no
  pending status**. These conflict for the moment between "bytes uploaded" and
  "version built." **Resolution (invariant wins, per the README-vs-ADR convention):**
  the HTTP handler still creates **no row** — it only writes the bytes to storage and
  enqueues the job (ADR-0030). The `ingest_document` handler (`internal/ingest/ingestdoc`)
  creates the document row **and** its first version **together** in the single
  `TenantStore.Put` transaction (ADR-0008), so a query on `live_chunks` never sees a
  half-built or version-less document, and a crashed/failed ingest leaves nothing
  visible. The `202` response carries the queued job as the client's handle.
- **Upload connector semantics.** The `upload` kind is registered
  (`internal/connector/upload`) so the sources API's config-validation and
  test-connection seams resolve it (no longer the unregistered-kind seam). But upload
  is **not scheduled** (SPEC-04 §5): documents are pushed one `ingest_document` job at
  a time, never enumerated. `ValidateConfig` accepts any JSON object (upload has no
  kind-specific config), `Test` is a no-op success (no external system to reach), and
  `Sync` fails loudly with `ErrNotScheduled` — the scheduler (EPIC-09) never creates a
  `sync_source` job for an upload source.
- **Ingest as INCREMENTAL, single-document.** The handler runs the sink in
  `Incremental` mode with one `Put` and no full-sync `Complete`, so ingesting one
  uploaded file never soft-deletes the tenant's other uploaded documents (a full sync
  would). Identity is `(upload source, filename)`; re-upload → same document, new
  content hash → new immutable version and `current_version` flip (STORY-05.1).
- **The embedding provider is an injected seam.** `EmbedderFactory` builds the
  `Embedder` from tenant settings; the production factory (embed.New + the platform's
  provider keys) is wired with the worker (EPIC-09). Tests inject a deterministic stub,
  as the embedding provider is an external service (matching the sink e2e). The tenant
  settings (provider/model/dim, providers_allowed, chunking) drive the pipeline, so a
  tenant's content only ever reaches a permitted provider (SPEC-09 §2, ADR-0037).
- **Implicit upload source (SPEC-04 §5).** An upload with no explicit `?source` is
  attributed to the tenant's implicit `upload` source, resolved (lazily created on
  first use) by an idempotent upsert on the existing `sources` table keyed by the
  existing `(tenant_id, name)` unique constraint — **no schema change** (kind `upload`
  and the constraint already exist). Sources are control-plane data (C-3), so the
  resolver runs on the control-plane pool, never a tenant DB. With neither an explicit
  source nor a resolver the service fails closed rather than enqueue a source-less job.
- **Size ceiling from settings.** The upload ceiling is the tenant's
  `settings.limits.max_upload_mb` (SPEC-02 §5), read per request; it fails safe to the
  global `MAX_UPLOAD_BYTES` (ADR-0030) when settings are absent, mis-typed, or
  unreadable — the limit is never left unbounded by a settings hiccup.
- **MIME sniffing (never trust the client).** The canonical content type comes from
  the extension allowlist (FR-SRC-02) AND a sniff of the file's leading bytes
  (`http.DetectContentType`); the client-supplied `Content-Type` is ignored. A file
  whose bytes contradict its extension — an executable renamed `.txt`, a non-PDF served
  as `.pdf`, a non-zip `.docx` — is rejected `400` before it can reach storage or a
  parser (a trust-boundary check, AGENTS.md).

## Consequences
- One S3 client path for local MinIO and production; local dev needs only the compose
  MinIO service. No new vendor SDK beyond the aws-sdk-go-v2 family already vendored.
- The upload path is now end-to-end real: bytes in MinIO, a real `ingest_document`
  job, and — when dispatched — a document + version + chunks, with re-upload producing
  a new version. Proven by a golden-path e2e against the real stack (real MinIO + real
  control-plane Postgres + a real enrolled tenant DB; the embedding provider stubbed).
- The document-row-creation boundary is now documented where it belongs (this ADR and
  SPEC-04 §5), so the ADR-0008 invariant is not re-litigated by the looser prose.
- Object storage is optional for API liveness: an unset/unreachable store at boot logs
  a warning and leaves uploads on the seam; reads keep working.
- Deferred to EPIC-09: the River worker that dispatches queued `ingest_document` jobs
  to this handler and wires the production `EmbedderFactory` with provider keys.
