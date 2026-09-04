# Connectors

**Traces:** FR-SRC-01..14 · **Specs:** SPEC-04 (framework), SPEC-07 §2 (sources/documents API), SPEC-09 (security) · **Audience:** tenant administrators

A **connector** is the code that pulls (or receives) your content and feeds it into
the platform's ingestion pipeline. Each **source** you create names one connector
**kind** and carries the configuration and credentials that connector needs. This
directory documents every connector kind: what it does, its configuration
reference, the credentials it expects, and a copy-pasteable example.

These pages describe the platform **as built** (STORY-06.3 and STORY-07.1–07.8).
Where the config here differs from an illustrative example in SPEC-04, the page
documents the realised behaviour and flags the difference.

## Kinds

| Kind | Page | Scheduled? | Credentials | What it does |
|---|---|---|---|---|
| `upload` | [upload.md](upload.md) | No (push) | None | You push files in via `POST /v1/documents`. |
| `web_crawl` | [web_crawl.md](web_crawl.md) | Yes | None | Breadth-first crawl of a website within an allowlist. |
| `sitemap` | [sitemap.md](sitemap.md) | Yes | None | Enumerates the URLs listed in one or more sitemaps. |
| `api` | [api.md](api.md) | Yes | Per auth type | Fetches records from a JSON HTTP API. |

> `s3` is reserved in the connector kind enum but is **not implemented** in this
> release; only the four kinds above are usable.

## Source lifecycle

Every source moves through the same three steps (SPEC-07 §2, SPEC-04 §7):

1. **Create** — `POST /v1/sources` with `kind`, `name`, `config`, and (if the kind
   needs them) `credentials`. The config is validated against that connector's
   JSON Schema; unknown keys and type errors are rejected as a `400` with a field
   list. Credentials are encrypted immediately (see below).
2. **Test** — `POST /v1/sources/{id}/test` runs the connector's live
   *test-connection* probe (reachability **and** credentials), not just a config
   shape check. See [Test-connection](#test-connection) below.
3. **Sync** — the platform runs the connector on a schedule (crawl / sitemap / api)
   or per-upload (`upload`). Each enumerated item becomes a document version in
   your tenant database; retrieval only ever reads committed, current versions.

Update a source with `PATCH /v1/sources/{id}` (including new `credentials`);
`GET`/`DELETE` behave as expected. The tenant is always resolved from your
authenticated principal — there is never a `tenant_id` in a request (FR-ACC-03).

## Credentials

- Credentials are supplied as a flat JSON object on `POST`/`PATCH /v1/sources`,
  under the `credentials` key: `{"api_key": "..."}`. Values must be strings; a
  nested or non-string value is a `400`.
- They are **encrypted at rest** with the platform's envelope encryption
  (AES-256-GCM data key wrapped by KMS, SPEC-09 §2, C-4) into
  `sources.credentials_enc` before the row is written. The service never stores
  the plaintext.
- They are **never returned.** `GET /v1/sources` and every other response omit the
  credentials column entirely; there is no read-back API for a secret.
- They are decrypted only for the duration of a `test` or `sync`, handed to the
  connector, and cleared afterwards. Credentials are **never logged** at any level,
  and connector errors are sanitised so a secret cannot leak through a message.
- Each connector kind expects specific credential keys — see the kind's page. The
  `upload`, `web_crawl` and `sitemap` kinds require **no** credentials.

To rotate a credential, `PATCH` the source with a new `credentials` object.

## Shared behaviours

All three network connectors (`web_crawl`, `sitemap`, `api`) share the same egress
and safety machinery:

- **SSRF egress guard (NFR-SEC-04, SPEC-09 §4, ADR-0044).** Every outbound request
  resolves DNS and refuses to connect to unspecified, loopback (`127/8`, `::1`),
  private (`10/8`, `172.16/12`, `192.168/16`, IPv6 unique-local `fc00::/7`),
  link-local (`169.254/16` — including the `169.254.169.254` cloud-metadata
  address — and `fe80::/10`) addresses. IPv4-mapped IPv6 forms are normalised so a
  private address cannot be smuggled through. **The guard re-checks on every
  redirect hop**, so a redirect to a private address is blocked at connect time.
  For the API connector's `oauth2_cc`, the token endpoint dials through the same
  guard.
- **Per-response size cap.** Responses are read to a hard ceiling (**20 MB** for
  web/sitemap page bodies and API JSON responses); an oversize body is rejected.
- **Request timeout & redirect cap.** Each fetch has a **30 s** timeout and
  follows at most **10 redirects**.
- **Per-host politeness (crawl/sitemap).** robots.txt is fetched once per host and
  honoured; a configurable per-host `delay_ms` is enforced between requests; the
  `User-Agent` names the platform and a contact URL.
- **Conditional fetch (crawl/sitemap).** Unchanged pages cost a `304` (or a
  content-hash comparison) and are not re-parsed or re-emitted (SPEC-04 §2c).
- **Rate limiting & `Retry-After` (api).** Requests wait on the sync rate limiter;
  a `429`/`503` carrying `Retry-After` is honoured with a bounded retry (up to 5
  attempts, backoff capped at 60 s).

## Test-connection

`POST /v1/sources/{id}/test` verifies a source is actually usable (FR-SRC-14,
ADR-0050). Semantics:

- Each kind runs a kind-specific probe (see the kind's page). It first re-validates
  the config, then makes at most one live request.
- Every network probe applies a hard **10 s** deadline
  (`egress.ProbeTimeout`) so a hanging host cannot wedge the test.
- Errors are **actionable and sanitised**: they name only the non-secret host and
  map transport failures to messages like *host not found*, *address not permitted
  (SSRF guard)*, *connection refused*, *timed out*, or a status-based message. They
  never echo the raw error, the URL query string, or any credential.
- A failed live probe (unreachable host, bad credentials) is returned as a `400`
  validation error with the actionable message — it is a problem with your source
  config, not a platform fault.

## Adding a connector

Adding a new connector kind is a self-contained change: a new package under
`internal/connector/<kind>` implementing the `Connector` interface, its JSON Schema
for config validation, one `Register` call in the package `init`, the kind added to
the `source_kind` enum, an integration test against a fixture server, and a docs
page here (SPEC-04 §7). No change is needed outside the new package and its
registration (NFR-MNT-01).
