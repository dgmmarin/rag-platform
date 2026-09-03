# ISSUE-0001: OpenAPI generation and contract tests

**Type:** Feature · **Status:** Done · **Story:** STORY-04.2 · **Traces:** SPEC-07 §3, ADR-0028

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-04.2 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Publish the platform's HTTP API as an OpenAPI 3.1 document generated from Go,
serve it at `/v1/openapi.json`, and add contract tests that CI runs against
recorded responses (SPEC-07 §3).

## Scope
- Build `api/openapi.yaml` from code (the STORY-04.1 router surface: operational
  endpoints, auth routes, `/admin/*`, `/v1/usage`, the spec endpoint itself, and
  the SPEC-07 §1 error envelope + conventions). Later routes (04.3–04.6) append
  to the same route table.
- Serve the JSON form at `/v1/openapi.json` (open, no auth).
- Contract + drift tests wired into `mise run test` / `mise run e2e`.

## Resolution
- `internal/api/openapi.go` builds the document from `liveRoutes()` and
  `ErrorCodes()`; `OpenAPIHandler()` serves it as JSON; the router mounts
  `GET /v1/openapi.json`.
- `ragctl openapi` (via `mise run openapi`) regenerates the checked-in
  `api/openapi.yaml`; a drift-guard unit test fails CI when it is stale.
- A jsonschema contract test (unit + `test/e2e/openapi_e2e_test.go` over the real
  control-plane Postgres) validates real error responses against the served
  `ErrorEnvelope` schema, with a negative control.
- Design decision and the divergence from SPEC-07 §3's suggested codegen tools
  are recorded in ADR-0028. No new schema/migration; no tenant content in the
  control plane (C-3).

## Verification
`mise run test`, `mise run lint` (touched packages clean), `mise run e2e`
(`TestOpenAPIContractGoldenPath`), and the coverage gate — see the STORY-04.2
delivery note in `docs/backlog/BACKLOG_STATUS.md`.
