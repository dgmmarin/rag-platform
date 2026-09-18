# ISSUE-0087: Typed request/response schemas in the OpenAPI for external integrators

**Type:** Chore (API docs) · **Status:** Done · **Priority:** Medium · **Traces:** SPEC-07 §3, ADR-0028, FR-RET-06/08/10

## Summary
The generated `api/openapi.yaml` (served at `GET /v1/openapi.json`) covered every endpoint with auth,
tags, parameters, error schemas and accurate prose, but the request and success bodies were described
only in prose. There were no `requestBody` schemas and the 2xx responses had no `content` schema, so an
external integrator could not generate a typed client for the endpoints they actually call.

## What was built
The code-derived generator (`internal/api/openapi.go`, ADR-0028) now emits typed schemas for the core
integration endpoints, so the shared spec stays code-derived and drift-guarded (no hand-maintained
parallel file):

- **Generator machinery:** `Operation.RequestBody`, a `route.reqSchema`/`okSchema` (and `reqBodyType`)
  pair, and component `$ref` wiring in `requestBodyFor`/`responsesFor`.
- **Component schemas** mirroring the Go DTOs exactly: `RetrieveRequest`/`RetrieveResponse`/`Chunk`,
  `QueryRequest`/`QueryResponse`/`Citation`/`Usage`/`HistoryTurn`, `FeedbackRequest`, and the shared
  `Filters` — beside the existing `ErrorEnvelope`.
- Wired onto `POST /v1/retrieve`, `POST /v1/query`, `POST /v1/feedback` (the endpoints partners
  integrate against). CRUD endpoints (sources/documents/jobs) keep their accurate prose for now; they
  can adopt typed schemas the same way when needed.
- `api/openapi.yaml` regenerated via `mise run openapi`.

## How to share it
- Hand a partner `api/openapi.yaml`, or point them at the live `GET /v1/openapi.json`.
- Auth is `Authorization: Bearer rk_<prefix>_<secret>` (documented as the `bearerAuth` scheme); the
  tenant is derived from the key.

## Tests
- `internal/api` unit (drift guard: regenerate with `mise run openapi`) and the e2e OpenAPI contract
  test both pass; lint clean.

## Follow-up
- Extend typed schemas to the sources/documents/jobs CRUD bodies if integrators need them.
- Consider a templated `servers` entry so a partner can set their deployment base URL.

## Related
ADR-0028 (code-derived OpenAPI), SPEC-07 §3, ISSUE-0001 (OpenAPI generation).
