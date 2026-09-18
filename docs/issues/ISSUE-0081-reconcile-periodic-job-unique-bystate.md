# ISSUE-0081: reconcile_jobs periodic job never enqueues (River unique ByState missing required states)

**Type:** Bug · **Status:** Done · **Priority:** High · **Traces:** SPEC-08 §3, ADR-0078, ADR-0005

## Summary
The worker logs, every `reconcileInterval` (60 s):

```
ERROR PeriodicJobEnqueuer: Internal error generating periodic job
  error="UniqueOpts.ByState must contain all required states, missing: pending, scheduled"
```

The `reconcile_jobs` periodic job (`internal/worker/worker.go`) set a custom unique
`ByState: {available, running}`. River v0.15's `PeriodicJobEnqueuer` rejects a custom unique `ByState`
that omits any required non-terminal state (`pending`, `scheduled`, `available`, `running`), so it
never enqueues the job. The mirror reconciler (SPEC-08 §3, ADR-0078) therefore never runs on its
schedule, so an orphaned `running` mirror row left by a crashed worker only heals on the next worker
restart (`RunOnStart`), not within a minute as designed.

## Why it slipped through
The reconcile unit test and e2e drive `jobs.Reconciler.Reconcile` directly; neither starts a real
River client with `PeriodicJobs`, so the enqueuer validation was never exercised.

## Root cause
A custom unique `ByState` must include River's required non-terminal states. `syncActiveStates`
(`internal/worker/args.go`) — used for `sync_source` uniqueness — is already exactly that non-terminal
set (`pending, scheduled, available, running, retryable`), so the reconcile job should reuse it rather
than declare a narrower, invalid set.

## Fix
- `reconcileInsertOpts()` (`internal/worker/reconcile.go`) now returns
  `UniqueOpts{ByState: syncActiveStates}`; `worker.go`'s periodic-job closure calls it. Dedup intent is
  unchanged: a new reconcile is skipped while a prior one is still un-finished.

## Tests
- `internal/worker` unit `TestReconcileInsertOptsHasRequiredUniqueStates`: asserts the wired unique
  `ByState` contains `pending`, `scheduled`, `available`, `running` — a regression guard so River can
  always enqueue the periodic reconcile.

## Related
ADR-0078 (mirror reconciliation), ADR-0005 (River job queue), ISSUE-0078.
