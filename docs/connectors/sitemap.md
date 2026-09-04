# Sitemap connector (`sitemap`)

**Traces:** FR-SRC-06, FR-ING-02 · **Spec:** SPEC-04 §3/§3a · **Decisions:** ADR-0047 (shared core + `lastmod`), ADR-0044/0045/0046 (shared egress/extraction/conditional fetch) · **Credentials:** none · **Scheduled:** yes

The `sitemap` connector enumerates exactly the URLs listed in one or more sitemaps.
It drives the **same crawl core** as [`web_crawl`](web_crawl.md) — the same egress
guard, HTML→markdown extraction, canonical de-dup, conditional fetch, robots.txt,
per-host politeness and `crawl_pages` state — differing only in three ways: the
frontier comes from sitemap XML, **no on-page links are followed**, and
`<lastmod>` drives incremental skipping.

## Configuration reference

All fields go in the source's `config` object. Unknown keys are rejected. Only
`sitemap_urls` is required. There is **no** `max_depth`, `start_urls`, `allow`, or
`render_js` (no link following).

| Field | Type | Required | Default | Meaning |
|---|---|:--:|---|---|
| `sitemap_urls` | array of string (≥1) | yes | — | Sitemap or sitemap-index URLs; each must be an absolute `http(s)` URL. |
| `deny` | array of string | no | — | Substrings; any URL containing one is skipped. |
| `max_pages` | integer ≥0 | no | `1000` | Hard ceiling on pages fetched. |
| `delay_ms` | integer ≥0 | no | `0` | Per-host politeness delay between requests. |
| `concurrency` | integer ≥0 | no | `4` | Maximum concurrent fetches. |
| `include_selectors` | array of string | no | — | CSS selectors; when set, only their top-most matches are extracted. |
| `exclude_selectors` | array of string | no | — | CSS selectors whose subtrees are dropped before extraction. |

`max_depth` is pinned to `0` internally (the frontier is exactly the sitemap URLs).

## Behaviour

- **Sitemap parsing.** `<urlset>` and `<sitemapindex>` documents are parsed with the
  standard XML decoder. A `<sitemapindex>` is expanded recursively into its child
  sitemaps. Gzipped sitemaps (`.xml.gz`) are detected by their magic bytes and
  transparently inflated, regardless of the `Content-Type`.
- **Bounded recursion (ceilings).** To bound a hostile or huge sitemap tree, the
  connector caps nested index depth (**5** levels), total sitemap documents fetched
  (**200**), and total page URLs collected (**50 000**) per source. A single
  unreachable or malformed sitemap is logged and skipped; only cancellation aborts.
- **No link following.** Only the URLs the sitemaps list are fetched; on-page links
  are never enqueued.
- **`lastmod` incremental (two layers).** `<lastmod>` is parsed as W3C-datetime /
  ISO-8601 (down to date-only). On an **incremental** sync, if a URL's `lastmod` is
  **not newer** than the page's recorded last-fetched time, it is skipped with **no
  HTTP request at all** — cheaper than a conditional GET. When `lastmod` says
  maybe-changed (or is absent), the request is made and the shared conditional GET /
  content-hash still suppresses a re-emit if the bytes are unchanged. A full sync
  fetches everything.
- **Shared crawler behaviours.** Extraction to markdown (include/exclude selectors
  + semantic fallback), `<link rel="canonical">`→`ExternalID` de-dup, robots.txt,
  per-host `delay_ms`, the SSRF guard on every hop, the 20 MB size cap, the 30 s
  timeout and ≤10 redirects, and `crawl_pages` resumability all behave exactly as in
  [`web_crawl`](web_crawl.md).

### Deletion detection

Same reconciliation as the crawler (SPEC-04 §3/§2c): the `lastmod`/conditional skip
runs only on incremental syncs where deletion detection is a no-op, so an
unfetched-because-unchanged page is never soft-deleted; a periodic full
re-enumeration re-emits everything.

## Credentials

None. Do not send a `credentials` object.

## Test-connection

`POST /v1/sources/{id}/test` fetches **and parses** the first `sitemap_url` through
the SSRF-guarded egress client (reusing the gzip/size-cap fetch), within the 10 s
probe deadline. Actionable errors cover unreachable, non-2xx, non-XML, and empty
(no `<url>`/`<sitemap>` entries) sitemaps.

## Example

```jsonc
// POST /v1/sources
{
  "kind": "sitemap",
  "name": "Acme site (via sitemap)",
  "config": {
    "sitemap_urls": ["https://acme.com/sitemap_index.xml"],
    "deny": ["/tag/", "?replytocom="],
    "max_pages": 50000,
    "delay_ms": 500,
    "concurrency": 8,
    "include_selectors": ["main", "article"],
    "exclude_selectors": ["nav", "footer"]
  }
}
```
