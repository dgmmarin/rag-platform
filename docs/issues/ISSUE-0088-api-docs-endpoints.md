# ISSUE-0088: Serve a browsable API reference and the spec as YAML

**Type:** Chore (API docs) · **Status:** Done · **Priority:** Low · **Traces:** SPEC-07 §3, ADR-0028, ISSUE-0087

## Summary
The OpenAPI spec was served only as JSON (`GET /v1/openapi.json`). Integrators also want a link they
can open in a browser, and the YAML form for tooling. This adds both, open (no auth) like the JSON.

## What was built
- `GET /docs` — a browsable API reference (Redoc) rendering `/v1/openapi.json` (`DocsHandler`). The Redoc
  bundle loads from a CDN; ponytail: keeps the binary free of an embedded JS bundle, upgrade path is to
  vendor it for an air-gapped deployment.
- `GET /v1/openapi.yaml` — the same document as YAML (`OpenAPIYAMLHandler`), the form most OpenAPI
  tooling prefers.
- Both mounted in `internal/api/router.go` and added to `liveRoutes()`, so they appear in the spec; the
  drift guard and contract/router tests pass and `api/openapi.yaml` was regenerated.

## How to share
Send a partner `https://<deployment>/docs` to browse, or `/v1/openapi.json` / `/v1/openapi.yaml` to
import into a client generator.

## Related
ISSUE-0087 (typed integration schemas), ADR-0028 (code-derived OpenAPI), SPEC-07 §3.
