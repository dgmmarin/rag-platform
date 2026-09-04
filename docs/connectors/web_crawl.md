# Web crawl connector (`web_crawl`)

**Traces:** FR-SRC-03/04/05, FR-ING-02 · **Spec:** SPEC-04 §2/§2a/§2b/§2c · **Decisions:** ADR-0043 (core), ADR-0044 (SSRF), ADR-0045 (extraction), ADR-0046 (conditional fetch) · **Credentials:** none · **Scheduled:** yes

The `web_crawl` connector performs a breadth-first crawl of a website, starting
from one or more seed URLs and staying within a configurable allowlist. Each
fetched HTML page is extracted to clean markdown and streamed into ingestion; each
distinct canonical URL becomes one document.

## Configuration reference

All fields go in the source's `config` object. Unknown keys are rejected. Only
`start_urls` is required.

| Field | Type | Required | Default | Meaning |
|---|---|:--:|---|---|
| `start_urls` | array of string (≥1) | yes | — | Seed URLs; each must be an absolute `http(s)` URL. |
| `allow` | array of string | no | — | URL **prefixes** the crawl may follow. With none set, the crawl stays on the seed hosts. |
| `deny` | array of string | no | — | **Substrings**; any URL containing one is skipped. Applied even inside the allowlist. |
| `max_depth` | integer ≥0 | no | `3` | Maximum link depth from the seeds. Exact (level-synchronous BFS). |
| `max_pages` | integer ≥0 | no | `1000` | Hard ceiling on pages fetched, whatever the concurrency. |
| `delay_ms` | integer ≥0 | no | `0` | Per-host politeness delay between requests. |
| `concurrency` | integer ≥0 | no | `4` | Maximum concurrent fetches. |
| `include_selectors` | array of string | no | — | CSS selectors; when set, only their top-most matches are kept for extraction. |
| `exclude_selectors` | array of string | no | — | CSS selectors whose matching subtrees are always dropped before extraction. |
| `render_js` | boolean | no | `false` | **Not supported** — see below. |

A value of `0` (or absent) for `max_depth`/`max_pages`/`concurrency` means "apply
the default"; a negative `delay_ms` is clamped to `0`.

> **`render_js` is deferred (v2).** JavaScript rendering via a headless-browser
> service is described in SPEC-04 §2 but **not built** in this release. Setting
> `render_js: true` is rejected by config validation with an actionable error.

> **Spec-vs-code note — defaults.** The illustrative example in SPEC-04 §2 shows
> explicit values (`max_depth: 5`, `max_pages: 5000`, `delay_ms: 500`,
> `concurrency: 8`). Those are illustrative, not the built-in defaults; the actual
> defaults are `3 / 1000 / 0 / 4` as tabled above. Set the fields explicitly if you
> want the SPEC example's behaviour.

> **Spec-vs-code note — tenant crawl cap.** The tenant setting
> `settings.limits.max_pages_per_crawl` (default 5000, SPEC-02) exists but is **not
> yet consulted** by the crawler; the effective page cap is the per-source
> `max_pages` (default 1000). Set `max_pages` per source to control crawl size.

## Behaviour

- **Breadth-first with exact limits.** Depth-*d* URLs are fetched concurrently
  (bounded by `concurrency`), their links enqueued at *d+1*, then the crawl
  advances — so `max_depth` is exact and `max_pages` admits at most N fetches.
- **robots.txt** is fetched once per host and honoured (fail-open on error/non-2xx);
  the `User-Agent` identifies the platform and a contact URL.
- **URL normalisation & canonical de-dup.** URLs are normalised (lowercase
  scheme/host, drop default port and fragment, strip `utm_*`/known trackers, sort
  query) and de-duplicated. `<link rel="canonical">` becomes the document's stable
  `ExternalID`, and URLs sharing a canonical emit a single document.
- **Extraction to markdown (SPEC-04 §2b).** For HTML, `exclude_selectors` subtrees
  are dropped; if `include_selectors` match, only those are kept; otherwise the
  platform's semantic content extractor picks the `<main>`/`<article>`/`<body>`
  root and strips known chrome (nav, header/footer, cookie/consent/share/related/
  comment/newsletter blocks). The result is rendered to GFM markdown (headings,
  lists, tables, code, inline links). Title precedence is `<title>` → `og:title` →
  first `<h1>`.
- **Non-HTML passthrough.** A non-HTML response (PDF, etc.) that is within the
  allowlist is passed through as raw bytes to the parsing pipeline, not markdown.
- **Conditional fetch (SPEC-04 §2c).** ETag/Last-Modified are stored and replayed as
  `If-None-Match`/`If-Modified-Since`; a `304` skips parse/emit and only bumps the
  last-seen time. When a server sends no validators, a `sha256` of the body is
  compared to the stored hash and identical bytes are skipped. Incremental syncs
  re-visit fetched pages conditionally; a full sync keeps the resume-skip.
- **Resumability.** Crawl state persists to the tenant `crawl_pages` table, so an
  interrupted crawl resumes rather than restarting.
- **SSRF guard.** Every fetch (and every redirect hop) is checked against the egress
  allow rules — see [Shared behaviours](README.md#shared-behaviours). Per-response
  size cap 20 MB, 30 s timeout, ≤10 redirects.

### Deletion detection

Deletion detection runs on full re-enumerations (the sink's `Complete` reconciles
pages no longer seen). On incremental syncs, conditional-skip never soft-deletes an
unchanged page. Periodic full re-enumeration is the deletion-detection path in this
release (SPEC-04 §2c, ADR-0046).

## Credentials

None. `web_crawl` authenticates nothing; do not send a `credentials` object.

## Test-connection

`POST /v1/sources/{id}/test` performs one `GET` of the first `start_url` through the
SSRF-guarded egress client, within the 10 s probe deadline. A `2xx`/`3xx` is
success; a non-2xx returns "start URL returned &lt;status&gt;"; transport failures
map to the shared actionable messages. robots.txt is **not** consulted for a test (a
robots disallow is not an unreachable source), but the SSRF guard always applies.

## Example

```jsonc
// POST /v1/sources
{
  "kind": "web_crawl",
  "name": "Acme docs",
  "config": {
    "start_urls": ["https://docs.acme.com/"],
    "allow": ["https://docs.acme.com/", "https://acme.com/products/"],
    "deny": ["/search", "?page="],
    "max_depth": 4,
    "max_pages": 5000,
    "delay_ms": 500,
    "concurrency": 8,
    "include_selectors": ["main", "article"],
    "exclude_selectors": ["nav", "footer", ".cookie"]
  }
}
```
