# ISSUE-0026: Surface connector "test connection" errors through the HTTP /test path

**Type:** Bug · **Status:** Done · **Story:** STORY-07.8 (follow-up) · **Traces:** FR-SRC-14, SPEC-07 §1, ADR-0050 (Decisions: ADR-0040, ADR-0041)

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records the follow-up gap ADR-0050 flagged during
> STORY-07.8; it is a fix to 07.8's delivery, not a new story-point change.

## Summary
Close the boundary ADR-0050 flagged: connectors now return actionable "test connection"
errors (STORY-07.8), but `POST /v1/sources/{id}/test` mapped a failed `Connector.Test`
(a non-sentinel error) to a generic **500 "could not test source"**, so the tenant admin
never saw the actionable message. FR-SRC-14 ("returns actionable errors") was therefore
met only at the connector boundary, not through the API. This surfaces the connector's
already-sanitised message through the `/test` response.

## Decision
A well-formed request whose live connection/credential probe FAILED (unreachable host,
bad credentials) is about the admin's own source config — a client-facing 4xx, not a 500.
It is surfaced as **400 validation** by wrapping the connector's `Test` error in the
existing `*sources.ValidationError`, mirroring the CREATE path (which wraps a connector
`ValidateConfig` error the same way). Rationale: lowest code, reuses the existing SPEC-07
§1 envelope machinery and the `validation` code (already in the enum), and is consistent
with the create path. A dedicated **422** was considered (arguably more precise for
"well-formed but the remote failed") and rejected: it would add a new envelope code and
diverge from the create path for no admin-visible benefit — the actionable detail lives
in the envelope `message`, not the status. Recorded in ADR-0050's follow-up section.

## Scope
- **`internal/cp/sources/service.go`** — `Service.Test`: wrap a connector probe failure
  as `*ValidationError` carrying the connector's message; preserve `ErrConnectorUnavailable`
  (unregistered kind → 404 seam) and `ErrNotFound` unwrapped; a genuine internal fault
  (credential decrypt, store failure) still occurs before the probe and stays a 500.
- **`internal/cp/sources/handlers.go`** — no mapping change (the existing
  `*ValidationError → 400` in `writeServiceError` already handles it); comments updated.
- **`internal/api/openapi.go`** + regenerated **`api/openapi.yaml`** — add the `400`
  response to `sourceTest` (`mise run openapi`); drift/contract guards stay green.

## Out of scope
No change to the connectors' `Test` logic (STORY-07.8, done), the credential
decrypt/zero lifecycle (untouched), or any other endpoint. No migration, no new
dependency, no envelope-schema change (the `validation` code already exists).

## Acceptance / DoD evidence
- **TDD (RED first):** `service_test.go` `TestTestConnectionSurfacesConnectorFailure`
  (connector failure → `*ValidationError` with the exact message) and
  `handlers_test.go` `TestHandlerTestConnectionFailureIs400` (→ 400 `validation` +
  actionable message in the body) failed against the old 500 mapping, then passed.
- **Regression guards:** `TestTestConnectionPreservesUnavailableFromValidator` and the
  existing `TestHandlerTestConnectionSeam` (unwired/unregistered → 404 not_found seam),
  and `TestHandlerTestConnectionNotFound` (unknown source → 404), all green.
- **Sanitisation (C-4):** the message is the connector's `Test` error, already
  sanitised at the connector boundary (ADR-0050: never echoes the raw error or a URL
  query, names only the non-secret host); the service returns it verbatim and does not
  re-wrap the raw error. Credential decrypt/zero on the Test path is unchanged.
- **Checks:** `go test ./internal/cp/sources/... ./internal/api/...` green (full
  `go test ./...` green under a clean env); `go vet` clean; golangci-lint (v2.13.1) 0
  issues on the changed packages; OpenAPI regenerated and `TestYAMLDriftGuard` /
  contract test green.

## Traceability
Resolves the "Flagged boundary" recorded in ADR-0050 and ISSUE-0025 / the STORY-07.8
delivered notes.
