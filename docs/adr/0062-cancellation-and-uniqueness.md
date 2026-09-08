# ADR-0062: Cancellation and uniqueness — the River Canceller drops queued jobs and signals running ones, with the mirror detecting a remote cancel via the context cause

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** SPEC-08 §4, SPEC-08 §1, FR-ADM-02 · **Decisions:** ADR-0005, ADR-0031, ADR-0060

## Context
SPEC-08 §4: `POST /v1/jobs/{id}/cancel` cancels a job — River drops a **queued** job
immediately so no worker claims it, and a **running** job observes `ctx.Done()` between
documents and exits `cancelled`, committing nothing partial (SPEC-05 §5). SPEC-08 §1
also requires one active sync per source. STORY-04.5 built the cancel API with a nil
`Canceller` seam (ADR-0031): a queued cancel flipped the mirror row, a running cancel
returned the not-available seam. This story wires the seam to River.

Uniqueness was already delivered with the queue integration (STORY-09.2): `sync_source`
carries River `ByArgs` uniqueness on `source_id` over the active-state window, backed by
the `jobs_one_active_sync_per_source` partial index — so "one active sync per source"
holds without new work here.

## Options / decisions
- **A `riverCanceller` (composition root) implements `jobs.Canceller` over the River
  client.** It looks up `river_job_id` for the (tenant, job) mirror row and calls
  `client.JobCancel(riverJobID)`. River drops a queued job and signals a running one
  (cancelling its work context). A job with no linked River id (a legacy jobs-row-only
  kind) is a no-op; a River job already gone (`rivertype.ErrNotFound`) is not an error —
  cancel is idempotent.
- **A queued cancel now drops the River job AND flips the mirror.** Before the queue
  existed, flipping the mirror row was enough. Now a queued job has a real River job, so
  the service calls the Canceller (River drops it) **and** `CancelQueued` (flips the
  mirror) — otherwise a worker could still claim and run a job the admin "cancelled".
  The mirror flip is needed because a dropped queued job is never worked, so the
  middleware never runs to write the terminal.
- **A running cancel is left to the middleware.** The service signals River and returns
  202 (cancellation requested); the row stays `running` until the worker exits between
  documents. River cancels the work context with an `ErrJobCancelledRemotely` **cause**,
  so the handler surfaces `context.Canceled` while `context.Cause(ctx)` carries the
  cancel. The mirror middleware therefore maps `river.JobCancel(err)` **or**
  `isJobCancel(context.Cause(ctx))` to the cancelled terminal (extending the STORY-09.2
  terminal mapping). A plain drain/hard-stop cancel has a different cause and stays
  retryable → `queued`, so a shutdown never mislabels a job cancelled.
- **Nothing partial commits.** The ingestion sink already commits one document at a time
  in its own transaction (SPEC-05 §5) and the handlers respect their context, so a job
  cancelled mid-run leaves the documents it already finished and abandons the rest — the
  e2e asserts a cancelled single-document job commits nothing.

## Consequences
- Cancel is now fully effective for both states: a queued job vanishes from the queue,
  a running job stops promptly between documents, and the admin view reflects
  `cancelled` either way.
- The middleware's cancel detection is the one subtlety a future reader must keep: a
  remote cancel is recognised by the context *cause*, not the returned error, which is
  what separates it from an ordinary shutdown.
- Uniqueness needed no new code — it rides the STORY-09.2 River/index mechanisms.
- The Canceller runs on the insert-only client `serve` already builds; `JobCancel` is a
  database operation and needs no worker.
