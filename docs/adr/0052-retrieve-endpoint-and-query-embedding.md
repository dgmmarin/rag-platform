# ADR-0052: Retrieve endpoint — the `retrieve.Service` orchestration, query embedding through the shared ingest seam, top_k ceiling, and provider-key sourcing

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-RET-08, FR-ACC-03, SPEC-06 §1/§2, SPEC-07 §2, C-1, C-3, C-4, C-5 · **Decisions:** ADR-0003, ADR-0007, ADR-0028, ADR-0037, ADR-0051

## Context
STORY-08.2 puts the STORY-08.1 hybrid query (`retrieve.Retrieve`, ADR-0051) behind
`POST /v1/retrieve` (SPEC-07 §2): a retrieval-only endpoint that returns ranked
chunks without generation (FR-RET-08). `Retrieve` is embedding-provider agnostic —
it consumes a query *vector* plus the raw query text. This story supplies the
vector: it embeds the incoming query *string* with the tenant's configured
embedding provider (the same provider/model used at ingest — the query and the
corpus MUST share an embedding space, or vector similarity is meaningless), then
runs the hybrid query and returns the results. Reranking (08.3), the `min_score`
grounding floor and the LLM answer path (08.5) are downstream; `/v1/retrieve`
returns the raw fused results those layers consume.

Everything reaches tenant content through a `*tenant.DB` from the resolver
(ADR-0003, C-1, C-3); the tenant is derived from the authenticated API key
(FR-ACC-03), never a request parameter.

## Options and decisions

### Where the code lives and its shape
The endpoint's service + handlers live in `internal/retrieve` (`service.go`,
`handlers.go`), co-located with the query itself — the house pattern (`documents`,
`cp/sources` keep Service + Handlers with their domain). `Service.Search(ctx, tid,
Request)` resolves the tenant DB, loads settings, builds the embedder, embeds the
query and calls the package `Retrieve`. The pure `Retrieve` function stays untouched
and embedding-agnostic; `Service` is the orchestration layer above it. The retrieval
step is an injected seam (`Service.Retriever`, defaulting to `Retrieve`) so `Search`
is unit-testable with a fake retriever + fake embedder — no database — while the SQL
itself is covered by the e2e suite. Rejected: a separate `internal/retrieveapi`
package (the `documents` precedent co-locates), and putting orchestration in
`internal/api` (that layer wires, it does not own domain logic).

### Query embedding reuses the ingest embedder seam
The query is embedded through `internal/ingest/embed` — the *same* `Embedder` seam
and provider implementations the corpus was embedded with (ADR-0037), behind a
`retrieve.EmbedderFactory` mirroring `ingestdoc.EmbedderFactory`. `embed.New` fails
closed on the tenant's `settings.providers_allowed` before building anything
(SPEC-09 §2), so a query can never reach a provider the tenant did not permit.
Reusing the ingest path is what guarantees the query and documents share an
embedding space. **Known limitation (ponytail):** the Cohere provider hardcodes
`input_type: "search_document"`; a query ideally uses `"search_query"` (asymmetric
retrieval). The other providers (OpenAI/Voyage/TEI) are symmetric and unaffected.
Fixing Cohere's query mode is deferred — it is a provider-package change (NFR-MNT-02)
and out of this story's scope; noted in ISSUE-0029.

### top_k: default from settings, hard ceiling
An unset `top_k` falls back to the tenant's `settings.retrieval.final_k` (SPEC-02 §5,
the ADR-0007 default 8 when both are zero). Any `top_k` is clamped to a fixed ceiling
(`defaultMaxTopK = 100`) so a client cannot request an unbounded fused set that would
balloon fusion and the response body. ponytail: a fixed constant, not a per-tenant
setting; lift it into `settings.retrieval` only if a tenant ever needs a larger page.

### Provider API key sourcing
The production `KeyedEmbedderFactory` authenticates to the embedding provider with a
single platform key from config (`EMBEDDING_API_KEY`, optional `EMBEDDING_BASE_URL`
for a self-hosted/proxy endpoint such as TEI). ponytail: one key per deployment. C-5
deploys a tenant single-region, so a deployment serves one embedding provider and a
single key suffices; make this a provider→key map only when a deployment serves
tenants on heterogeneous providers. The key is confidential — never logged, never
returned (C-4). An empty key leaves the endpoint on a clean "could not embed" error
rather than reaching a provider.

### Error mapping never leaks provider internals
Service errors map to the SPEC-07 §1 envelope: an empty query is `400 validation`;
any embedding-provider or factory failure is `ErrEmbedding` → a generic `500 internal`
("could not embed the query") that never surfaces the provider endpoint, token or
upstream error body; an unavailable/unknown/schema-behind tenant is `503
tenant_unavailable` (mirroring `documents.Service.open`). A suspended tenant resolves
to a read-only handle on which retrieval (a read-only transaction, ADR-0051) still
works.

### OpenAPI
The route is added to the same code-derived route table (`internal/api/openapi.go`,
ADR-0028); `mise run openapi` regenerated `api/openapi.yaml` and the drift guard +
contract tests stay green. Consistent with every other route, the row documents the
response envelope by description rather than a full request/response JSON schema (the
OpenAPI model in this repo documents responses, not request bodies) — the request
shape is described in the operation summary.

## Consequences
- `/v1/retrieve` is a fully working endpoint (query scope): embed → hybrid query →
  ranked chunks with score + citation metadata, respecting filters and top_k.
- `internal/retrieve` now depends on `internal/ingest/embed` and the resolver; the
  pure `Retrieve` function is unchanged and still embedding-agnostic.
- A new config surface (`EMBEDDING_API_KEY` / `EMBEDDING_BASE_URL`) is the first
  platform-level embedding-provider secret; EPIC-09's ingest worker can adopt the
  same `KeyedEmbedderFactory` shape when it wires the ingest embedder.
- Cohere asymmetric query mode and the single-key→map upgrade are the two recorded
  future items.
