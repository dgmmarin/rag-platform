# ADR-0050: Connector "test connection" — per-kind live probes, a shared ≤10 s deadline, and a shared secret-free error classifier

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-SRC-14, NFR-SEC-04, C-4, SPEC-04 §1 · **Decisions:** ADR-0040, ADR-0041, ADR-0044, ADR-0048

## Context

The connector framework (STORY-06.1, ADR-0040) defined `Connector.Test(ctx, cfg,
creds)` and wired it end to end: `POST /v1/sources/{id}/test` → `connector.SourcesValidator`
→ the sources service, which decrypts `credentials_enc` and passes the plaintext map in
(STORY-06.2, ADR-0041). Until now each connector's `Test` only validated the config
SHAPE (`web_crawl`/`sitemap`/`api`) or was a no-op (`upload`) — it never touched the
network.

STORY-07.8 (FR-SRC-14) makes `Test` actually verify a source is usable before it is
saved/scheduled: **reachability AND credentials, within 10 s, with actionable errors.**
This ADR records the design; the HTTP path, the credential decrypt/zero lifecycle, the
egress SSRF guard, the API auth builder and the sitemap parser all pre-exist and are
reused unchanged.

## Options and decisions

- **Probe the real external system per kind, reusing each connector's existing fetch
  path — do not fork a second HTTP path.** Each `Test` drives the same egress seam its
  `Sync` uses, so the SSRF guard, User-Agent, size caps and (for the API connector) the
  auth builder are identical between a test and a real sync — a test cannot pass on a
  path a sync would fail on.
  - **`upload`:** stays a trivial success. An upload source has no external system and
    no credentials (documents are pushed through `POST /v1/documents`); object-storage
    health is a platform-wide `/readyz` concern, not a per-source test — a transient
    storage blip must not make every upload source report itself broken, and one
    tenant's test must not probe shared infrastructure. Documented in code.
  - **`web_crawl`:** one `GET` of the first `start_url` through the SSRF-guarded `Doer`.
    A 2xx/3xx is success; a non-2xx is `"start URL returned <status>"`. robots.txt is
    deliberately NOT consulted for a test (a robots disallow is not an unreachable
    source), but the SSRF guard always applies. GET (not HEAD) is chosen: it is what
    the crawler actually does, so it is representative, and the body is never read, so
    it is cheap.
  - **`sitemap`:** fetch AND parse the first sitemap URL, reusing the STORY-07.5
    `fetchSitemap` (gzip + size cap) and the `encoding/xml` parser. This distinguishes
    the failure modes a tenant actually hits — unreachable, non-2xx, non-XML, empty
    (no `<url>`/`<sitemap>` entries) — each an actionable message.
  - **`api`:** build the authed client with the decrypted credentials (reusing
    `buildAuthedClient`, ADR-0048) and make ONE request to the first endpoint (or the
    base URL) — no pagination, no body processing. `401/403` → `"authentication
    failed: check credentials"`; `2xx` → success; any other status → an actionable,
    redacted message. Because `buildAuthedClient` for `oauth2_cc` fetches the token
    lazily on the first request, a bad `client_id`/`client_secret` surfaces here as a
    credential error (see the classifier below).

- **A single hard ≤10 s deadline, derived inside every `Test`.** Each network `Test`
  does `ctx, cancel := context.WithTimeout(ctx, egress.ProbeTimeout)` (`ProbeTimeout =
  10s`) so a hanging host can never wedge the `/test` request (FR-SRC-14 "within 10 s").
  This is a tighter, request-scoped bound than the connectors' 30 s `Sync` fetch
  timeout; the shorter one wins. The constant lives in `internal/egress` so all three
  network connectors share one definition, and each connector's Test-applies-the-deadline
  behaviour is pinned by a test that records the probe request's context deadline (no
  10 s wait in CI).

