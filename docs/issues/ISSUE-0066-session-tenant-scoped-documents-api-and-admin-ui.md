# ISSUE-0066: Session tenant-scoped documents API + admin UI documents browser (STORY-11.4)

**Type:** Feature · **Status:** Done · **Story:** STORY-11.4 · **Traces:** FR-ADM-03, ADR-0075

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs
> (ADR-0075 session tenant-scoped API).

## Summary
STORY-11.4 gives the session admin UI a read-only documents browser: list a tenant's documents
(filter by status and free-text `q`), open one for its detail and current-version metadata, and read
its chunks (FR-ADM-03). It reuses the `RequireTenantAccess` middleware and the EXISTING
`documents.Handlers` verbatim — the same reuse pattern STORY-11.2 established for sources
(ISSUE-0059) and STORY-11.3 for jobs (ISSUE-0060), so no list/get/chunks logic is duplicated between
the Bearer `/v1/documents` surface and this session surface. No new ADR: this applies ADR-0075
(auth). The surface is read-only — upload and delete stay on the Bearer surface only.

## Scope
- **Task 1 — server mount** (`internal/api/router.go` + `router_test.go`): mount
  `GET /admin/tenants/{tenantId}/documents`, `GET /admin/tenants/{tenantId}/documents/{id}`,
  `GET /admin/tenants/{tenantId}/documents/{id}/chunks` behind `RequireSession -> RequireTenantAccess`.
  Every route is a read, so all three use `RequireTenantSourcesRead` (`PermQuery`, any role) and none
  carry CSRF. `{id}` is the document id; `{tenantId}` is the tenant path segment. No api_server wiring
  change — `DocumentList`/`DocumentGet`/`DocumentChunks` Deps are already populated for the Bearer
  surface.
- **Task 2 — documents list (web)**: `web/lib/documents.ts` client + `useDocuments()` hook;
  `web/app/admin/documents` page + `DocumentsTable` (title/uri/external-id, status, source, MIME type,
  last seen; row link to detail); filters by status and a submitted free-text search (`q`).
- **Task 3 — document detail + chunks (web)**: `web/app/admin/documents/[id]` detail page
  (`useDocument` + `useChunks`) with `DocumentDetail` (summary, current-version metadata, chunk list
  with position, heading path, token count and model; the embedding vector is never returned).

## Out of scope (later stories)
- Query playground / eval screens — STORY-11.6, STORY-12.4.
- Document upload/delete from the UI — the Bearer `/v1/documents` ingest/delete surface stays the
  mutation path; the admin browser is read-only.
- Chunk/document keyset pagination in the UI — the list and chunk views read the first page; cursor
  paging is a later refinement.

## Tests / runnable checks
- **`internal/api/router_test.go`**: `TestTenantDocumentsRoutesChain` (all 3 routes: session → read
  gate order, correct handler reached) + `TestTenantDocumentsNoCSRF` (read-only surface mounts no
  CSRF). `go test ./internal/api/`: **PASS**; `go build ./...`: **PASS**.
- **web (`web/`, vitest + Testing Library)**: `web/components/DocumentsTable.test.tsx` (columns,
  title→uri→external-id fallback, empty state, row link) and `web/components/DocumentDetail.test.tsx`
  (summary + version metadata, version block omitted when absent, chunk list, no-chunks message).
  `cd web && npx vitest run`: **PASS** (77/77, +8 new); `npm run build`: clean, routes
  `/admin/documents` + `/admin/documents/[id]` present.
- **live E2E (`web/e2e/admin-screens.spec.ts`, Playwright)**: the Documents screen loads over the
  real round-trip (browser → BFF → ragctl → tenant DB) and renders its table or empty state, never
  the error banner. Self-skips without `E2E_BASE_URL` (`mise run web-e2e`).
