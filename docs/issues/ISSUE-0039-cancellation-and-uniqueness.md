# ISSUE-0039: Cancellation and uniqueness

**Type:** Feature · **Status:** Done · **Story:** STORY-09.4 · **Traces:** SPEC-08 §4, SPEC-08 §1, FR-ADM-02, ADR-0005, ADR-0031, ADR-0060, ADR-0062

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs
> (`docs/adr/`). This issue records STORY-09.4 for traceability.

## Summary
Wires the `jobs.Canceller` seam (nil since STORY-04.5) to River: cancelling a QUEUED
job drops it from the queue immediately and flips the mirror; cancelling a RUNNING job
signals River, whose context cancel stops the handler between documents (nothing
partial) and the mirror middleware records `cancelled`. Uniqueness ("one active sync per
source") was already delivered in STORY-09.2 and needed no new code.

## Scope
- `internal/cli/enqueue.go`: `riverCanceller` (implements `jobs.Canceller`) — looks up
  `river_job_id` and calls `client.JobCancel`; wired into the jobs service in
  `api_server.go`.
- `internal/cp/jobs/service.go`: the queued-cancel path now calls the Canceller (drop
  the River job) before flipping the mirror.
- `internal/worker/mirror.go`: the terminal cancel mapping now also fires on a REMOTE
  cancel, detected via `context.Cause(ctx)` (River's `ErrJobCancelledRemotely`).
- Docs: ADR-0062, this issue, backlog.
- Not in scope: uniqueness (STORY-09.2); a bulk-cancel API; audit of the cancel action
  (FR-ADM-05, rides the existing job lifecycle).

## Resolution
- **Queued:** `Canceller.Cancel` → `JobCancel` drops the River job so no worker claims
  it (SPEC-08 §4), then `CancelQueued` flips the mirror (the dropped job is never worked,
  so the middleware never writes the terminal). A missing/legacy-unlinked job is a
  no-op; `rivertype.ErrNotFound` is swallowed (idempotent).
- **Running:** the service signals River (202) and the row stays running until the
  worker exits. River cancels the work context with an `ErrJobCancelledRemotely` cause;
  the handler surfaces `context.Canceled` while `context.Cause` carries the cancel, so
  the middleware maps `isJobCancel(err) || isJobCancel(context.Cause(ctx))` → cancelled.
  A drain/hard-stop cancel has a different cause and stays retryable → queued.
- **Nothing partial:** the sink commits per document (SPEC-05 §5); a cancelled run keeps
  finished documents and abandons the rest.

## Tests
- Unit (`internal/worker`, hermetic): a remote cancel (handler returns `context.Canceled`
  with a `river.JobCancel` context cause) → cancelled; a plain context cancel → retrying.
- Unit (`internal/cp/jobs`, hermetic): a queued cancel with a Canceller wired both drops
  the River job (Canceller called) and flips the mirror row; the running-cancel and
  nil-Canceller-seam tests still hold.
- e2e (`test/e2e/worker_e2e_test.go`, real Postgres): a running job held in-flight by a
  gated fetcher is cancelled through River, stops, mirrors `cancelled` (never
  succeeded), and commits no document.
- Build/vet green (`-tags e2e`); full unit suite green.
