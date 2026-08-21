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

## 3. OpenAPI
Generated from Go types (`oapi-codegen` or `swag`) into `api/openapi.yaml`; served at `/v1/openapi.json`. Contract tests in CI validate responses against it.
