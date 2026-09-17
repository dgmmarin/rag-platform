# ISSUE-0072: A timed-out or truncated crawl must not full-sync soft-delete

**Type:** Bug · **Status:** Fixed · **Priority:** High · **Traces:** FR-SRC-01, SPEC-04 §2, ADR-0031

## Resolution
Fixed in `internal/connector/webcrawl/crawl.go` `finish`: it now calls `sink.Complete` (the FULL-sync
delete pass) **only when the crawl finished cleanly** (`cause == nil`). A crawl truncated by a context
timeout/cancel returns the error and skips the delete pass, so the job fails and retries instead of
soft-deleting the pages it never reached. Pages fetched before truncation were already committed by
their `sink.Put` and stay active. Covers both the web_crawl and sitemap connectors (shared `crawl.go`).
Tests: `TestCrawlTruncatedSkipsComplete` (truncated → `Complete` not called, returns `context.Canceled`)
and `TestCrawlCleanCrawlCompletesOnce` (clean crawl still reconciles once). `go test ./internal/...`:
PASS. Note: the resume-across-retries deletion (ISSUE-0065) is a separate, still-open path.

## Summary
When a `sync_source` crawl is cut short — it hits the 30-minute `syncJobTimeout`
(`internal/worker/sync.go:116`) with `context deadline exceeded` — the job is still recorded as
`succeeded` and full-sync reconciliation runs, soft-deleting every document the truncated crawl did
not re-see. A partial crawl therefore **deletes most of the corpus**, which is the opposite of the
intended "sync only touches what changed" behaviour.

## Observed (production, tenant acme / manual.tourpaq.com)
- Earlier crawls saw ~958–963 pages. A later crawl **timed out after 153 pages / 18 MB** (~12 s/page,
  the site was throttling after repeated heavy crawls).
- That run: `status=succeeded`, `error="timeout: context deadline exceeded"`, `attempt=3/3`,
  `stats.DocsSeen=153`, **`stats.DocsDeleted=0`**.
- Yet **810 documents were soft-deleted** in one event (all `deleted_at = 2026-09-16 16:00 UTC`),
  collapsing active coverage from ~960 to 153. The `DocsDeleted=0` stat is also wrong (the deletions
  were not counted), matching the ISSUE-0065 full-sync-across-retries accounting class.

## Impact
A transient slowdown / rate-limit on a large source silently destroys the majority of its ingested
content (recoverable only within the 30-day soft-delete GC window via a full re-crawl). Retrieval
quality and answer grounding degrade until an operator notices and re-crawls.

## Root cause (to confirm in code)
- The crawl runner treats a deadline-exceeded run as a completed sync and proceeds to the
  reconcile/soft-delete pass instead of failing the attempt.
- Full-sync reconciliation deletes "not seen this run" without a guard that the run actually
  completed (crawled to exhaustion / hit `max_pages`, not the timeout).

## Proposed fix (options)
1. **Fail, don't reconcile, on truncation:** if the crawl ends by context timeout/cancel (not natural
   completion or `max_pages`), mark the attempt failed and **skip the delete pass** entirely. Safest;
   a partial crawl never deletes.
2. **Reconcile only on a complete crawl:** carry a "crawl exhausted the frontier / hit max_pages"
   flag into reconciliation; delete unseen docs only when it is set.
3. Fix the `DocsDeleted` accounting so a truncated run at least reports the deletions it caused
   (visibility), independent of 1/2.

Prefer (1)+(3): a timed-out crawl should never be a deletion event.

## Tests
- Worker e2e: a crawl cancelled/timed-out mid-frontier leaves previously-active docs **active** (no
  soft-delete), and the job is not `succeeded`.
- Reconciliation unit test: unseen docs are deleted only when the crawl completed, not when truncated.

## Related
- ISSUE-0062 (sync job timeout), ISSUE-0065 (full-sync soft-delete accounting across retries),
  ISSUE-0073 (`.md` duplication inflates crawl time, making the timeout more likely).
