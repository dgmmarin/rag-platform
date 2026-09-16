# ISSUE-0062: sync_source / ingest_document jobs cancelled by River's 1-minute default timeout

**Type:** Bug · **Status:** Done · **Story:** EPIC-09 (jobs) · **Traces:** SPEC-08 §1, SPEC-05 §2, ADR-0059

## Symptom
A `sync_source` job failed repeatedly with:
```
"status": "running", "attempt": 2, "error": "context deadline exceeded"
```
The crawl never completes; each attempt is cancelled and retried on the SPEC-08 §1 backoff (1m/5m/30m).

## Root cause
River's `JobTimeoutDefault` is **1 minute** (`river@v0.15.0/client.go:42`). The worker client
(`internal/worker/worker.go`) sets no `Config.JobTimeout`, and no worker overrides `Timeout()` —
every worker embeds `river.WorkerDefaults`, whose `Timeout()` returns 0, meaning "use the default".
So River cancels each job's context after 60 seconds. A `sync_source` job crawls → parses → chunks →
embeds every document of a source at a politeness rate (`defaultCrawlRate` = 2 req/s), which cannot
finish in 60s for any real site — the context is cancelled mid-run and the in-flight fetch/DB/embed
call returns `context deadline exceeded`.

`ingest_document` had the same latent defect: SPEC-05 §2 allows the Python sidecar **120 seconds** to
parse one document — already double the 60s job deadline — so a slow PDF was cancelled before the
sidecar could return.

## Fix
Override `Timeout()` on the two ingestion workers with a budget that fits the work they do:
- **`internal/worker/sync.go`** — `syncWorker.Timeout()` → `30m` (`syncJobTimeout`). Covers a typical
  crawl; stays under the client's 1h `RescueStuckJobsAfter` net so a genuinely wedged job is still
  rescued. Upgrade path noted: derive the budget from the source's `max_pages`/rate for large crawls.
- **`internal/worker/ingest.go`** — `ingestWorker.Timeout()` → `5m` (`ingestJobTimeout`). Clears the
  120s sidecar parse ceiling plus batched embedding with its retry/backoff headroom.

Other workers (gc, delete_source, mirror) keep the 1-minute default — they are short by design; a
future long-running one adds its own `Timeout()` the same way.

## Regression guard
- **`internal/worker/timeout_test.go`** — `TestJobTimeoutsExceedRiverDefault` asserts
  `ingest_document`'s budget exceeds both `river.JobTimeoutDefault` and the 120s sidecar ceiling, and
  `sync_source`'s exceeds the default while staying under the 1h rescue window. RED confirmed (both
  returned 0s) then GREEN. `go test ./internal/worker/`: **PASS**.

## Operational note
The fix takes effect only once the **worker binary is rebuilt and restarted**. The already-failing
job runs its remaining attempt on the old code; re-run the sync after deploying and watch whether it
completes or surfaces a real connector error (unreachable host, SSRF block) that the timeout was
masking.
