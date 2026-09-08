# ISSUE-0040: Per-tenant concurrency caps and fairness

**Type:** Feature · **Status:** Done · **Story:** STORY-09.5 · **Traces:** SPEC-08 §1, ADR-0005, ADR-0060, ADR-0063

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs
> (`docs/adr/`). This issue records STORY-09.5 for traceability.

## Summary
A per-tenant concurrency cap on the ingest queue (default 2) so one tenant cannot occupy
every worker and starve the others (SPEC-08 §1). OSS River has no partition concurrency,
so it is a worker middleware: a job whose tenant is already at its cap is SNOOZED (not
blocked), freeing the worker goroutine to serve another tenant.

## Scope
- `internal/worker/limiter.go`: `tenantLimiter` (a `river.WorkerMiddleware`), per-process
  per-tenant running counter, snooze at cap; registered OUTERMOST (before the mirror) in
  `worker.New`.
- `worker.Deps.IngestPerTenantCap` + `--ingest-per-tenant-cap` /
  `RAGCTL_INGEST_PER_TENANT_CAP` (default 2), plumbed through `runWorker`.
- Docs: ADR-0063, this issue, backlog.
- Not in scope: a cluster-wide (shared-counter) cap (ponytail upgrade path); caps on
  maintenance/platform queues (SPEC-08 §1 caps ingest).

## Resolution
- **Snooze, don't block:** at cap, `river.JobSnooze` returns the worker goroutine
  immediately and re-queues the job; a blocking wait would hold the goroutine and cause
  the very starvation being prevented.
- **Outermost middleware:** the cap decision precedes the mirror's `running` write, so a
  yielded job stays `queued`.
- **Ingest only, per process:** the counter is in-memory per worker; ponytail — effective
  global cap is cap×R under R replicas, with a documented path to a shared counter.

## Tests
- Unit (`internal/worker`, hermetic, `-race`): the cap admits N per tenant and refuses
  the N+1th while a different tenant is unaffected; a released slot frees capacity; a
  non-ingest job passes through. **Synthetic load:** many concurrent ingest jobs for one
  tenant run at most `cap` at once (the rest snooze) while another tenant's job is served
  immediately — proving fairness.
- Build/vet/`-race` green; full unit suite green.
