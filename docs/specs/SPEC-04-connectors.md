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

### 1a. Realised framework (STORY-06.1)

`internal/connector` implements the interface above, the registry, and config
validation (FR-SRC-13, NFR-MNT-01). The concrete connectors (upload STORY-06.3,
web_crawl/sitemap/api EPIC-07) are separate stories; nothing in this package
reaches a DB, object storage or the network.

- **Registry.** `Registry` maps a `Kind` to a `func() Connector` factory (a fresh
  instance per use, since a connector may hold per-sync state). `Register` panics
  on a nil factory or duplicate kind (init-time misconfiguration fails loudly).
  `Lookup` returns a fresh connector or `ok=false`; `Kinds` lists registered kinds.
  A process-wide `DefaultRegistry()` backs the package-level `Register`/`Lookup`.
- **Config validation.** `SchemaValidator` compiles a JSON Schema once and
  `Validate(cfg)` returns a `*ConfigError` (a sorted `[]FieldError` list, no
  secrets — config is non-credential data), mirroring tenant-settings validation
  (STORY-03.5). A connector embeds its schema and calls it from `ValidateConfig`,
  so validation is declarative and identical across connectors (SPEC-04 §7 step 2).
- **Control-plane seam.** `SourcesValidator` adapts a `Registry` to the sources
  API's `Validator` port (STORY-04.3) *structurally* — the sources package keeps
  no connector import; the dependency is injected at the composition root
  (`internal/cli`). `ValidateConfig(kind, cfg)` delegates to the registered
  connector, or returns nil for an unregistered kind (kind-specific validation is
  deferred until that connector exists; the sources package's generic validation
  still applies). `Test(ctx, kind, cfg)` runs the connector's `Test` (with no
  credentials yet — STORY-06.2), or returns an injected "unavailable" sentinel
  (`sources.ErrConnectorUnavailable`) for an unregistered kind, so `/test` reports
  the not_found seam until the connector lands. Wiring a new connector is its
  package + one `Register` call — no change to the sources package or the router
  (NFR-MNT-01).
- **Provisional.** `StateStore` (per-source key/value, §1) is realised as a
  minimal `Get`/`Set`; `Stats` carries the connector-reported enumeration counters.
  Both are exercised only by `Sync`, which is EPIC-07, so their concrete shapes are
  finalised when the first connector implements `Sync`. `SyncRun.Limiter` is
  `*golang.org/x/time/rate.Limiter`.

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

### 2a. Realised crawl core (STORY-07.1)

`internal/connector/webcrawl` implements the FIRST real `Connector.Sync` (ADR-0043).
It registers `web_crawl` via `init()` (blank-imported at the composition root), so
the sources API runs its JSON-Schema `ValidateConfig` and config-level `Test`.

- **BFS + limits.** Level-synchronous breadth-first crawl: depth *d* URLs fetched
  concurrently (bounded by `concurrency` via `errgroup`), their links enqueued at
  *d+1*, barrier, advance — so `max_depth` is exact and the `max_pages` cap (an
  atomic pre-check) admits at most N fetches whatever the concurrency. Allow (prefix)
  and deny (substring) gate frontier expansion; with no `allow` configured the crawl
  stays on the seed hosts. A per-page fetch/parse failure is recorded and swallowed
  (the crawl continues); only context cancellation aborts.
- **Politeness.** robots.txt is fetched once per host and honoured (a small
  hand-rolled parser; allow-all on error/non-2xx); the `User-Agent` names the platform
  and a contact URL. Per-host delay is enforced by a `hostGate` (mutex held across the
  `delay_ms` sleep) in addition to `SyncRun.Limiter` when the worker supplies one.
- **Normalisation + canonical.** URLs are normalised (lowercase scheme/host, drop
  default port + fragment, strip `utm_*`/known trackers, sort query) and de-duplicated;
  `<link rel=canonical>` becomes the Document `ExternalID`, and URLs sharing a
  canonical emit one Document.
- **State + resume.** Crawl state persists to `crawl_pages` through a `PageStore`
  reached as a `CrawlState` capability of `SyncRun.State` (`NewTenantPageStore`, a
  tenant.DB adapter — the only path to tenant data, ADR-0003; no `tenant_id`, no
  cross-DB FK). Discovered URLs are stored *pending* (`last_fetched_at` NULL) at their
  depth; fetched URLs record `last_fetched_at`, status, ETag, Last-Modified and
  content hash. A re-run skips already-fetched URLs and continues the pending
  frontier, so an interrupted crawl **resumes**.
- **Seams left for the rest of EPIC-07 (ADR-0043).** *Egress/SSRF (07.2):* every fetch
  goes through the injectable `Doer`; the default permits loopback for tests, and 07.2
  drops the SSRF-guarded transport in with no crawl-logic change. *Extraction (07.3):*
  pages are emitted as raw `Body`; only title/canonical/links are parsed here.
  *Conditional fetch (07.4):* ETag/Last-Modified/content-hash are persisted but no
  304/HEAD-skip is done yet.

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
Not scheduled. `POST /v1/documents` writes the file to object storage, attributes it to `source_id` = the tenant's implicit upload source, and enqueues an `ingest_document` job. Re-upload with same filename creates a new version.

### 5a. Realised handling (STORY-06.3)

The upload path is completed by three pieces (ADR-0042; ISSUE-0017):

