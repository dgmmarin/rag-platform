# ADR-0030: Documents endpoints — tenant content through the resolver, with object-storage and document-store seams

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** FR-SRC-02, FR-ADM-03, FR-ACC-03, C-1, C-3, SPEC-07 §2, SPEC-03 §2, SPEC-04 §5 · **Decisions:** ADR-0003, ADR-0005, ADR-0008, ADR-0027, ADR-0028

## Context
STORY-04.4 must deliver the SPEC-07 §2 documents surface: `POST /v1/documents`
(multipart upload), `GET /v1/documents` (filter by source, status, q),
`GET /v1/documents/{id}` (with current-version metadata, optional
`?content=true`), `DELETE /v1/documents/{id}` (soft delete) and
`GET /v1/documents/{id}/chunks` (admin debugging). It traces FR-SRC-02 (the file
upload connector: PDF/DOCX/Markdown/HTML/TXT/CSV up to a configurable size,
default 50 MB) and FR-ADM-03 (browse documents and their chunks).

Unlike sources (STORY-04.3, ADR-0029), the data here is **tenant content**:
`documents`, `document_versions`, `chunks` and the `live_chunks` view all live in
`schemas/tenant.sql`, not the control plane (C-3). So — unlike every EPIC-04
route mounted so far — this path must reach a tenant database, and the only way
to do that is a `tenant.DB` from the resolver (ADR-0003). STORY-04.1 deferred
session-based `/v1` tenant resolution and mounted only API-key-scoped routes
whose middleware puts the tenant *identity* in the request context; `serve`
opened no resolver (`buildAPIServer` held the cipher "reserved for tenant-plane
resolution wired by later EPIC-04 routes"). This is that story.

Three pieces the full upload flow appears to need do not exist yet:

- **Object storage** for the raw upload bytes — MinIO/S3 is EPIC-06.
- **The document/version store and the upload connector** that create the
  `documents` row and build its first version — STORY-05.1 + EPIC-06.
- **The ingest worker** (River, ADR-0005) that consumes `ingest_document` and
  builds the version + chunks in one transaction — EPIC-09.

The house rule (STORY-04.1/04.2/04.3 precedent): build the HTTP layer and the
read/delete path fully against what the tenant schema already supports, and
inject the not-yet-built pieces as seams that fail closed — do not build
EPIC-05/06/09 work under an EPIC-04 story.

Two facts shape the design:

- A `tenant.DB` is unforgeable by construction (unexported fields, only
  `Resolver.Open` builds one — ADR-0003). So the read/delete SQL is exercised by
  the e2e suite against a real enrolled tenant DB, not a mock; the service's
  non-DB logic (validation, the storage seam, the enqueue, pagination, open-error
  mapping) is unit-tested with fakes.
- An `active` document must have a non-null `current_version` and there is no
  "pending" status (SPEC-03 §2 invariant 1, `document_status` enum). So a partial
  document row cannot be created at upload time without violating the invariant.

## Options
- **How the endpoints reach tenant data.** (a) a control-plane query with a
  `tenant_id` filter — rejected: documents are tenant content, so this violates
  C-3, and tenant tables carry no `tenant_id` (C-1); (b) resolve the tenant
  identity from the API key (as today) and open a `tenant.DB` via the resolver
  injected into the documents service (chosen — ADR-0003's only sanctioned path).
  `buildAPIServer` now constructs `tenant.NewResolver` from the control pool + the
  startup cipher, finally using the reserved cipher.
- **POST response / document-row creation.** (a) create an `active` document row
  with a null `current_version` at upload time — rejected: violates SPEC-03 §2
  invariant 1; (b) add a "pending" status — rejected: a tenant-schema change
  owned by EPIC-05, out of scope; (c) enqueue the `ingest_document` job and return
  it (202); the worker/document-store (STORY-05.1, ADR-0008) creates the row and
  its first version in one transaction (chosen). The API's job is to accept the
  upload and enqueue ingestion; building the document is the pipeline's job.
- **Object storage.** (a) block the whole endpoint until EPIC-06 — rejected: the
  enqueue against the real `jobs` table is deliverable now; (b) a narrow injected
  `Storage` port (`Put`), nil until EPIC-06 (chosen): while nil, `Ingest` fails
  closed with the not_found seam envelope (mirroring STORY-04.1/04.3); when wired,
  the upload is stored and the real `ingest_document` job is enqueued. No fake
  storage ships in production code (the integrity rule); the enqueue path is
  exercised via a test `Storage` in unit + e2e.
- **Upload type/size validation.** Enforced at the API layer now (FR-SRC-02
  allowlist by extension; `http.MaxBytesReader` + the configurable
  `MaxUploadBytes` ceiling). The connector (EPIC-06) may add its own checks; the
  API gate fails closed regardless.
- **Idempotency-Key.** Stored in the `ingest_document` job payload and replayed
  when the same key recurs (mirrors the sources sync idempotency, ADR-0029) — no
  new store.
- **Chunks debug view.** Returns the current-version chunks (FR-ADM-03) but never
  the opaque `embedding` vector (large, useless to a human, and cheap to omit).

## Decision
Add `internal/documents` (tenant content, so **not** under `internal/cp`): a
`Store` interface with a `TenantStore` over `*tenant.DB` (list/get/chunks/soft
delete), a `Service` holding the `tenant.Resolver` (the only source of a handle),
a control-plane `JobEnqueuer` for the `ingest_document` job, and an optional
`Storage` seam; and `Handlers` speaking the SPEC-07 §1 envelope with the tenant
taken only from the resolved context (FR-ACC-03). The five routes are appended to
`New` (scopes per SPEC-07 §2: ingest for upload/delete, query for list/get, admin
for chunks) and to `liveRoutes()` so the OpenAPI doc, the drift guard and the
contract tests (ADR-0028) grow with them; `api/openapi.yaml` is regenerated by
`mise run openapi`. `buildAPIServer` wires the resolver and leaves `Storage` nil.

## Consequences
- List, get (with current-version metadata and `?content=true`), the chunks debug
  endpoint and the soft delete are live and e2e-tested against a real enrolled
  tenant database now — reached only through the resolver + `tenant.DB` (ADR-0003,
  C-3), tenant derived from the API key (FR-ACC-03).
- `POST /v1/documents` validates the upload (FR-SRC-02 type allowlist + size
  ceiling) and, when `Storage` is wired, stores the bytes and enqueues a real
  `ingest_document` job (control-plane `jobs` table). Until EPIC-06 wires
  `Storage`, it returns the not_found seam envelope; wiring it in
  `buildAPIServer` is then the only change here (NFR-MNT-01).
- No document row is created on upload: the row + first version are built by the
  ingest worker/document store (STORY-05.1, EPIC-09) in one transaction
  (ADR-0008), so a crash mid-ingest never leaves a partial `active` document
  visible (SPEC-03 §2 invariant 1). The 202 response carries the queued job as the
  client's tracking handle.
- The resolver is now part of the request path in `serve`; a tenant that is
  provisioning/deleting/unknown or schema-behind yields `tenant_unavailable`, and
  a suspended (read-only) tenant refuses the delete — both fail closed.
- No schema/migration change: `documents`/`document_versions`/`chunks` and the
  `ingest_document` job kind already existed, so the drift guard stays green. A
  new `MaxUploadBytes` config knob (env `MAX_UPLOAD_BYTES`, default 50 MB)
  realises FR-SRC-02's "configurable size".
