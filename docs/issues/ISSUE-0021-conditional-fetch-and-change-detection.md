# ISSUE-0021: Conditional fetch and change detection

**Type:** Feature · **Status:** Done · **Story:** STORY-07.4 · **Traces:** FR-ING-02, SPEC-04 §2/§2c, ADR-0046 (Decisions: ADR-0038, ADR-0043)

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-07.4 for traceability; the backlog story
> remains the authoritative work item. STORY-07.4 advances EPIC-07 to 19/39.

## Summary
Close the STORY-07.1 conditional-fetch seam on the `web_crawl` connector: send
`If-None-Match`/`If-Modified-Since` from the ETag/Last-Modified stored in
`crawl_pages`, treat a `304 Not Modified` as unchanged (no read/parse/extract/emit —
only `last_fetched_at` is bumped), and fall back to a raw-body content-hash compare
for servers that send no validators (identical bytes ⇒ no re-emit). The response
validators are stored on every 200 so the NEXT crawl is conditional.

## Scope
- **`internal/connector/webcrawl/crawl.go` `process`** — build conditional request
  headers from the prior `crawl_pages` state; a `304` returns early via `markUnchanged`
  (no body read, no parse); on a `200`, compare `sha256(body)` to the stored hash and
  skip emit on a match; otherwise emit (07.3 path) and persist the new validators.
- **`crawl.go` `markUnchanged` (new)** — bumps `last_fetched_at`, carries the prior
  status/content-hash forward (refreshing ETag/Last-Modified if newly supplied),
  counts the page as *seen* (not *changed*) in `Stats`; no parse, no emit.
- **`crawl.go` `run`** — previously-fetched pages are re-queued for a **conditional
  re-visit** on an incremental sync (`SyncRun.Full == false`); a full sync keeps the
  STORY-07.1 resume-skip unchanged. `crawl_pages` state loaded at run start is held on
  the crawler as the source of prior validators.
- **Conditional GET over HEAD** — the AC's "HEAD/304" is realised as a conditional
  GET → 304 (one round trip, no body when unchanged); rationale in ADR-0046.

## Out of scope
Sitemap (07.5), HTTP API connector (07.6/07.7), SSRF (07.2, done), extraction quality
(07.3, done). No schema, no migration (the `crawl_pages` columns already exist), no
OpenAPI change. The `connector.Sink` interface is unchanged.

## Deletion-detection reconciliation
Conditional skip runs only on **incremental** syncs, where the ingestion sink's
`Complete` is a no-op (`sink.Incremental`, SPEC-05 §5) — so a 304 / unchanged page,
though not `Put`, can never be soft-deleted. The page is still marked seen in
`crawl_pages` (`last_fetched_at` bumped). A full sync's cheap-304-re-see for deletion
detection additionally needs a sink "mark seen without re-ingest" signal (a
`connector.Sink` change owned by EPIC-09); until then deletion detection is the
periodic full re-enumeration (SPEC-04 §4 pattern). See ADR-0046.

## Acceptance / DoD evidence
- TDD: three RED unit tests preceded the implementation —
  `TestCrawlConditional304SkipsParseAndEmit` (a 304 re-see emits nothing and does not
  follow the trap link in the 304 body — proof of "no parse"; `If-None-Match` sent;
  validators kept), `TestCrawlConditionalContentHashSkipsReEmit` (same bytes, no ETag
  ⇒ no re-emit, no parse), and `TestCrawlConditionalReEmitsOnChange` (differing bytes
  ⇒ re-emit).
- e2e (`TestWebCrawlConditionalFetch`, real Postgres+pgvector via the resolver +
  `tenant.DB`): a full crawl records the ETag + content hash in `crawl_pages`; an
  incremental crawl sends the conditional GET, the server returns 304, the sink gets
  nothing, `last_fetched_at` advances, and the stored validators survive. The
  STORY-07.1 resume e2e (`TestWebCrawlPersistsAndResumes`) still passes.
- `mise run test` green except the known `internal/cli` `.env`-injection caveat
  (passes with a clean env); `mise run e2e` webcrawl golden paths green.
- `go vet ./...` clean (lint bar). `crawl.go`/`crawl_test.go` add zero
  golangci-lint findings (pre-existing findings in `egress_test.go` etc. untouched).
- No schema/OpenAPI change; drift guard green.
