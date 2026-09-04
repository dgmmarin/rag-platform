# ADR-0043: Web-crawler core — BFS architecture, egress/extraction/conditional-fetch seams, and crawl-state persistence

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-SRC-03, FR-SRC-04, SPEC-04 §2, C-1, C-3 · **Decisions:** ADR-0003, ADR-0038, ADR-0040, ADR-0041, ADR-0042

## Context
STORY-07.1 is the FIRST real `Connector.Sync` implementation (ADR-0040 shipped the
interface, registry and config validation; STORY-06.3/ADR-0042 shipped the
not-scheduled upload connector). The web-crawl connector (SPEC-04 §2, FR-SRC-03/04)
must enumerate a website breadth-first within an allowlist, honour robots.txt and a
per-host delay, normalise/de-duplicate URLs, follow `<link rel=canonical>`, bound
depth/pages/concurrency, stream each page into the ingestion `Sink`, and persist
crawl state so an interrupted crawl **resumes**.

EPIC-07 is split across nine stories. STORY-07.1 is deliberately *only* the crawl
core; three adjacent concerns are explicitly out of scope and must be left as clean
seams the later stories drop into without touching crawl logic:

- **SSRF/egress hardening (STORY-07.2, SPEC-09 §4)** — block private/loopback/
  link-local/metadata ranges, re-validated on each redirect hop.
- **HTML content-extraction quality (STORY-07.3, FR-SRC-05)** — readability +
  include/exclude selectors + HTML→markdown + a golden corpus.
- **Conditional fetch / change detection (STORY-07.4, FR-ING-02)** — ETag/
  Last-Modified/304 HEAD-skip.

## Options and decisions

- **Where does the crawler live?** `internal/connector/webcrawl`, registered into
  the default registry from `init()` and blank-imported at the composition root
  (`internal/cli/api_server.go`), exactly like the upload connector — so wiring it is
  its package + one `Register` call, nothing else (NFR-MNT-01).

- **BFS engine.** (a) A fully concurrent frontier with a shared work queue —
  rejected: correct depth accounting, the `max_pages` cap and deterministic tests
  become fiddly. (b, chosen) **Level-synchronous BFS**: process all URLs at depth
  *d* concurrently (bounded by `concurrency` via `errgroup.SetLimit`), collect their
  outbound links into depth *d+1* under a mutex, barrier on `g.Wait()`, advance.
  Depth is exact by construction; the `max_pages` cap is an `atomic.AddInt32`
  pre-check that admits at most N fetches regardless of concurrency (a bad page is
  recorded and swallowed so the crawl continues, mirroring the sink's per-document
  error policy, ADR-0038). A per-page HTTP/parse failure never aborts the run; only
  context cancellation propagates.

- **Per-host politeness.** SPEC-04 §1 gives a connector a single `SyncRun.Limiter`
  (`*rate.Limiter`, the worker-supplied global limiter) but SPEC-04 §2 also wants a
  configurable per-host delay. We honour BOTH: the limiter (`Wait`) if present, and a
  per-host `hostGate` that holds a mutex across the politeness sleep and reserves the
  next slot `delay_ms` ahead. Holding the gate mutex serialises same-host fetches
  while letting other hosts proceed. The sleep is injectable for deterministic tests.

- **Reaching `crawl_pages` under ADR-0003 — the `CrawlState` seam.** The frozen
  STORY-06.1 interface hands a connector only the generic key/value
  `StateStore` on `SyncRun.State` (right for an API cursor, wrong for an indexed
  per-URL table), and ADR-0003 forbids a connector opening its own pool. Options:
  (a) serialise the whole frontier into one `StateStore` blob — rejected: defeats the
  purpose of the indexed `crawl_pages` table and its `(source_id, last_fetched_at)`
  index. (b) add a crawl-specific field to the generic `SyncRun` — rejected: pollutes
  every connector with crawl concerns. (c, chosen) an **optional-interface capability
  on `SyncRun.State`**: the crawler defines `PageStore` (`Load`/`Upsert`) and
  discovers it by type-asserting `run.State`. The worker (EPIC-09) backs `State` with
  a single tenant.DB object — shipped here as `NewTenantPageStore` returning
  `CrawlState` (`StateStore` + `PageStore`) — so the worker builds ONE state object
  (which it must anyway) and the crawler discovers the richer capability with no
  worker special-casing. Absent the capability the crawler falls back to an in-memory
  store and warns: correct, but non-resumable. All `crawl_pages` SQL is inside the
  tenant.DB adapter (ADR-0003, C-3); `source_id` is an informational copy of a
  control-plane id, no cross-DB FK, no `tenant_id` column (C-1, SPEC-03 §2 inv. 4).
  This finalises the "provisional pending Sync" note on `StateStore` in ADR-0040:
  `StateStore` stays a minimal key/value cursor; connector-specific tables ride on
  `State` as capabilities.

