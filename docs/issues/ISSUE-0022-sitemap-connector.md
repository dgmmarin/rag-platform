# ISSUE-0022: Sitemap connector

**Type:** Feature · **Status:** Done · **Story:** STORY-07.5 · **Traces:** FR-SRC-06, SPEC-04 §3/§3a, ADR-0047 (Decisions: ADR-0040, ADR-0043, ADR-0044, ADR-0045, ADR-0046)

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-07.5 for traceability; the backlog story
> remains the authoritative work item. STORY-07.5 advances EPIC-07 to 22/39.

## Summary
Add the `sitemap` connector (SPEC-04 §3, FR-SRC-06): discover pages from one or more
sitemap URLs, including `<sitemapindex>` files (recursively). It drives the SAME crawl
core as `web_crawl` — maximum reuse, no fork — differing only in the three ways
SPEC-04 §3 names: the frontier is seeded from sitemap XML, links are not followed, and
`<lastmod>` drives incremental skipping.

## Scope
- **Shared-core refactor (`internal/connector/webcrawl/crawl.go`, behaviour-preserving
  for `web_crawl`)** — two seams: a `crawler.seeds []seed` frontier source (nil ⇒ derive
  from `start_urls`, as today) and a `followLinks` flag gating BFS frontier expansion
  (default true). `item`/`seed` gain a `lastmod`; a `lastmod` incremental skip is added
  to `process` (fires only for `!Full` items carrying a `lastmod`, so `web_crawl` is
  untouched).
- **`internal/connector/webcrawl/sitemap.go` (new)** — `<urlset>`/`<sitemapindex>`
  parsing with stdlib `encoding/xml`, recursive index expansion, gzip (`.xml.gz`)
  inflation with stdlib `compress/gzip` (magic-byte detection), bounded recursion
  (`maxSitemapDepth`/`maxSitemapDocs`/`maxSitemapURLs`), `lastmod` parsing, and the
  `newSitemapCrawler`/`runSitemap` entry into the shared core (fetches reuse the egress
  `Doer` and size cap).
- **`internal/connector/webcrawl/sitemapconn.go` (new)** — the `sitemap` connector:
  JSON-Schema `ValidateConfig` (`sitemap_urls` required), `Test` (config-only, 07.8
  hardens live probing), `Sync`, and `init()` registration of `KindSitemap`.
- **`internal/connector/webcrawl/pagestore.go`** — `Page.LastFetchedAt` loaded from
  `crawl_pages.last_fetched_at` (the `lastmod` compare baseline); the in-memory store
  mirrors the tenant store's `now()` fetch-time semantics.
- **`internal/connector/webcrawl/webcrawl.go`** — shared `config` gains `sitemap_urls`;
  `withSitemapDefaults` pins `max_depth=0`.
- **`internal/cli/api_server.go`** — comment only: the existing `webcrawl` blank import
  now registers both `web_crawl` and `sitemap` (same package).

## Out of scope
HTTP API connector (07.6/07.7); all-kinds live "test connection" (07.8 — the sitemap
`Test` here is config-only); connector docs (07.9). No egress-guard or extraction
changes beyond making them shareable. No schema, no migration (the `crawl_pages`
columns already exist), no OpenAPI change. No new dependency.

## Acceptance / DoD evidence
- **TDD:** unit tests written RED first (undefined symbols), then GREEN —
  `TestSitemapIndexAndChildParsing` (index → two child sitemaps → pages; sitemap files
  themselves never emitted), `TestSitemapGzip` (gzipped `.xml.gz` inflated and parsed),
  `TestSitemapNoLinkFollowing` (an on-page link to `/trap` is never crawled),
  `TestSitemapLastmodIncrementalSkip` (older `lastmod` ⇒ no request/no emit; newer
  `lastmod` ⇒ fetched), plus config/registration tests.
- **e2e (`test/e2e/sitemap_e2e_test.go`, real Postgres via the resolver + `tenant.DB`):**
  a sitemap-index → child-sitemap → three pages full sync emits all three and records
  them in `crawl_pages`, follows no on-page link; an incremental re-sync whose sitemap
  `lastmod` is older than the real `last_fetched_at` re-fetches nothing and emits
  nothing. Ran GREEN against localhost:5432 (7.2 s). The web_crawl resume + conditional
  e2e still pass (behaviour-preserving refactor).
- **mise tasks:** `mise run test` green except the known `internal/cli` `.env`-injection
  caveat (passes with a clean env — verified `go test ./...` all green);
  `mise run lint` (golangci-lint via `go run`) — zero findings in the changed webcrawl
  files (`gofmt` clean, no `cap` builtin shadow); pre-existing findings elsewhere
  untouched. webcrawl coverage 79.3% (≥ 70 % connector gate).
- **Constraints:** all `crawl_pages` I/O via `tenant.DB` `PageStore` (ADR-0003, C-3);
  no `tenant_id`, no cross-DB FK (C-1); sitemap fetches through the SSRF-guarded egress
  (NFR-SEC-04); no secret/content logged. No schema/OpenAPI change; drift guard green.
