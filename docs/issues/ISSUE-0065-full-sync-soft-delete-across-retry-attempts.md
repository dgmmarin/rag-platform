# ISSUE-0065: A full sync spanning multiple retry attempts soft-deletes documents ingested by earlier attempts

**Type:** Bug · **Status:** Todo · **Story:** EPIC-05 (ingestion) / EPIC-09 (jobs) · **Traces:** SPEC-05 §1/§5, ADR-0008, ADR-0038, ISSUE-0062

## Symptom
A full `sync_source` of a large source (988 crawled pages, `manual.tourpaq.com`) ran to
`succeeded`, but afterwards only a small tail of the corpus is live:

```
documents by status:  deleted = 810,  active = 153
live_chunks (queryable) = 2,491   /   chunks total = 14,209
```

All ~963 documents were parsed, chunked and embedded successfully (local Ollama `nomic-embed-text`,
768-dim, zero null vectors), but 810 of them ended `status='deleted'` — so the queryable corpus is
~16% of what was ingested.

## Root cause
The interaction of three correct-in-isolation behaviours:

1. **Job timeout + retries (ISSUE-0062):** `sync_source` has a 30-minute per-attempt timeout. On a
   CPU-bound embed backend the full crawl did not finish in one attempt, so River ran it as **3
   attempts** (attempt 1 → timeout → attempt 2 → timeout → attempt 3 → success), each attempt a fresh
   `Work` call with its own `run.started_at`.
2. **Resumable crawl (ADR-0046):** on a resumed attempt the web-crawl connector **skips** pages it
   already fetched (conditional fetch / crawl_pages state). So attempt 3 re-touched only the pages
   still outstanding when it started — it did **not** re-`Put` the ~810 documents attempts 1 & 2 had
   already ingested, leaving their `last_seen_at` at the earlier attempts' timestamps.
3. **Full-sync soft-delete (SPEC-05 §1, `Sink.Complete`):** a **full** sync ends by soft-deleting
   every document of the source with `last_seen_at < run.started_at` (deletion reconciliation). On
   attempt 3, `run.started_at` is attempt 3's start, so the 810 documents last seen during attempts
   1 & 2 match the predicate and are soft-deleted as "no longer present at the source."

Net: the completing attempt treats documents ingested by earlier attempts of the **same logical
sync** as deleted. A full sync is only safe to reconcile deletions when a single attempt observed the
**whole** source; a timeout-split full sync violates that assumption.

## Impact
- Silent data loss for any full sync whose crawl exceeds the job timeout (large sources on slow/CPU
  embed backends are the common trigger). The job reports `succeeded`; nothing signals the loss.
- An **incremental** sync does not soft-delete (SPEC-05 §5), so it is unaffected — only `full=true`.

## Fix options (decide when picked up)
1. **Do not reconcile deletions on a resumed attempt.** Detect `job.Attempt > 1` (or a persisted
   "sync run id") and skip `Sink.Complete`'s soft-delete unless this attempt observed the full
   enumeration. Simplest; deletions then reconcile on the next clean single-attempt full sync.
2. **Scope "seen" to the whole logical sync, not one attempt.** Persist the logical sync's start (the
   first attempt's `started_at`) in the job payload/state and pass it as the soft-delete cutoff, so
   documents seen by any attempt of this sync are retained.
3. **Make a full sync finish in one attempt.** Raise/tune `syncJobTimeout` (or derive it from
   `max_pages`) so a large crawl completes before the timeout. Weakest — only moves the boundary.

Option 1 or 2 is the real fix; 3 is a mitigation. Prefer 2 if the crawl-run identity is easy to
thread; else 1.

## Reproduce / verify
- Seed a source whose full crawl+embed exceeds `syncJobTimeout` (e.g. a large crawl on a CPU embed
  endpoint). Trigger a `full` sync; observe it succeed across ≥2 attempts; then
  `select status, count(*) from documents group by 1` shows a large `deleted` count.
- A regression test would drive the sink/worker with a run that "sees" only a subset after a
  simulated resume and assert `Complete` does not soft-delete the earlier-seen documents.

## Workaround (until fixed)
Re-run the sync once the corpus is stable (a single clean attempt re-touches everything and
un-deletes), or use an **incremental** sync (no soft-delete). Raising `syncJobTimeout` so the full
sync fits one attempt also avoids it.
