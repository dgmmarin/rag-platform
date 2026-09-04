# ISSUE-0018: Web crawler core

**Type:** Feature · **Status:** Done · **Story:** STORY-07.1 · **Traces:** FR-SRC-03, FR-SRC-04, SPEC-04 §2, ADR-0043

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-07.1 for traceability; the backlog story
> remains the authoritative work item. STORY-07.1 opens EPIC-07 (8/39).

## Summary
The first real `Connector.Sync`: a `web_crawl` connector (SPEC-04 §2, FR-SRC-03/04)
that crawls a website breadth-first within an allowlist, honours robots.txt and a
per-host delay, normalises/de-duplicates URLs, follows `<link rel=canonical>`,
bounds depth/pages/concurrency, streams each page into the ingestion `Sink`, and
persists crawl state to `crawl_pages` so an interrupted crawl resumes. SSRF
hardening (07.2), content-extraction quality (07.3) and conditional fetch (07.4)
are explicitly NOT built — each is left as a clean seam.

## Scope
- `internal/connector/webcrawl`: the connector (`Kind`/`ValidateConfig` via JSON
  schema + semantic checks/`Test`/`Sync`), registered via `init()` and blank-imported
  at the composition root (`internal/cli/api_server.go`).
  - `crawl.go`: level-synchronous BFS engine — depth/pages limits, allow(prefix)/
    deny(substring) gating, bounded concurrency (`errgroup`), atomic `max_pages` cap,
    per-host `hostGate` delay + `SyncRun.Limiter`, robots enforcement, canonical
    de-dup, per-page error tolerance.
  - `normalize.go`: hand-rolled URL normalisation (no `purell`).
  - `robots.go`: hand-rolled robots.txt group selection + longest-prefix Allow/Disallow.
  - `extract.go`: minimal HTML structure (title/canonical/links) — the extraction seam.
  - `egress.go`: the `Doer` egress seam + loopback-permitting default — the SSRF seam.
  - `pagestore.go`: `PageStore`/`CrawlState`, `memPageStore`, and `NewTenantPageStore`
    (a `*tenant.DB` adapter over `crawl_pages`, ADR-0003).
- Not in scope: SSRF/private-range blocking + redirect re-validation (07.2);
  readability/selectors/golden corpus (07.3); ETag/304/HEAD-skip optimisation (07.4);
  sitemap (07.5); API connector (07.6/07.7); cross-kind test-connection (07.8); the
  River worker that builds a real `SyncRun` and wires the ingestion sink (EPIC-09).

## Resolution
- **Reaching `crawl_pages` under ADR-0003.** The frozen STORY-06.1 interface gives a
  connector only the key/value `StateStore`; the crawler defines a `PageStore` and
  discovers it as a `CrawlState` capability of `SyncRun.State`. The worker backs
  `State` with `NewTenantPageStore` (a tenant.DB adapter); no interface change, no raw
  pool, no `tenant_id`/cross-DB FK (C-1/C-3, SPEC-03 §2 inv. 4). This finalises
  ADR-0040's provisional `StateStore` (ADR-0043).
- **Seams.** Egress via `Doer` (07.2), extraction emits raw `Body` (07.3),
  conditional-fetch state (etag/last-modified/hash) persisted but unused (07.4).
- **No new dependency.** Normalisation and robots are hand-rolled over the stdlib and
  the vendored `x/net/html`, `x/sync`, `x/time` (reuse-first; `purell` archived,
  `temoto/robotstxt` considered and declined — see ADR-0043).
- **No migration / no OpenAPI change.** `crawl_pages` already exists; the drift guard
  stays green and `api/openapi.yaml` is unchanged.

## Verification
- TDD throughout (tests watched red before implementation): `parseAndNormalize`
  (8 normalisation cases + equivalence + non-HTTP rejection); `parseRobots`/`allowed`
  (wildcard vs specific group, longest-match, empty=allow, case-insensitive agent);
  `extractHTML` (title/canonical + resolved/normalised/filtered links); the connector
  surface (kind/registration/schema validation/semantic render_js + non-HTTP rejection);
  and the BFS engine (`crawl_test.go`) against httptest servers — depth limit,
  `max_pages` cap actually stops the crawl, allow/deny gating, robots honoured,
  canonical→ExternalID de-dup, non-HTML passed as Body, per-host delay applied, and
  **resume** from a persisted mem store.
- `go test ./internal/connector/webcrawl/`: green; coverage 78.2% (≥70% gate).
- `mise run lint` (golangci-lint v2.13.1, toolchain go1.26): the new package and the
  one-line composition-root change are clean; the only reds are the pre-existing
  go1.26 toolchain-drift issues in untouched files (parse/connector.go/audit), as
  noted for STORY-06.3.
- `internal/cli` unit reds are the documented `.env`-injection caveat (pass with a
  clean env); nothing else in `go test ./...` regressed.
- e2e (`test/e2e/webcrawl_e2e_test.go`, `-tags e2e`) against the real stack: a real
  enrolled tenant DB, the crawler crawling an in-process httptest site, `crawl_pages`
  written through the tenant.DB `PageStore` — a capped run leaves 3 pending rows + 1
  fetched (with content_hash), and a second run over the SAME `crawl_pages` fetches the
  pending leaves WITHOUT refetching the root (resume proven by the site's hit counter).
  Assertions use the pgxpool / httptest server directly; cleanup drops the tenant DB
  over the control pool with FORCE — never `docker compose exec` (ISSUE-0014). PASS in 6.8 s.
