# SPEC-07: Public HTTP API

**Implements:** §5.1 of SRS, FR-ACC-03/04, NFR-SEC-07

## 1. Conventions
- Base path `/v1`; JSON; `snake_case`; UUIDs as strings; timestamps RFC 3339.
- Auth: `Authorization: Bearer <api key>` or session cookie. Tenant is derived from the principal (FR-ACC-03); there is no `tenant_id` parameter on tenant-scoped routes. Platform routes under `/admin` require `is_platform_admin`.
- Errors: `{"error":{"code":"not_found","message":"...","request_id":"..."}}`; codes: `unauthorized, forbidden, not_found, validation, rate_limited, tenant_unavailable, conflict, internal`.
- Pagination: `?limit=50&cursor=...` → `{"items":[...],"next_cursor":"..."}`.
- Idempotency: `Idempotency-Key` header honoured on `POST /v1/documents` and `POST /v1/sources/{id}/sync`.
- Rate limiting: token bucket per API key and per tenant (`settings.limits.qps`); `429` with `Retry-After`.
- Request ID: `X-Request-Id` echoed; generated if absent.

## 2. Endpoints
| Method | Path | Scope | Notes |
|---|---|---|---|
| POST | /v1/query | query | SPEC-06 |
| POST | /v1/retrieve | query | chunks only, no generation |
| POST | /v1/feedback | query | `{query_id, rating, comment}` |
| GET | /v1/sources | admin | |
| POST | /v1/sources | admin | body validated by connector |
| GET/PATCH/DELETE | /v1/sources/{id} | admin | delete enqueues `delete_source` |
| POST | /v1/sources/{id}/sync | admin | `{"full":true}`; 409 if one already active |
| POST | /v1/sources/{id}/test | admin | runs `Connector.Test` |
| POST | /v1/documents | ingest | multipart upload; returns document + job id |
| GET | /v1/documents | query | filter by source, status, q |
| GET | /v1/documents/{id} | query | includes current version metadata, optional `?content=true` |
| DELETE | /v1/documents/{id} | ingest | soft delete |
| GET | /v1/documents/{id}/chunks | admin | debugging |
| GET | /v1/jobs, /v1/jobs/{id} | admin | |
| POST | /v1/jobs/{id}/cancel | admin | queued or running |
| GET/POST/DELETE | /v1/api-keys | admin | secret returned once |
| GET/POST/DELETE | /v1/members | admin | |
| GET/PATCH | /v1/settings | admin | JSON-schema validated |
| GET | /v1/usage?from&to | admin | daily rows |
| POST | /admin/tenants | platform | enqueues provision |
| GET | /admin/tenants | platform | |
| PATCH | /admin/tenants/{id} | platform | status, db connection, settings |
| DELETE | /admin/tenants/{id} | platform | schedules deletion with grace |
| GET | /healthz, /readyz | — | |

### 2a. Sources (STORY-04.3 realisation)
The sources routes are served by `internal/cp/sources` over the control-plane
pool — sources are registry data and live in the control plane (C-3); this path
never opens a tenant database (ADR-0003). The tenant is taken only from the
authenticated API key (FR-ACC-03). Concurrent-sync `409` is enforced by the
existing `jobs_one_active_sync_per_source` partial unique index (a queued
`sync_source` mirror row is written for the EPIC-09 worker to consume); the
`Idempotency-Key` is stored in `jobs.payload` and replays the active sync job.
Two dependencies are injected seams provided by later epics: the connector
framework (`ValidateConfig`/`Test`, EPIC-06 STORY-06.1) — until it is wired,
`POST .../test` returns the `not_found` seam envelope and create/update run only
generic validation — and the job worker (EPIC-09 STORY-09.1/09.6) that executes
the queued jobs and performs the FR-SRC-12 cascade. Source credentials
(FR-SRC-10) are deferred to STORY-06.2: a `credentials` field is rejected `400`
(fail closed, C-4) and is never returned. See ADR-0029.

