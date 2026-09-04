# HTTP API connector (`api`)

**Traces:** FR-SRC-07/08, FR-SRC-14, NFR-SEC-04 · **Spec:** SPEC-04 §4/§4a/§4b · **Decisions:** ADR-0048 (auth/pagination/JSONPath), ADR-0049 (templating/incremental), ADR-0044 (SSRF) · **Credentials:** per auth type · **Scheduled:** yes

The `api` connector fetches records from a tenant-configured JSON HTTP API. For each
configured endpoint it authenticates the request, walks the endpoint's pagination,
extracts each record, and maps it to a document via a template. It supports
incremental sync driven by an "updated-since" cursor.

## Configuration reference

The `config` object has three top-level keys, all required: `base_url`, `auth`, and
`endpoints`. Unknown keys are rejected.

| Field | Type | Required | Meaning |
|---|---|:--:|---|
| `base_url` | string | yes | Absolute `http(s)` base; endpoint `path`s are resolved against it. |
| `auth` | object | yes | Auth shape (non-secret). See [Authentication](#authentication). |
| `endpoints` | array (≥1) | yes | Collections to enumerate. See [Endpoints](#endpoints). |

## Authentication

The `auth` object carries only the **non-secret** shape (which type, header name,
token endpoint). Secret values are supplied separately in the source's
`credentials` object and are encrypted at rest and never returned or logged (see
[README](README.md#credentials)). A missing required secret fails closed with an
error naming only the missing key, never its value.

| `auth.type` | Extra `auth` config | Required credential keys | How it is applied |
|---|---|---|---|
| `api_key_header` | `header` (string, default `X-API-Key`) | `api_key` | Sets the configured header to the API key. |
| `bearer` | — | `token` | Sets `Authorization: Bearer <token>`. |
| `basic` | — | `username`, `password` | HTTP Basic auth. |
| `oauth2_cc` | `token_url` (string, required, absolute `http(s)`), `scopes` (array, optional) | `client_id`, `client_secret` | OAuth2 client-credentials; token fetched, cached and refreshed automatically. |

For `oauth2_cc`, both the token endpoint **and** every API call dial through the
SSRF guard (NFR-SEC-04). A bad `client_id`/`client_secret` surfaces on the first
request as an authentication failure.

## Endpoints

Each entry in `endpoints[]` describes one collection to enumerate. `name`, `path`,
`pagination`, and `items_path` are required.

| Field | Type | Required | Default | Meaning |
|---|---|:--:|---|---|
| `name` | string | yes | — | Unique name; also namespaces the document `ExternalID` and the incremental cursor. |
| `path` | string | yes | — | Path (or path+query) resolved against `base_url`. |
| `method` | string | no | `GET` | HTTP method. |
| `pagination` | object | yes | — | Pagination strategy. See below. |
| `items_path` | string | yes | — | Dot-path to the array of records in the response (e.g. `$.data`). |
| `id_path` | string | no | — | Dot-path to a stable record id; falls back to the record's sequence number if absent. |
| `updated_path` | string | no | — | Dot-path to the record's updated timestamp → `ModifiedAt` and the incremental cursor. |
| `incremental_param` | string | no | — | Query-param name for "updated since" on incremental runs. |
| `template` | string | no | — | `text/template` rendering the document body (markdown). |
| `uri_template` | string | no | — | `text/template` rendering the document's citation URI. |
| `metadata` | object `{key: dot-path}` | no | — | Extra metadata fields, each resolved by dot-path. |

With no `template`, the raw record JSON is used as the body
(`application/json`); with a `template`, the body is `text/markdown`.

### Pagination

`pagination.type` is required; every strategy is bounded by a `max_pages` ceiling
(default **10 000**) so a broken API that never signals the end still terminates.

| `type` | Params (with defaults) | Termination |
|---|---|---|
| `none` | — | One request. |
| `page` | `page_param` (`page`), `size_param` (`per_page`), `start_page` (`1`), `size` (page size; the size param is sent only when `size` > 0) | Empty page, or a page shorter than `size` when `size` is set. |
| `offset` | `offset_param` (`offset`), `limit_param` (`limit`), `size` (the limit; default `100`) | Empty or short page. |
| `cursor` | `cursor_param` (`cursor`), `cursor_path` (**required**) | `cursor_path` resolves to empty/absent. |
| `link-header` | — | No `rel="next"` in the RFC 5988 `Link` header (relative next URLs are resolved). |

### Templates and helpers

`template` and `uri_template` are Go `text/template`, **parsed once per endpoint**
and executed per record with the record's decoded JSON as the dot context (e.g.
`{{.name}}`, `{{.sku}}`). A **parse** error is a config error (rejected at validate/
test time); an **execution** error skips that one record and is recorded, never
aborting the sync. Error messages carry only the template location and Go types,
never record content.

Three helpers are available:

| Helper | Signature | Behaviour |
|---|---|---|
| `join` | `{{join .tags ", "}}` | Joins a decoded list (or `[]string`) with a separator; `nil`/absent → empty string; a non-list value renders as its `fmt` form. |
| `money` | `{{money .price .currency}}` or `{{money .price}}` | Formats a numeric amount (`json.Number`, number, or numeric string) to two decimals, with an optional currency suffix; a non-numeric value renders verbatim; an empty currency is omitted. |
| `date` | `{{date .updated_at "2006-01-02"}}` | Parses an RFC3339 / naive-datetime / date-only string or epoch seconds and reformats it with a Go layout (default RFC3339); an unparseable value renders verbatim. |

**Missing fields render `<no value>`** — the `text/template` default. It is
predictable and visible; a missing optional field never drops the whole document
(ADR-0049).

### Metadata and JSON paths

`items_path`, `id_path`, `updated_path`, `cursor_path`, and every `metadata` value
use a **minimal dot-path evaluator**, not full JSONPath (ADR-0048). Supported:

- `$` or empty — the root value.
- `$.a.b.c` — descend object keys.
- `$.items.0` — numeric array index.

**Not supported:** wildcards, filters, unions, recursive descent, or any other
JSONPath feature. Numbers are decoded exactly (a numeric cursor/id keeps its literal
text, never `1.2e+04`).

### Incremental sync

- On a **non-full** run, when an endpoint has an `incremental_param` and a stored
  cursor, the connector sends `<incremental_param>=<cursor>` on every page request so
  the API returns only newer records. A **first run** (no cursor) or a **full** run
  sends no incremental param and enumerates everything.
- As records stream, the connector tracks the maximum `updated_path` value and, after
  a successful enumeration, persists that **verbatim** source value as the cursor (in
  the tenant `connector_state` table), so the next run resumes in the API's own
  timestamp format.
- **Weekly full sync (the split).** This connector supports both modes and records
  `api:last_full_sync` after a full run, but it does **not** self-promote an
  incremental run to full. Deletion detection is driven by a full-mode sync whose
  cadence is set by the platform scheduler (a future release), mirroring the
  crawler's reconciliation (SPEC-04 §4b/§2c). In this release, incremental runs keep
  content current; a full run reconciles deletions.

## Shared behaviours

- **SSRF guard** on `base_url`, `token_url`, and every redirect hop (NFR-SEC-04).
- **Size cap** 20 MB per JSON response; **30 s** timeout; **≤10** redirects.
- **Rate limiting & `Retry-After`.** Each request waits on the sync rate limiter; a
  `429`/`503` with a `Retry-After` (delta-seconds or HTTP-date) is honoured with a
  bounded retry (up to 5 attempts, backoff capped at 60 s).
- Errors and logs **redact** the URL query string, so a secret in a query value or
  cursor is never surfaced.

## Test-connection

`POST /v1/sources/{id}/test` builds the authed client with the decrypted
credentials and makes **one** request to the first endpoint (or `base_url`), within
the 10 s probe deadline. A missing secret names only the missing key; `401`/`403`
(or an OAuth2 token rejection) → "authentication failed: check credentials"; `2xx` →
success; any other status → an actionable, redacted message. No pagination is
walked.

## Example

A complete source config with credentials for the `bearer` auth type:

```jsonc
// POST /v1/sources
{
  "kind": "api",
  "name": "Acme product catalogue",
  "config": {
    "base_url": "https://api.acme.com",
    "auth": { "type": "bearer" },
    "endpoints": [
      {
        "name": "products",
        "path": "/v1/products",
        "method": "GET",
        "pagination": {
          "type": "cursor",
          "cursor_param": "cursor",
          "cursor_path": "$.next_cursor"
        },
        "items_path": "$.data",
        "id_path": "$.id",
        "updated_path": "$.updated_at",
        "incremental_param": "updated_since",
        "template": "# {{.name}}\nSKU: {{.sku}}\nPrice: {{money .price .currency}}\n\n{{.description}}",
        "uri_template": "https://acme.com/p/{{.slug}}",
        "metadata": { "category": "$.category.name" }
      }
    ]
  },
  "credentials": {
    "token": "sk-live-…"
  }
}
```

Credential objects for the other auth types:

```jsonc
// api_key_header  (config: {"auth": {"type": "api_key_header", "header": "X-API-Key"}})
{ "api_key": "…" }

// basic
{ "username": "…", "password": "…" }

// oauth2_cc  (config: {"auth": {"type": "oauth2_cc", "token_url": "https://api.acme.com/oauth/token", "scopes": ["read"]}})
{ "client_id": "…", "client_secret": "…" }
```

> **Spec-vs-code note.** The SPEC-04 §4 example writes `"auth":{"type":"bearer"}`
> with the secret implied; the realised connector takes **all** secrets from the
> separate `credentials` object (never from `config`), with the exact key names
> above (`api_key` / `token` / `username`+`password` / `client_id`+`client_secret`).
