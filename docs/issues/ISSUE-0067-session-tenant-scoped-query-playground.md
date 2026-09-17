# ISSUE-0067: Session tenant-scoped query playground + feedback (STORY-11.6)

**Type:** Feature · **Status:** Done · **Story:** STORY-11.6 · **Traces:** FR-RET-06, FR-RET-09, ADR-0075

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs
> (ADR-0075 session tenant-scoped API). The query/feedback backend is ISSUE-0033 / ISSUE-0035.

## Summary
STORY-11.6 gives the session admin UI a query playground: ask a grounded question over the current
tenant's content, read the answer with its citations and a grounded flag, and rate the answer thumbs
up/down (FR-RET-06, FR-RET-09). It reuses the `RequireTenantAccess` middleware and the EXISTING
`query.Handlers.Query` + `querylog.Handlers.Feedback` verbatim — the same reuse pattern STORY-11.2
established for sources (ISSUE-0059), so no answering or feedback logic is duplicated between the
Bearer `/v1/query`+`/v1/feedback` surface and this session surface. No new ADR: this applies ADR-0075
(auth).

## Scope
- **Task 1 — server mount** (`internal/api/router.go` + `router_test.go`): mount
  `POST /admin/tenants/{tenantId}/query` and `POST /admin/tenants/{tenantId}/feedback` behind
  `RequireSession -> RequireTenantAccess`. Both map to `PermQuery` (any role) — the same access the
  Bearer routes require via `query` scope — so both use `RequireTenantSourcesRead`. Both are
  session-cookie POSTs, so both carry CSRF. `{tenantId}` is the tenant path segment. No api_server
  wiring change — `Query`/`Feedback` Deps are already populated for the Bearer surface.
- **Task 2 — query client (web)**: `web/lib/query.ts` — `runQuery(tenantId, question, csrf)` (calls
  the non-streaming variant, `stream:false`, returning the JSON `Result`) and
  `sendFeedback(tenantId, queryId, rating, csrf)`.
- **Task 3 — playground UI (web)**: `web/app/admin/query` page (question textarea, ask, error state)
  + `AnswerPanel` (grounded badge, model, answer text, citation list, thumbs up/down feedback that
  disables once recorded).

## Out of scope (later work)
- **SSE streaming in the UI.** The query handler streams (`retrieval`/`delta`/`done` events) off the
  request's `stream` flag and the BFF proxies the body through unchanged, but the first cut calls the
  JSON path (identical `Result`). Reading the SSE frames is a later progressive enhancement.
- Conversation history / multi-turn, filters (source/date), and the query-log history view
  (`GET /v1/queries`) are not surfaced here.

## Tests / runnable checks
- **`internal/api/router_test.go`**: `TestTenantQueryRoutesChain` (both routes: session → read gate
  order, correct handler reached) + `TestTenantQueryCSRF` (both POSTs blocked without CSRF).
  `go test ./internal/api/`: **PASS**; `go build ./...`: **PASS**.
- **web (`web/`, vitest + Testing Library)**: `web/components/AnswerPanel.test.tsx` (answer + grounded
  badge + model + citations, ungrounded/no-citations, thumbs-up/down wiring, disabled-once-rated).
  `cd web && npx vitest run`: **PASS**; `npm run build`: clean, route `/admin/query` present.