- **Object storage.** `internal/objectstore` is an S3-compatible client
  (`aws-sdk-go-v2/service/s3`, reusing the vendored SDK core — no `minio-go`), backing
  the documents `Storage` seam (upload writes) and the ingest handler's read-back
  (`Get`). Locally the backend is MinIO; in production any S3-compatible store. The
  raw bytes are keyed `uploads/<tenant>/<uuid>-<filename>`.
- **Upload connector.** `internal/connector/upload` registers the `upload` kind so the
  sources API's config-validation / test-connection seams resolve it. Upload is **not
  scheduled** (documents are pushed one `ingest_document` job at a time), so
  `ValidateConfig` accepts any JSON object, `Test` is a no-op success, and `Sync`
  returns `ErrNotScheduled` — the scheduler never creates a `sync_source` job for an
  upload source.
- **`ingest_document` handler.** `internal/ingest/ingestdoc` is the directly-callable
  handler (the River worker that dispatches to it is EPIC-09 STORY-09.1). Given a
  queued job it fetches the bytes, loads tenant settings, builds the Embedder, and
  runs one INCREMENTAL `Sink.Put` (parse → chunk → embed → commit) — so ingesting one
  upload never soft-deletes the tenant's other documents. Identity is `(upload source,
  filename)`; a re-upload with new content produces a new immutable version and flips
  `current_version` (SPEC-05 §5, STORY-05.1).

**Where the document row is created (reconciliation).** The prose above says the
upload path "creates a document row," but an *active* document must have a non-null
`current_version` and there is no pending status (SPEC-03 §2 invariant 1, ADR-0008).
So the HTTP handler creates **no row** — it only writes bytes and enqueues the job
(ADR-0030). The row **and** its first version are built together, atomically, by the
`ingest_document` handler's `TenantStore.Put` (ADR-0008/0033), so a query on
`live_chunks` never sees a half-built or version-less document. The `202` response
carries the queued job as the client's handle. The invariant governs (ADR-0042).

**API-layer gates.** `POST /v1/documents` sniffs the file's leading bytes
(`http.DetectContentType`) and requires them to match the extension allowlist — the
client `Content-Type` is never trusted — and enforces the per-tenant ceiling
`settings.limits.max_upload_mb` (SPEC-02 §5), failing safe to the global
`MAX_UPLOAD_BYTES` (SPEC-07 §2b). The implicit upload source is an ordinary `sources`
row (kind `upload`, `(tenant_id, name)` unique), resolved by an idempotent upsert on
the control-plane pool — no schema change.

## 6. Credentials
`Credentials` is a decrypted `map[string]string` handed to the connector for the duration of a sync and zeroed afterwards. Never logged; `Test` errors are sanitised.

### 6a. Realised handling (STORY-06.2)

Source credentials (FR-SRC-10) are sealed on write and decrypted only for a
`Test`/`Sync`, using the same platform envelope Cipher the resolver/provisioner use
(AES-256-GCM DEK wrapped by KMS, SPEC-09 §2, C-4, ADR-0041). Nothing new in the
crypto scheme — the write path reuses `crypto.Cipher`.

- **On write (create/update).** The public API body carries `credentials` as a flat
  `{name: value}` object (a non-string or nested value fails to decode into
  `map[string]string`, so the shape is a 400 for free). The sources service marshals
  it, encrypts it, zeroes the plaintext JSON buffer, and moves the ciphertext into
  the row's `sources.credentials_enc` (`bytea`) — the store only ever sees ciphertext;
  the service nils the plaintext map before the store call. A missing Encrypter with
  credentials present fails closed (never a plaintext store). No column was added:
  `credentials_enc` already exists.
- **Never returned.** The public `Source` projection and the `sourceColumns` scan
  deliberately omit `credentials_enc`; a dedicated `Store.GetCredentials` is the only
  read of that column, used solely by the decrypt path. No response echoes credentials.
- **Decrypt-and-zero (Test/Sync seam).** `Test` reads `credentials_enc`, decrypts it
  into a `map[string]string`, zeroes the decrypted byte buffer immediately after
  unmarshalling, passes the map to the connector via the `Validator.Test(…, creds)`
  seam (the `connector.SourcesValidator` adapter forwards it as `connector.Credentials`),
  and clears the map the moment `Test` returns (`defer`). The same decrypt helper feeds
  the future sync worker's `SyncRun.Creds` (EPIC-07/09); `Sync` itself is not
  implemented here.
  - *ponytail:* Go strings (the map values) cannot be overwritten in place, so
    "zeroed" means the decrypted `[]byte` buffer is wiped and the map is cleared
    (values become GC-eligible). Wiping the string bytes would require a `[]byte`-valued
    credential type across the connector interface — the upgrade path.
- **Sanitised errors.** Crypto/decrypt errors carry neither the ciphertext nor a
  secret value; a connector `Test` failure is mapped to the generic public envelope
  (never the raw error), and credentials are never logged at any level.

## 7. Adding a connector (checklist)
1. New package under `internal/connector/<kind>` implementing the interface.
2. JSON schema for config; `ValidateConfig` uses it.
3. Register kind; add to `source_kind` enum via control-plane migration.
4. Integration test against a recorded fixture server.
5. Docs page under `docs/connectors/<kind>.md`.