### 2b. Documents (STORY-04.4 realisation)
The documents routes are served by `internal/documents`. Unlike sources,
documents/versions/chunks are **tenant content** (`schemas/tenant.sql`, C-3), so
this path reaches a tenant database — and the only way to do that is a `tenant.DB`
from the resolver (ADR-0003). The tenant is taken only from the authenticated API
key (FR-ACC-03); the service opens the handle via the resolver, which
`buildAPIServer` now constructs (the reserved startup cipher decrypts each
tenant's DB password, SPEC-09 §2). Scopes follow the table above: `ingest` for
upload/delete, `query` for list/get, `admin` for the chunks debug endpoint.

`GET /v1/documents` (`?source&status&q&limit&cursor` → `{items,next_cursor}`
keyset pagination), `GET /v1/documents/{id}` (current-version metadata, optional
`?content=true` for the full normalised text), `DELETE /v1/documents/{id}` (soft
delete — status→`deleted`; `live_chunks` already excludes non-active documents),
and `GET /v1/documents/{id}/chunks` (current-version chunks for debugging; the
opaque embedding vector is never returned) are fully served against the tenant
schema.

`POST /v1/documents` validates the multipart upload at the API layer (FR-SRC-02:
type allowlist — PDF/DOCX/Markdown/HTML/TXT/CSV — and a configurable size ceiling,
default 50 MB via `MAX_UPLOAD_BYTES`), then two seams complete it: **object
storage** for the raw bytes is the EPIC-06 seam (a `Storage` port, nil today — so
the endpoint returns the not_found seam envelope until STORY-06.x wires it), and
the **ingest worker** (EPIC-09) that consumes the job. The `ingest_document`
enqueue itself is real (control-plane `jobs` table); the `Idempotency-Key` is
stored in `jobs.payload` and replays the active ingest job. No document row is
created on upload: an active document must have a non-null `current_version`
(SPEC-03 §2 invariant 1) and there is no pending status, so the row and its first
version are built together by the ingest worker/document store (STORY-05.1,
ADR-0008); the `202` response carries the queued job as the client's handle. See
ADR-0030.

### 2c. Jobs (STORY-04.5 realisation)
The jobs routes are served by `internal/cp/jobs` over the control-plane pool —
the `jobs` table is the history/mirror view of the queue (ADR-0005), a
control-plane table (C-3); this path never opens a tenant database (ADR-0003),
mirroring sources (§2a), not documents. The tenant is taken only from the
authenticated API key (FR-ACC-03). All three routes are the `admin` scope.

`GET /v1/jobs` (`?status&kind&source&limit&cursor` → `{items,next_cursor}` keyset
pagination on `(queued_at, id)`; the status/kind filters are validated against the
enums) and `GET /v1/jobs/{id}` return the job with status, `attempt`, `stats`,
timing, and a computed `duration_ms` for finished jobs (FR-ADM-02).

`POST /v1/jobs/{id}/cancel` implements SPEC-08 §4 against what exists today:
- a **queued** job is cancelled immediately — the mirror row is flipped
  `queued`→`cancelled` in one guarded statement (effective now; no worker holds a
  queued row), HTTP `200`;
- a **running** job's cancel is cooperative (the worker observes `ctx.Done()`
  between documents and exits `cancelled`, committing nothing partial). That is a
  River operation and River lands in EPIC-09, so it is an injected `Canceller`
  seam: nil today → the not_found seam envelope (mirroring §2a `/test` and §2b
  upload); once wired, HTTP `202` (cancellation requested) with the worker
  middleware writing the `running`→`cancelled` transition (SPEC-08 §3);
- a **terminal** job (succeeded/failed) → `409 conflict`; an already-cancelled
  job is an idempotent `200`.

No mirror column is added for the running cancel signal — it belongs in River, not
the mirror. See ADR-0031.

### 2d. Admin tenants (STORY-04.6 realisation)
The four `/admin/tenants` routes are served by `internal/cp/tenants`
(`AdminService`/`AdminHandlers`) over the control-plane pool. They are the
**platform** scope (§2): mounted under `/admin` behind
`RequireSession → RequirePlatformAdmin` with CSRF on the mutations — the same
guard the audit and impersonation routes use (STORY-04.1). Unlike the `/v1`
surface they are deliberately **not** tenant-scoped: a platform admin operates
across tenants, so the tenant is a route/body value here (FR-ACC-03 governs the
tenant-scoped `/v1` routes, not the platform surface). The tenant view returned
never carries connection details or secrets (C-4).

This is a thin HTTP layer over the tenant lifecycle that already exists
(STORY-02.3/02.4/02.5, ADR-0016/0017): each route routes to the existing service
that owns it, adding no duplicate audit (each of those already audits its own
action):

- `POST /admin/tenants` runs `provision.Provisioner.Provision` and records a
  `provision_tenant` mirror row, returning `{tenant, job_id}` (SPEC-07 §2). The
  async River `provision_tenant` job is EPIC-09 (ADR-0005); until then, exactly
  as `ragctl enroll` established (STORY-02.3, ADR-0016), provisioning runs
  synchronously and the tenant is active before the response returns — so the
  mirror row is a truthful `succeeded` record (the jobs table is the
  history/mirror view, ADR-0005), not a perpetually-queued placeholder.
- `GET /admin/tenants` (`?limit&cursor` → `{items,next_cursor}` keyset pagination
  on `(created_at, id)`) lists the registry.
- `PATCH /admin/tenants/{id}` routes each present sub-change: `settings` →
  `SettingsService.Patch` (FR-TEN-08, JSON-schema validated, embedding.dim
  immutable), `connection` → `Lifecycle.Move` (FR-TEN-07, password re-encrypted,
  C-4), `status` → `Lifecycle.Suspend`/`Resume` (FR-TEN-04). An unknown id is
  `404` before any write; an illegal status transition or an immutable-settings
  change is `409`.
- `DELETE /admin/tenants/{id}` (`?grace`, default 7 days) calls
  `Lifecycle.ScheduleDelete` (FR-TEN-05): status → `deleting`, `delete_after`
  recorded, `202`. The irreversible teardown after the grace window is the
  EPIC-09 River `delete_tenant` job (ADR-0005); scheduling is the complete, real
  action today.

The one genuine seam is the async River provision/delete **execution** (EPIC-09);
everything the four routes need exists. See ADR-0032.

## 3. OpenAPI
Generated from Go into `api/openapi.yaml`; served at `/v1/openapi.json`. Contract tests in CI validate responses against it.

The document is built in `internal/api/openapi.go` from the same route table the
router mounts and the same error-code constants `WriteError` emits — so the spec
is code-derived and cannot silently drift from the API. It is served as JSON at
`/v1/openapi.json` (open, no auth) and marshalled to the checked-in
`api/openapi.yaml` by `mise run openapi` (`ragctl openapi`). A drift-guard unit
test fails CI when the YAML is stale, and a contract test (unit + an e2e golden
path over the real stack) validates recorded error responses against the
`ErrorEnvelope` schema the spec itself publishes, using
`santhosh-tekuri/jsonschema/v6`.

This realises the section's intent without a code-generation toolchain
(`oapi-codegen` is spec-first — the wrong direction — and `swag` adds
annotation-driven codegen for a small, mostly-seam surface); ADR-0028 records the
decision and the divergence. Stories 04.3–04.6 add their routes to the same route
table, and the served JSON, the YAML artifact, the drift guard, and the contract
test all extend with them.