- **Resumability semantics.** On enqueue, a discovered URL is upserted as a *pending*
  `crawl_pages` row (`last_fetched_at` NULL) with its depth; on fetch, the row is
  updated (`last_fetched_at=now()`, status, ETag, Last-Modified, content hash). A
  re-run loads persisted state, marks fetched URLs so they are **not** refetched, and
  re-queues pending rows at their recorded depth — so an interrupted crawl continues
  instead of restarting blindly.
  - *ponytail ceiling:* skipping already-fetched pages resumes an interrupted crawl,
    but interacts with the sink's full-sync deletion detection (unseen ⇒ soft-delete,
    ADR-0038). Reconciling a resumed full sync with deletion detection is deferred to
    STORY-07.4 (conditional fetch makes re-seeing a page cheap: a HEAD/304), so a
    scheduled full sync can re-enumerate every page without re-downloading unchanged
    bodies. STORY-07.1's crawl e2e uses a recording sink, not the deletion sink.

- **URL normalisation — hand-rolled, no dependency.** Lowercase scheme+host, drop the
  default port, drop the fragment, strip `utm_*` and a small known-tracker set, sort
  the remaining query (SPEC-04 §2). `PuerkitoBio/purell` is archived; the rules are a
  dozen lines over `net/url`, so no dependency is added (lazy-senior reuse-first).

- **robots.txt — a small hand-rolled parser, no dependency.** `parseRobots` selects
  the longest matching `User-agent` group (falling back to `*`), and `allowed` does
  longest-prefix Allow/Disallow precedence. A fetch error or non-2xx robots response
  is treated as allow-all (the permissive convention). This is ~80 lines over
  `net/http`; `github.com/temoto/robotstxt` was considered and **not** added — the
  scoped parser is comparable in size and correctness and avoids a new dependency
  (the reuse-first rule; the ADR is where the "acceptable if justified" bar is met and
  declined). Upgrade path: swap in `temoto/robotstxt` if crawl-delay directives, sitemap
  hints or exotic wildcard patterns become necessary.

- **Egress seam (STORY-07.2).** Every fetch goes through the `Doer` interface
  (`egress.go`); the default is an `*http.Client` with a 30 s timeout and a redirect
  cap, and `http.DefaultTransport`. 07.2 replaces the Doer (or the transport's
  `DialContext`) with an SSRF-guarded dialer — each redirect hop re-dials, so the
  guard re-validates per hop automatically — with **no** change to crawl logic.
  *ponytail:* the default permits loopback so httptest-based unit tests reach
  127.0.0.1; the 07.2 guard MUST block loopback in production, selected at the
  composition root with tests overriding it with a permissive Doer.

- **Extraction seam (STORY-07.3).** `extractHTML` pulls only what the crawler needs —
  title, `<link rel=canonical>` (the canonical URL becomes the Document `ExternalID`,
  SPEC-04 §2), and outbound links for the frontier. The page is emitted as **raw
  bytes** in `Document.Body` (SPEC-04 §2: non-HTML within the allowlist is passed as
  Body for the parse pipeline; HTML likewise for now). 07.3 slots readability +
  include/exclude selector extraction behind this same parse step.

- **Conditional-fetch state (STORY-07.4).** ETag/Last-Modified/content-hash ARE
  persisted to `crawl_pages` on every fetch (the columns already exist), but no
  If-None-Match/HEAD/304-skip optimisation is done here. 07.4 reads the stored state.

- **Canonical de-duplication.** Two URLs sharing a canonical emit one Document (an
  `emitted` set keyed by `ExternalID`), so query-string variants collapse.

## Consequences
- No schema change: `crawl_pages` already exists (SPEC-03 §2); the drift guard stays
  green, `api/openapi.yaml` is unchanged (no request/response schema moved).
- No new dependency: normalisation and robots are hand-rolled over the stdlib and the
  already-vendored `golang.org/x/net/html`, `golang.org/x/sync/errgroup`,
  `golang.org/x/time/rate`.
- `web_crawl` is now a registered connector, so the sources API runs its JSON-Schema
  config validation and (config-level) test-connection; full cross-kind reachability
  testing is STORY-07.8.
- The three seams (egress/extraction/conditional-fetch) are the contract EPIC-07's
  next stories build against; hardening the fetch path or the extractor touches no
  crawl logic (NFR-MNT-01).
- The worker/scheduler that constructs a real `SyncRun` (with `NewTenantPageStore` as
  `State` and the ingestion sink) is EPIC-09; STORY-07.1 ships the connector and the
  tenant.DB-backed store it will wire.
