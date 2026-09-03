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
