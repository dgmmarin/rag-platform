# ADR-0047: Sitemap connector — shared crawl core, stdlib sitemap parsing, and lastmod-before-conditional-GET incremental

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-SRC-06, SPEC-04 §3, C-1, C-3 · **Decisions:** ADR-0003, ADR-0040, ADR-0043, ADR-0044, ADR-0045, ADR-0046

## Context
STORY-07.5 (FR-SRC-06) adds the `sitemap` connector. SPEC-04 §3 defines it as "the
same as web crawl but frontier seeded from sitemap(s) (including sitemap index), no
link following, `lastmod` used for incremental sync." The web-crawl connector already
exists (STORY-07.1–07.4): a BFS crawl core with an SSRF-guarded egress `Doer`
(ADR-0044), HTML→markdown extraction (ADR-0045), conditional GET / 304 / content-hash
change detection (ADR-0046), canonical de-dup, robots/politeness, and a `crawl_pages`
`PageStore` over `tenant.DB` (ADR-0003).

The central requirement is **maximum reuse**: the sitemap connector must share the
fetch/extract/conditional/state machinery, not fork it. It differs from `web_crawl` in
exactly three ways — the frontier is seeded from sitemap XML, links are not followed,
and `lastmod` drives incremental skipping.

## Options and decisions

- **Parameterise the crawl core rather than duplicate it.** STORY-07.1's `run` engine
  is extended with the two seams the two connectors differ on, both behaviour-preserving
  for `web_crawl`:
  - a **frontier source** — a new `crawler.seeds []seed` field. When non-nil (the
    sitemap connector), `run` seeds the depth-0 frontier from it; when nil (web_crawl),
    it derives seeds from `cfg.StartURLs` exactly as before. A `seed` carries the
    normalised URL, the raw URL and an optional `lastmod`.
  - a **`followLinks` flag** — `run`'s BFS gates frontier expansion on it. `web_crawl`
    sets it true (default in `newCrawler`); the sitemap connector clears it, so a
    fetched page's links never enter the frontier. Combined with `max_depth=0`, the
    frontier is exactly the sitemap's URLs.
  Everything downstream — `process`/`emit`, the egress `Doer`, the 07.4 conditional
  GET/304/content-hash, the 07.3 extraction, canonical→`ExternalID`, robots, per-host
  delay, size/timeout caps, and the `crawl_pages` `PageStore` — is the **same code**,
  reached identically by both connectors. This is the lowest-code sharing: two struct
  fields and one branch in the seed loop, versus a second crawl engine.

- **Same package, not a sibling.** The sitemap connector lives IN
  `internal/connector/webcrawl` (`sitemap.go` + `sitemapconn.go`) and reuses the
  package's *unexported* core (`crawler`, `config`, `seed`, `parseAndNormalize`,
  `PageStore`) with **zero new exported surface**. A sibling package would have forced
  exporting the crawler internals purely to share them — more code and a wider API for
  no benefit. It registers `KindSitemap` from a package `init()`, so the composition
  root's existing blank import of `webcrawl` wires both kinds with no `internal/cli`
  change — still "a connector is its package + its registration" (NFR-MNT-01), the
  registration just rides an import that already exists.

- **Sitemap parsing on the stdlib — no new dependency.** `<urlset>` and
  `<sitemapindex>` are decoded with `encoding/xml`; `encoding/xml` matches child
  elements by local name irrespective of namespace, so the sitemaps.org `xmlns` needs
  no namespace-qualified tags. A `<sitemapindex>` is expanded **recursively** into its
  child sitemaps. Gzipped sitemaps (`.xml.gz`, extremely common) are handled with
  stdlib `compress/gzip`, detected by the gzip **magic bytes** (`0x1f 0x8b`) rather
  than trusting `Content-Type`/`Content-Encoding`, so a `.gz` served as
  `application/octet-stream` still inflates. No `robotstxt`/sitemap library is added
  (consistent with STORY-07.1's hand-rolled robots and normalisation).

- **Bounded recursion (a hostile-tree ceiling).** A sitemap index can nest and fan out
  arbitrarily. Collection is bounded by `maxSitemapDepth` (nesting), `maxSitemapDocs`
  (total sitemap files fetched) and `maxSitemapURLs` (total page URLs). Reaching a
  bound stops collection with a warning rather than erroring — partial results are
  still useful. `maxSitemapURLs` mirrors the sitemaps.org 50 000-URL per-file cap as a
  whole-tree budget. *ponytail:* fixed ceilings; the upgrade path is per-source config
  with the same fail-safe stop. Sitemap fetches reuse the crawler's egress `Doer`
  (SSRF guard) and the 20 MB response cap, and gzip inflation is likewise bounded to
  20 MB against a decompression bomb.

- **`lastmod` skips BEFORE the conditional GET — a cheaper first layer.** `<lastmod>`
  is parsed as W3C-datetime/ISO-8601 (down to date-only; unparseable ⇒ zero ⇒ "no
  signal", fetch anyway — the safe default). On an **incremental** sync, if a URL's
  sitemap `lastmod` is **not newer** than the page's recorded
  `crawl_pages.last_fetched_at`, the crawler skips it with **no HTTP request at all**.
  This is strictly cheaper than the 07.4 conditional GET (which still costs one round
  trip). The two layers compose: when `lastmod` says maybe-changed (or is absent) the
  request is made, and the 07.4 conditional GET / content-hash still suppress a
  re-emit if the page is truly unchanged. A full sync ignores `lastmod` and fetches
  everything. The comparison needs the prior fetch time, so `Page` gains a
  `LastFetchedAt` loaded from `crawl_pages.last_fetched_at` (the in-memory test store
  mirrors the tenant store's `now()` semantics so unit tests are faithful). This is a
  read-only addition — no schema change (the column already exists) and no new write.

- **Deletion detection — same reconciliation as the crawler (ADR-0046).** The
  `lastmod`/conditional skip runs only on incremental syncs, where the ingestion
  sink's `Complete` is a no-op, so a skipped page can never be soft-deleted. A skipped
  page's `crawl_pages` row is left untouched (its prior `last_fetched_at` stands — the
  page was not re-fetched). Deletion detection for sitemaps, as for crawls, is the
  periodic full re-enumeration.

## Consequences
- **No new dependency:** `encoding/xml` and `compress/gzip` are stdlib; the change is
  otherwise the existing crawl core plus two connector files.
- **No schema change, no migration, no OpenAPI change:** `crawl_pages` already carries
  `last_fetched_at` (now also read into `Page.LastFetchedAt`); the drift guard stays
  green. Sitemap config is validated by an embedded JSON Schema like every connector.
- **Behaviour-preserving for `web_crawl`:** `seeds` defaults nil and `followLinks`
  defaults true, so the crawler seeds from `start_urls` and follows links exactly as
  before; `item.lastmod` is zero for crawl items, so the `lastmod` skip never fires.
  All STORY-07.1–07.4 unit and e2e tests remain green.
- **Constraints upheld:** all `crawl_pages` I/O stays inside the `tenant.DB`
  `PageStore` (ADR-0003, C-3); no `tenant_id`, no cross-DB FK (C-1); sitemap fetches
  go through the SSRF-guarded egress (NFR-SEC-04); no secret and no document content
  is logged (identity/URL only, at debug/warn).
- **Adding the connector touched only its package + registration** (NFR-MNT-01/02):
  `internal/connector/webcrawl` (two new files, a two-field crawl-core extension) and
  a comment at the composition root; the sources API, router and ingestion pipeline
  are unchanged.
