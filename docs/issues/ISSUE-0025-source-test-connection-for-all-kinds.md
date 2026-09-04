# ISSUE-0025: Source "test connection" for all kinds

**Type:** Feature · **Status:** Done · **Story:** STORY-07.8 · **Traces:** FR-SRC-14, NFR-SEC-04, SPEC-04 §1/§1b, ADR-0050 (Decisions: ADR-0040, ADR-0041, ADR-0044, ADR-0048)

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-07.8 for traceability; the backlog story
> remains the authoritative work item. STORY-07.8 advances EPIC-07 to 37/39.

## Summary
Make each connector's `Test` actually probe the external system for **reachability AND
credentials, within 10 s, with actionable errors** (FR-SRC-14, SPEC-04 §1b) — replacing
the config-only stubs. The HTTP path (`POST /v1/sources/{id}/test` → `SourcesValidator`
→ the sources service credential decrypt, STORY-06.1/06.2) is unchanged; this fills in
the real per-kind probing behind it.

## Scope
- **`internal/egress/classify.go`** (new) — `ClassifyError(err, host) string`, the
  shared secret-free transport-error classifier (SSRF-block / DNS / timeout / refused /
  generic), and `ProbeTimeout = 10s`. Lives here (imported by all three network
  connectors, no cycle) so the four Tests do not duplicate the mapping.
- **`internal/connector/webcrawl/probe.go`** (new) — `probeReachable` (one GET through
  the egress `Doer`, 2xx/3xx ⇒ ok, non-2xx ⇒ "start URL returned <status>"),
  `transportMessage` (classify a `fetchSitemap` error as transport-vs-application), and
  `hostOf`.
- **`internal/connector/webcrawl/webcrawl.go`** — `web_crawl` `Test` probes the first
  `start_url` under a ≤10 s deadline.
- **`internal/connector/webcrawl/sitemapconn.go`** — `sitemap` `Test` fetches AND parses
  the first sitemap URL (reusing `fetchSitemap` + the `encoding/xml` parser); actionable
  errors for unreachable / non-2xx / non-XML / empty / SSRF-block.
- **`internal/connector/api/api.go`** — `api` `Test` builds the authed client
  (`buildAuthedClient`, incl. lazy oauth2 token fetch) and makes ONE request to the
  first endpoint / base URL; 401/403 ⇒ credential error, 2xx ⇒ ok, else actionable +
  redacted.
- **`internal/connector/api/probe.go`** (new) — `classifyAPIError` (oauth2
  `RetrieveError` 401/403 ⇒ credential error, else `egress.ClassifyError`) and `hostOf`.
- **`internal/connector/upload/upload.go`** — doc-only: records WHY upload `Test` stays
  a trivial success (no external system/credentials; storage health is `/readyz`).

## Out of scope
Connector docs (STORY-07.9). No change to auth/pagination/crawl/extract/incremental
logic. No change to the sources service/handler or the `/test` HTTP envelope (see the
flagged boundary below). No migration, no OpenAPI change, no new dependency.

## Acceptance / DoD evidence
- **TDD:** tests written RED first (undefined `egress.ClassifyError`/`ProbeTimeout`;
  Test returning nil where a probe error is expected), then GREEN.
  - `internal/egress/classify_test.go` — nil, SSRF-block (wrapped `ErrBlocked`), DNS
    not-found, context-deadline + `net.Error` timeout, refused, generic-sanitised (a
    secret in the raw error is NOT echoed), `ProbeTimeout == 10s`.
  - `internal/connector/webcrawl/probe_test.go` + `sitemap_probe_test.go` — reachable
    success, status error, SSRF-block (real guard, loopback URL), DNS failure, connection
    refused, deadline-applied (recording `Doer`); sitemap not-XML, empty, SSRF-block,
    deadline-applied.
  - `internal/connector/api/probe_test.go` — reachable success, 401 ⇒ credential error,
    oauth2 bad-secret ⇒ credential error (token endpoint 401 → `RetrieveError`), missing
    credential names the key, SSRF-block (real guard), non-2xx status, deadline-applied
    (recording `RoundTripper`).
- **≤10 s deadline:** each network `Test` derives `context.WithTimeout(ctx,
  egress.ProbeTimeout)`; asserted by recording the probe request's context deadline (no
  10 s wait in CI).
- **Sanitisation:** `ClassifyError` never echoes `err.Error()`/the URL query (only the
  non-secret host); API status message reuses `redactURL`; oauth2 `RetrieveError` body
  not echoed; credentials never logged/returned (STORY-06.2 zeroing unchanged).
- **Checks:** `go test ./...` (clean env) green; `internal/egress` cover 87.8%,
  `webcrawl` 79.9%, `api` 80.5%, `upload` 87.5%, `connector` 85.4% (≥ 70 % gate); gofmt
  clean; `go vet` clean; golangci-lint reports 0 issues in the changed/new files (5
  pre-existing findings live in untouched test files / `connector.go`).
- **Constraints:** tenant access unaffected (no DB in this story); no tenant content in
  the control plane; secrets sanitised and unlogged (C-4); Go only (C-2); SSRF guard
  enforced on every probe (NFR-SEC-04). No control-plane/OpenAPI change, no migration,
  no new dependency.

## Flagged boundary
The `/test` HTTP handler (`internal/cp/sources`) maps a non-sentinel service error to a
generic 500, so the connector's actionable message is delivered/tested at the connector
boundary but genericised by the HTTP envelope. Surfacing it through the `/test` response
is a one-line follow-up in the sources package, deliberately outside STORY-07.8's scope
("only the connectors' Test methods"). Recorded in ADR-0050 so the follow-up is visible.
