# ISSUE-0029: Retrieve endpoint

**Type:** Feature · **Status:** Done · **Story:** STORY-08.2 · **Traces:** FR-RET-08, ADR-0052, SPEC-06 §2, SPEC-07 §2e

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-08.2 for traceability; the backlog story
> remains the authoritative work item. STORY-08.2 advances EPIC-08 (10/39).

## Summary
`POST /v1/retrieve` (SPEC-07 §2): a retrieval-only endpoint (FR-RET-08) that embeds
the incoming query string with the tenant's configured embedding provider — the same
`internal/ingest/embed` seam the corpus was embedded with, so query and documents
share an embedding space — then runs the STORY-08.1 hybrid query
(`retrieve.Retrieve`) and returns the ranked chunks with score + citation metadata
(id, document_id, source_id, content, uri, title, heading_path, metadata, score). No
generation: reranking (08.3), the grounding floor and the LLM answer path (08.5) are
downstream and consume the returned results. The tenant is resolved from the API key
(FR-ACC-03) and reached only through a `*tenant.DB` from the resolver (ADR-0003).

## Scope
- `internal/retrieve/service.go`: `Service.Search` (resolve tenant → load settings →
  build embedder → embed query → hybrid query), `Settings`/`SettingsSource`/
  `EmbedderFactory` seams, `parseSettings`, the top_k default+ceiling, the error
  sentinels (`ErrEmptyQuery`, `ErrEmbedding`, `ErrTenantUnavailable`), and the
  production `KeyedEmbedderFactory` (embed.New from settings + platform key).
- `internal/retrieve/handlers.go`: `POST /v1/retrieve` handler — body decode (query,
  top_k, filters), tenant-from-context (FR-ACC-03), response DTO, SPEC-07 §1 error
  envelope mapping.
- `internal/api`: `Deps.Retrieve` + route mount (query scope), `liveRoutes` row +
  `retrieval` tag; `api/openapi.yaml` regenerated (`mise run openapi`).
- `internal/config`: `EMBEDDING_API_KEY` / `EMBEDDING_BASE_URL`.
- `internal/cli/api_server.go`: wire `retrieve.Service` + handler.
- Tests: `internal/retrieve/service_test.go` + `handlers_test.go` (fake retriever +
  fake embedder), `internal/api/router_test.go` (route chain) + `openapi_test.go`
  (documented-route parity), `test/e2e/retrieve_endpoint_e2e_test.go` (golden path
  over the real router + tenant DB, embedder stubbed).
- Not in scope: reranking (08.3), the LLM query/answer path (08.4/08.5), streaming
  (08.6), history (08.7), query logging/feedback (08.8).

## Resolution
- **Reused the ingest embedder seam** for the query embedding (ADR-0052), gated
  fail-closed by `settings.providers_allowed` (SPEC-09 §2).
- **top_k**: unset → `settings.retrieval.final_k`; clamped to a fixed ceiling (100,
  ponytail) so a client cannot request an unbounded fused set.
- **Provider key**: a single platform `EMBEDDING_API_KEY` (ponytail: one key per
  deployment, justified by C-5 single-region-per-tenant; upgrade to a provider→key
  map for heterogeneous deployments). Never logged/returned (C-4).
- **Error mapping** never leaks provider internals: embed/factory failures →
  generic `500`; empty query → `400`; unavailable tenant → `503`.

## Known follow-ups
- The Cohere embedder hardcodes `input_type: "search_document"`; a query ideally uses
  `"search_query"`. Other providers are symmetric and unaffected. Deferred (a
  provider-package change, NFR-MNT-02) — see ADR-0052.

## Verification
- TDD: `internal/retrieve/service_test.go` + `handlers_test.go` were written first and
  watched **red** (undefined `Service`/`Settings`/`Handlers`), then **green** — they
  pin filter/top_k pass-through, the settings→embedder path, the top_k default +
  ceiling, empty-query validation, embed/factory-failure → `ErrEmbedding` (no leak),
  tenant-unavailable mapping, and the handler's envelope mapping + no-tenant 401.
- e2e (`test/e2e/retrieve_endpoint_e2e_test.go`, real stack, localhost:5432): a query
  embeds and retrieves seeded chunks over the real router — docA ranks first with
  score + citation metadata, top_k is respected, the source filter narrows results,
  and an empty query is a 400. The embedding provider is stubbed (external), as the
  ingest e2e stubs it.
- `mise run openapi` regenerated `api/openapi.yaml`; the drift guard and the OpenAPI
  contract e2e stay green. The isolation e2e suite (SPEC-01 §9), STORY-08.1 retrieval
  e2e, and the API router golden path all pass.
