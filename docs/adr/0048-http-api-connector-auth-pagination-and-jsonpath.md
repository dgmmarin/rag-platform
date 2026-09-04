# ADR-0048: HTTP API connector — auth via x/oauth2, a hand-rolled pagination engine, dot-path JSON extraction, and the 07.6/07.7 split

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-SRC-07, SPEC-04 §4, C-2, C-4, NFR-SEC-04, NFR-MNT-01 · **Decisions:** ADR-0003, ADR-0040, ADR-0041, ADR-0044

## Context
STORY-07.6 (FR-SRC-07) adds the generic **HTTP API** connector (`internal/connector/api`),
the third real `Connector.Sync` after the crawler (ADR-0043) and sitemap (ADR-0047).
SPEC-04 §4 defines a source that fetches a tenant-configured JSON API with one of four
authentication schemes, walks one of five pagination strategies, honours rate limits /
`Retry-After`, and maps each record to a document.

FR-SRC-07 is large, so it is split across two stories. **STORY-07.6 (this ADR)** builds
the fetch/auth/pagination/rate-limit **engine**. **STORY-07.7** builds the per-item
**mapping** (`text/template` rendering with helpers, `uri_template`, metadata JSONPath
extraction, `updated_path`) and **incremental sync** (`incremental_param`, cursor in
`SyncRun.State`, weekly full sync for deletion detection). This ADR records the engine
decisions and makes the seam between the two stories explicit.

## Options and decisions

- **Reuse `golang.org/x/oauth2/clientcredentials` for oauth2_cc — do NOT hand-roll token
  refresh.** The vendored `golang.org/x/oauth2` (already a direct require; the
  `clientcredentials` sub-package adds **no new dependency**, confirmed by an unchanged
  `go.mod`/`go.sum`) provides `clientcredentials.Config{ClientID,ClientSecret,TokenURL,
  Scopes}.Client(ctx)`, which returns an `*http.Client` whose `TokenSource` fetches,
  caches and refreshes the token automatically (`oauth2.ReuseTokenSource`, with a 10 s
  `expiryDelta` so a near-expiry token is refreshed proactively). Hand-rolling refresh
  would re-implement caching, expiry, and the token-endpoint request for no benefit
  (lazy-senior rung 4: an already-installed dependency solves it). Tests prove both the
  cached path (long-lived token ⇒ one token fetch across many pages) and the refresh
  path (short-lived token ⇒ the token endpoint is hit more than once).

- **The oauth2 client — and its token endpoint — dial through the SSRF guard.** The
  API connector fetches a tenant-supplied `base_url` AND a tenant-supplied `token_url`,
  so both must pass the egress guard (ADR-0044, NFR-SEC-04). We compose by threading the
  guarded `*http.Client` (`egress.GuardedClient`) into the oauth2 context via
  `context.WithValue(ctx, oauth2.HTTPClient, guarded)`. x/oauth2 uses that client for
  BOTH the token-endpoint request and (as the `oauth2.Transport` `Base`) every API call,
  so the guard covers the whole flow with no bespoke transport plumbing. A test with the
  real guarded client proves a `token_url` resolving to loopback is refused with
  `egress.ErrBlocked` before any token is issued. Header-based auth (api_key_header/
  bearer/basic) uses the same guarded base client directly.

- **Secrets come from `connector.Credentials`, never config (C-4, ADR-0041).** Config
  carries only the non-secret auth *shape* — which type, the api-key header name, the
  token endpoint, the scopes. The secret values (`api_key`, `token`, `username`/
  `password`, `client_id`/`client_secret`) arrive in the decrypted `Credentials` map the
  sources decrypt path (STORY-06.2) hands `Sync`. `buildAuthedClient` fails closed if a
  required key is missing, and its errors name only the missing *key*, never a value.
  URLs are query-redacted in error/log messages so a secret placed in a query param or a
  cursor never surfaces (SPEC-10: no content/secret logging).

- **A hand-rolled pagination engine, one strategy per SPEC type.** `none` (single
  request), `page` (increment a page number until an empty/short page), `offset` (advance
  offset/limit until a short/empty page), `cursor` (read the next cursor from
  `cursor_path` and pass it as `cursor_param` until absent), and `link-header` (follow
  RFC 5988 `Link: <…>; rel="next"`, resolving a relative next against the current URL).
  Every strategy is bounded by a `max_pages` ceiling (default 10 000, per-source
  overridable) — a **`ponytail:`** guard so a broken API that never signals the end
  cannot spin forever. The loop is a small state machine over a shared `getJSON`; a
  library would be more code than the five short loops the SPEC needs.

- **Rate limiting + `Retry-After`.** `getJSON` waits on `SyncRun.Limiter` (per-host
  politeness) before each request, and on `429`/`503` with a `Retry-After` header it
  honours the delay — parsing both wire forms (delta-seconds and HTTP-date) — with a
  bounded retry (5 attempts, delay capped at 60 s, another `ponytail:` ceiling) before
  failing. Proven by a fixture that 429s once with `Retry-After: 1` then serves 200.

- **JSON path extraction: a hand-rolled dot-path evaluator, no dependency.** The SPEC's
  paths are trivial — a leading `$`, dot-separated object keys and optional numeric array
  indices (`$.data`, `$.next_cursor`, `$.category.name`, `$.id`), plus a bare `$`/`""`
  root for a top-level array. A ~100-line evaluator over `encoding/json`
  (`evalPath`/`evalItems`/`evalString`, decoding with `UseNumber` so a numeric cursor/id
  keeps exact text) covers every path the SPEC uses, is trivially correct, and is
  reusable by 07.7's metadata extraction. A JSONPath library (e.g. `tidwall/gjson`) buys
  wildcards/filters/recursion the SPEC never uses, at the cost of a dependency — rejected
  on the reuse-first / lazy-correct rule (rung 5–6). This is the smaller **and** the
  edge-case-correct option for these inputs; ADR is the record if real-world APIs later
  force a richer path grammar (the upgrade path is to swap the evaluator behind the same
  three functions).

- **The 07.6/07.7 seam is `buildDocument`.** For 07.6 each enumerated item is emitted as
  a **placeholder** `connector.Document`: the raw item JSON as `Text`/`RawJSON`,
  `MimeType` `application/json`, `ExternalID` = `endpoint.name + "/" + id_path`. This
  makes pagination testable end to end (a recording sink asserts every item across every
  page is enumerated). `buildDocument` is the ONLY function 07.7 changes; the config
  schema already accepts the 07.7 fields (`template`, `uri_template`, `metadata`,
  `updated_path`, `incremental_param`) so a full source config validates today.

## Consequences
- Adding the connector is its package + one blank import at the composition root
  (`internal/cli/api_server.go`); the sources API resolves its JSON-Schema
  `ValidateConfig` and config-level `Test` with no other change (NFR-MNT-01). The
  `source_kind` enum already contains `api` — no migration.
- No new dependency, no OpenAPI change (internal connector config), no tenant-plane
  schema change (07.6 needs no `State`; the cursor-in-State is 07.7). C-1/C-3 are
  untouched — the connector reaches tenant data only through the sink the worker wires.
- Live reachability/credential probing in `Test` is deferred to STORY-07.8 (as for the
  other connectors); 07.6 `Test` is config-only.

## Alternatives rejected
- **Hand-rolled oauth2 token refresh** — more code, worse, re-implements a vendored,
  well-tested flow (rung 4).
- **A JSONPath library** — an unnecessary dependency for dot-paths (rung 5).
- **A generic paginator abstraction/interface** — the five loops are short and differ in
  their termination signal; an abstraction would be boilerplate nobody asked for.
