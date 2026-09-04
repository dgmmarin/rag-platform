# ISSUE-0023: HTTP API connector — auth and pagination

**Type:** Feature · **Status:** Done · **Story:** STORY-07.6 · **Traces:** FR-SRC-07, SPEC-04 §4/§4a, ADR-0048 (Decisions: ADR-0003, ADR-0040, ADR-0041, ADR-0044)

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-07.6 for traceability; the backlog story
> remains the authoritative work item. STORY-07.6 advances EPIC-07 to 30/39.

## Summary
Add the generic HTTP API connector (`internal/connector/api`, SPEC-04 §4, FR-SRC-07):
fetch a tenant-configured JSON API, authenticate each request, walk the endpoint's
pagination, honour rate limits / `Retry-After`, and stream each record into the
ingestion `Sink`. This is the engine half of FR-SRC-07; the per-item mapping
(`text/template`, `uri_template`, metadata JSONPath) and incremental sync
(`incremental_param` + cursor in `State`, weekly full sync) are STORY-07.7.

## Scope
- **`internal/connector/api/jsonpath.go`** — a no-dependency dot-path evaluator
  (`evalPath`/`evalItems`/`evalString`) over `encoding/json` for the SPEC's simple paths
  (`$.data`, `$.next_cursor`, `$.category.name`, bare `$` root); decoded with `UseNumber`
  so a numeric cursor/id keeps exact text. Reusable by 07.7's metadata extraction.
- **`internal/connector/api/auth.go`** — `buildAuthedClient`: the 4 auth types
  (api_key_header/bearer/basic + oauth2_cc reusing `golang.org/x/oauth2/clientcredentials`,
  no hand-rolled refresh). Secrets come from decrypted `connector.Credentials`, never
  config; fails closed on a missing secret; errors name only the missing key. The oauth2
  client and its token endpoint dial through the SSRF guard via `oauth2.HTTPClient` in ctx.
- **`internal/connector/api/paginate.go`** — the pagination engine (none/page/offset/
  cursor/link-header), each bounded by a `max_pages` ceiling (`ponytail:`); `getJSON`
  with `SyncRun.Limiter` politeness, `429`/`503` `Retry-After` (delta-seconds or
  HTTP-date) bounded retry, a 20 MB response cap, and query-redacted error/log URLs.
- **`internal/connector/api/egress.go`** — the SSRF-guarded base client
  (`egress.GuardedClient`, ADR-0044) as the fail-closed default, with
  `SetEgressClientForTest` for loopback httptest.
- **`internal/connector/api/api.go`** — the connector: config types, JSON-Schema
  `ValidateConfig` + semantic checks (base_url/token_url scheme, cursor needs
  cursor_path), config-only `Test`, `Sync`, `init()` registration of `KindAPI`, and the
  `buildDocument` seam (placeholder raw-JSON document now; 07.7 replaces it).
- **`internal/cli/api_server.go`** — one blank import of the new package registers `api`
  (NFR-MNT-01); no other change to the sources API or router.

## Out of scope
Per-item MAPPING and incremental sync (STORY-07.7); all-kinds live "test connection"
(07.8 — the API `Test` here is config-only); connector docs (07.9). No new dependency
(oauth2 client-credentials is a sub-package of the already-required `x/oauth2`). No
schema/migration (`source_kind` already has `api`; 07.6 needs no `State`). No OpenAPI
change (internal connector config).

## Acceptance / DoD evidence
- **TDD:** tests written RED first (undefined symbols) then GREEN. `jsonpath_test.go`
  (dot-path/array/root/number cases); `auth_test.go` (`TestAuthAppliedPerType` — all 4
  types authenticate against a 401-enforcing fixture; `TestAuthRejectsWrongSecret`,
  `TestAuthMissingCredential`, `TestOAuth2TokenEndpointSSRFGuarded` — loopback token_url
  blocked with `egress.ErrBlocked` through the real guard); `paginate_test.go`
  (`TestPaginationWalksAllPages` — each of the 5 types enumerates all 7 fixture items
  across 3 pages; `TestPaginationMaxPagesCeiling` — a never-ending cursor stops at
  max_pages; `TestRetryAfterParse` — both wire forms; `TestRetryAfterRetriesThenSucceeds`
  — 429+`Retry-After: 1` then 200, ~1 s elapsed); `api_test.go`
  (`TestSyncAuthPaginationMatrix` — all **4×5=20** auth×pagination combinations through
  the real `Sync`; `TestOAuth2TokenCachedThenRefreshed` — long-lived token fetched once,
  short-lived token refreshed >1×; `TestValidateConfig` — good + full-07.7-shape + 5 bad
  configs; `TestConnectorRegisteredAndKind`).
- **"fixture server tests for each combination" (AC):** the 4×5 matrix runs through
  httptest fixture servers (auth checker wrapping a pagination handler), all GREEN.
- **e2e / golden path:** the golden path is the connector-level `Sync` over real HTTP
  (httptest) with a recording sink asserting every item across every page is enumerated —
  hermetic (no DB, no object storage, no network), as the story needs no tenant `State`.
  Deviation from the usual `test/e2e/` build-tag suite is deliberate and per the story
  brief (07.6 touches no DB); the cursor-in-`State` DB path is 07.7.
- **mise tasks:** `mise run test` green except the known `internal/cli` `.env`-injection
  caveat — verified `internal/cli` passes with a clean env (`env -i … go test
  ./internal/cli/` → ok); `go vet ./...` clean; `gofmt` clean; api-package coverage
  **80.2%** (≥ 70 % connector gate). OpenAPI drift guard (`internal/api` openapi_test)
  green.
- **Constraints:** secrets only from decrypted `connector.Credentials`, never config,
  never logged (C-4, ADR-0041); the connector reaches tenant data only via the worker's
  sink (C-3); tenant-supplied base_url AND token_url both SSRF-guarded (NFR-SEC-04,
  ADR-0044); no `tenant_id`, no cross-DB FK (C-1); Go only (C-2). No schema/migration/
  OpenAPI change.
