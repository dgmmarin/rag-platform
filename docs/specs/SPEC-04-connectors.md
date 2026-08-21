# SPEC-04: Connector framework

**Implements:** FR-SRC-01..14, NFR-MNT-01, NFR-SEC-04 · **Decisions:** ADR-0002, ADR-0006

## 1. Interface
```go
package connector

type Kind string // upload | web_crawl | sitemap | api | s3

type Document struct {
    ExternalID string            // stable per source
    Title      string
    URI        string            // for citations
    MimeType   string
    Body       io.ReadCloser     // raw bytes; nil if Text set
    Text       string            // already-normalised text (API connector)
    RawJSON    json.RawMessage   // original record, optional
    Metadata   map[string]any
    ModifiedAt *time.Time
}

type Sink interface {
    // Called once per document; returns whether it was new/changed.
    Put(ctx context.Context, doc Document) (changed bool, err error)
    // Called when the connector finishes a full enumeration; enables deletion detection.
    Complete(ctx context.Context) error
}

type Connector interface {
    Kind() Kind
    ValidateConfig(cfg json.RawMessage) error
    Test(ctx context.Context, cfg json.RawMessage, creds Credentials) error
    // Enumerate content and stream it into the sink. Must be cancellable.
    Sync(ctx context.Context, run SyncRun, sink Sink) (Stats, error)
}

type SyncRun struct {
    SourceID  uuid.UUID
    Config    json.RawMessage
    Creds     Credentials
    State     StateStore   // per-source key/value in tenant DB (cursor, etag cache)
    Full      bool         // true = full enumeration, deletion detection allowed
    Limiter   *rate.Limiter
    Log       *slog.Logger
}
```
Registration: `connector.Register(Kind, func() Connector)` in each package `init`; the worker resolves by `sources.kind`.

## 2. Web crawl connector
Config:
```json
{"start_urls":["https://docs.acme.com/"],
 "allow":["https://docs.acme.com/","https://acme.com/products/"],
 "deny":["/search","?page="],
 "max_depth":5,"max_pages":5000,"delay_ms":500,"concurrency":8,
 "include_selectors":["main","article"],"exclude_selectors":["nav","footer",".cookie"],
 "render_js":false}
```
Behaviour:
- robots.txt fetched per host and honoured; `User-Agent` identifies the platform and a contact URL.
- URL normalisation (lowercase host, strip fragments and tracking params, sort query).
- Conditional requests with ETag/Last-Modified from `crawl_pages`.
- HTML → markdown via readability-style extraction then `html-to-markdown`; title from `<title>`/`og:title`/`h1`.
- Extracts `<link rel=canonical>`; canonical URL becomes `ExternalID`.
- SSRF guard: resolve host, reject private/loopback/link-local ranges; re-check on redirects.
- Non-HTML responses (PDF etc.) within allowlist are passed as `Body` for the parsing pipeline.
- `render_js=true` (v2) routes through a headless-browser service.

## 3. Sitemap connector
Same as web crawl but frontier seeded from sitemap(s) (including sitemap index), no link following, `lastmod` used for incremental sync.

## 4. HTTP API connector
Config:
```json
{"base_url":"https://api.acme.com",
 "auth":{"type":"bearer"},              // api_key_header | bearer | basic | oauth2_cc
 "endpoints":[{
   "name":"products","path":"/v1/products","method":"GET",
   "pagination":{"type":"cursor","cursor_param":"cursor","cursor_path":"$.next_cursor"},
   "items_path":"$.data",
   "id_path":"$.id",
   "updated_path":"$.updated_at",
   "incremental_param":"updated_since",
   "template":"# {{.name}}\nSKU: {{.sku}}\nPrice: {{.price}} {{.currency}}\n\n{{.description}}",
   "uri_template":"https://acme.com/p/{{.slug}}",
   "metadata":{"category":"$.category.name"}
 }]}
```
- Pagination types: none, page, offset, cursor, link-header.
- Templates are Go `text/template` with helpers (`join`, `money`, `date`); rendered text is the document body.
- Incremental: stores last max `updated_at` in `State`; full sync weekly (configurable) for deletion detection.
- Rate limiting via `Limiter` and `Retry-After` handling.

## 5. Upload connector
Not scheduled. `POST /v1/documents` writes the file to object storage, creates a document row with `source_id` = the tenant's implicit upload source, and enqueues an `ingest_document` job. Re-upload with same filename creates a new version.

## 6. Credentials
`Credentials` is a decrypted `map[string]string` handed to the connector for the duration of a sync and zeroed afterwards. Never logged; `Test` errors are sanitised.

## 7. Adding a connector (checklist)
1. New package under `internal/connector/<kind>` implementing the interface.
2. JSON schema for config; `ValidateConfig` uses it.
3. Register kind; add to `source_kind` enum via control-plane migration.
4. Integration test against a recorded fixture server.
5. Docs page under `docs/connectors/<kind>.md`.
