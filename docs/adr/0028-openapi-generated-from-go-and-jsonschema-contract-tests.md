# ADR-0028: OpenAPI spec built from a Go route table (not a codegen toolchain), served as JSON, and contract-tested with a drift guard + jsonschema

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** SPEC-07 §3, FR-ACC-03, NFR-MNT-01, C-3

## Context
STORY-04.2 must deliver, per SPEC-07 §3: an `api/openapi.yaml` "generated from Go
types", served at `/v1/openapi.json`, with "contract tests in CI [that] validate
responses against it". SPEC-07 §3 names two candidate generators — `oapi-codegen`
or `swag`.

The live HTTP surface today (STORY-04.1) is small: the operational endpoints
(`/healthz`, `/readyz`, `/metrics`, and the spec itself), the open auth routes,
the platform-admin surface (`/admin/audit`, `/admin/impersonations[/{id}]`), and
one tenant-scoped route (`GET /v1/usage`). Everything else in the SPEC-07 §2
table is an intentionally-unregistered seam for stories 04.3–04.6. The one
response body every route can already produce is the SPEC-07 §1 error envelope,
whose code vocabulary is fixed in the `internal/api` constants.

Two candidate tools cut against that shape and against the platform's
dependency-minimalism (NFR-MNT keeps the surface small; the lazy-senior-dev rung
ladder in the house guidelines says do not add a dependency a already-present one
covers):

- `oapi-codegen` is **spec-first** (YAML → Go types) — the opposite direction of
  "generate the YAML from Go", and it would make a hand-authored YAML the source
  of truth rather than the code.
- `swag` is code-first but **annotation-driven**: it needs doc-comment
  annotations on every handler plus a `swag init` build step and a pinned CLI
  tool. That is a lot of machinery to describe a mostly-seam surface, and the
  annotations live away from the router that actually mounts the routes.

## Options
- **Spec source.** (a) `oapi-codegen` (spec-first, wrong direction); (b) `swag`
  (annotation-driven codegen, new toolchain + build step); (c) build the OpenAPI
  3.1 document as a Go value from the same route table the router mounts and the
  same error-code constants `WriteError` emits, marshalling it to YAML/JSON.
- **Keeping the checked-in YAML honest.** (a) trust developers to rerun a
  generator; (b) a drift-guard unit test (mirroring the STORY-01.5
  `schemas/*.sql` drift guard) that fails when `api/openapi.yaml` differs from
  what the code generates.
- **Contract test.** (a) pull in a full OpenAPI request/response validator
  (kin-openapi — another dependency); (b) extract the `ErrorEnvelope` component
  schema (OpenAPI 3.1 schemas *are* JSON Schema 2020-12) and validate recorded
  responses against it with `santhosh-tekuri/jsonschema/v6`, already a direct
  dependency (used by the settings validator).

## Decision
- **(c) Build the document from Go.** `internal/api/openapi.go` builds the
  OpenAPI 3.1 document from a single `liveRoutes()` table and the
  `ErrorCodes()` list (derived from the SPEC-07 §1 `Code*` constants). The
  `ErrorEnvelope` schema mirrors the `errorEnvelope` Go type exactly, and its
  `code` enum *is* `ErrorCodes()`, so the published codes cannot drift from the
  ones the router emits. This is genuinely code-derived, keeps the description
  next to the router, and adds **no** code-generation toolchain. The only new
  import is `gopkg.in/yaml.v3`, already in the module graph (promoted from
  transitive to direct), and reused `santhosh-tekuri/jsonschema/v6`.
- **Served open at `/v1/openapi.json`.** `OpenAPIHandler()` marshals the same
  in-code document to JSON and the router mounts it unauthenticated (a public API
  description drives client/SDK generation). The YAML artifact is written by
  `ragctl openapi` (invoked by `mise run openapi`), which needs no DB or config —
  it is pure over the Go route table — so it regenerates offline and keeps
  `mise.toml` minimal (one task file, no new tool pin). Routing the generator
  through `ragctl` (a Kong subcommand) rather than a separate `main` keeps the
  single-entrypoint property of ADR-0009.
- **(b)+(b) Drift guard + jsonschema contract test.** A unit test asserts the
  checked-in `api/openapi.yaml` equals `MarshalOpenAPIYAML()`; a stale file fails
  CI with "regenerate with `mise run openapi`". A second unit test and an e2e
  golden path drive **real** error responses from the assembled router (a 401
  from the scope gate, a 404 from the unknown-route fallback) and validate them
  against the `ErrorEnvelope` schema extracted from the served spec, with a
  negative control (a bare-string `{"error":"…"}` body) proving the schema has
  teeth. The e2e runs over a real listener against the real control-plane
  Postgres (`test/e2e/openapi_e2e_test.go`), so CI validates recorded responses
  against exactly what the API publishes.

## Consequences
- **Divergence from SPEC-07 §3's literal tool suggestion, recorded here.**
  SPEC-07 §3 says "`oapi-codegen` or `swag`"; we build the document from Go
  directly instead. The section's *intent* — a code-derived `api/openapi.yaml`,
  served at `/v1/openapi.json`, with CI contract tests validating responses — is
  fully met, and SPEC-07 §3 has been updated to describe the realised approach
  (higher layer not contradicted, only its illustrative tool list narrowed to
  what was chosen). If the surface grows enough that hand-modelling operation
  bodies becomes the bottleneck, `swag` remains a drop-in later: the route table
  is the seam to feed it.
- **Growable seam.** Stories 04.3–04.6 add their routes to `liveRoutes()` (and,
  where they return bodies, a component schema); the JSON handler, the YAML
  artifact, the drift guard, and the contract test all extend for free. NFR-MNT-01
  is preserved — a new route touches the api package and its registration, not
  unrelated code.
- **No new schema/migration and no tenant data** (C-3): the generator and the
  endpoint read only the in-code document. `internal/api` remains outside the
  `.ci/coverage-packages.txt` gate (consistent with ADR-0027), so no coverage
  threshold is enforced on it; the new code is nonetheless covered by unit tests
  and an e2e golden path.
- **One dependency promoted, none added net-new for codegen:** `gopkg.in/yaml.v3`
  moves from indirect to direct; `santhosh-tekuri/jsonschema/v6` is reused. No
  `oapi-codegen`/`swag` binary, no `go:generate` main, no mise tool pin.