- **One shared, secret-free error classifier in `internal/egress` — not four copies.**
  `egress.ClassifyError(err, host) string` maps a transport-level failure to an
  actionable message: `egress.ErrBlocked` → "address not permitted (…blocked by the
  SSRF guard)"; `*net.DNSError` → "host not found: <host>"; a deadline/`net.Error`
  timeout → "connection timed out"; `ECONNREFUSED` → "connection refused"; anything
  else → a generic "could not connect". It lives in `internal/egress` because that is
  the package that already owns the SSRF sentinel and is imported by all three network
  connectors (no import cycle — `egress` imports no connector), and because the SSRF
  classification is the security-relevant case it should own. `web_crawl` and `api` call
  it directly on their `Do` error (always transport-level); `sitemap` reuses
  `fetchSitemap` — which collapses transport and HTTP-status errors into one return — so
  a tiny `webcrawl.transportMessage` helper decides whether the error is a recognised
  transport failure (classify it) or an application error (a non-2xx status, described
  by the connector). This is the "small shared helper" the story allowed; it stops the
  four Tests duplicating the mapping without over-abstracting.

- **Sanitisation: never echo the raw error or the URL's query string (C-4).** A failed
  fetch is almost always a `*url.Error` whose `Error()` includes the FULL request URL —
  and a URL can carry a secret in its query (a cursor, an api_key). So `ClassifyError`
  NEVER includes `err.Error()`; it names only `host`, which is non-secret config. The
  API connector's `Test` reuses the existing `redactURL` helper (ADR-0048) for its
  status message, and the oauth2 branch does not echo the `*oauth2.RetrieveError` (whose
  string carries the token-endpoint response body). Credentials are never logged and
  never returned; the sources service still zeroes the decrypted map after `Test`
  (STORY-06.2).

- **oauth2 credential failures are classified as credential errors, not connect
  errors.** When the `oauth2_cc` token endpoint rejects the client credentials, the
  first API request fails with an `*oauth2.RetrieveError`. `api.classifyAPIError` checks
  for it first: a `401/403` token response → "authentication failed: check credentials
  (the token endpoint rejected the client credentials)"; any other token rejection →
  "the token endpoint rejected the request"; everything else (including a token fetch
  that could not connect, or an SSRF block on the `token_url`) falls through to
  `egress.ClassifyError`. A dial-blocked token fetch is an `ErrBlocked`, not a
  `RetrieveError`, so it is correctly reported as an SSRF block.

## Consequences

- `Test` now proves a source is reachable and its credentials are accepted before it is
  saved/scheduled, with messages a tenant admin can act on. Adding a connector still
  needs no change outside its package (NFR-MNT-01); a new connector implements its own
  probe and reuses `egress.ClassifyError`/`egress.ProbeTimeout`.
- **No new dependency, no migration, no OpenAPI/schema change, no DB or object storage
  needed.** All tests are hermetic (httptest fixtures + the real egress guard for the
  SSRF-block cases). Coverage: `internal/egress` 87.8%, `webcrawl` 79.9%, `api` 80.5%,
  `connector`/`upload` unchanged — all ≥ 70 %.
- **Known boundary (flagged, out of STORY-07.8's scope).** The `/v1/sources/{id}/test`
  HTTP handler (`internal/cp/sources`) maps a non-sentinel service error to a generic
  500 ("could not test source"); the create path, by contrast, wraps a connector error
  in a client-facing `ValidationError`. So today the actionable message is delivered at
  the **connector boundary** (and asserted there) but is genericised by the HTTP
  envelope. Surfacing it through the `/test` response — e.g. wrapping the `Test` error
  in a `ValidationError`/dedicated envelope in `sources.Service.Test` — is a one-line
  change in the sources package, which STORY-07.8 leaves untouched by scope ("only the
  connectors' Test methods"). Recorded here so the follow-up is a deliberate, visible
  decision rather than a silent gap (per the source-of-truth hierarchy: FR-SRC-14 wants
  the admin to see an actionable error; the connector delivers it, the envelope should
  be updated next where that path is owned).
