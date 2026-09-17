# ISSUE-0075: A Full re-crawl fetched nothing and soft-deleted the whole corpus

**Type:** Bug · **Status:** Fixed · **Priority:** Critical · **Traces:** FR-SRC-01, SPEC-04 §1/§2, SPEC-05 §1/§5

## Summary
Triggering a **Full re-crawl** (`full=true`) on the manual.tourpaq.com source fetched **0 pages**
(`DocsSeen=0`, `BytesFetched=0`) yet was recorded `succeeded`, and the run then soft-deleted **all 963
documents** (active dropped 963 → 0). The site was reachable and robots allowed crawling; the fault was
two compounding bugs in the crawler/sink.

## Root causes
1. **Full re-crawl resume-skipped every page.** The crawl persists fetched pages in `crawl_pages`.
   `internal/connector/webcrawl/crawl.go` resume-skipped any page with `Fetched && full` — intended for
   RETRY resumption, but the flag persists across separate runs, so a fresh full re-crawl over a
   fully-fetched state skipped everything and fetched nothing.
2. **An empty crawl still ran the delete pass.** `sink.Complete` (Full mode) soft-deletes every
   document not seen since the run start. With zero documents seen, that deleted the entire corpus. The
   ISSUE-0072 fix only guarded a *context-timeout* truncation, not a clean-but-empty crawl.
3. (Compounding) the run-series delete boundary from ISSUE-0065 meant "not seen since the job's
   CreatedAt", so the previously-active 153 docs were in scope and went too.

## Fixes
- **Safety net (critical), `internal/ingest/sink/sink.go`:** `Complete` skips the delete pass entirely
  when `stats.DocsSeen == 0`. An empty crawl can never wipe the corpus again, whatever the cause.
- **Resume scoping, `internal/connector/webcrawl/crawl.go` + `internal/connector/connector.go` +
  `internal/worker/sync.go`:** `SyncRun.Since` carries the run-series start (the River job `CreatedAt`,
  stable across retries). A full sync resume-skips a fetched page only when `LastFetchedAt >= Since`
  (this run's earlier attempt); a page fetched in a prior run is re-fetched, so a fresh full re-crawl
  re-lists the whole source. `Since==zero` keeps the old skip-all behaviour (unit tests).

## Recovery performed
The 963 documents were soft-deleted with content intact (14,209 chunks preserved, every doc kept its
current_version). Re-activated in place (`update documents set status='active', deleted_at=null where
status='deleted'`); retrieval verified working (grounded answer, 8 citations).

## Tests
- `internal/ingest/sink` `TestCompleteFullSyncZeroSeenSkipsDelete` (zero-seen → no delete); existing
  delete tests updated to seed `DocsSeen>0`.
- `internal/connector/webcrawl` `TestCrawlFullReCrawlRefetchesPriorRun` (fresh full run re-fetches
  prior-run pages) alongside `TestCrawlResumesFromPersistedState` (within-run resume still skips).
- `go build ./...`, `go test ./internal/...`: PASS.

## Related
ISSUE-0072 (timeout truncation → skip delete), ISSUE-0065 (run-series delete boundary), ISSUE-0073
(`.md` duplication).
