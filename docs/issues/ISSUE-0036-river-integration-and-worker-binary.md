# ISSUE-0036: River integration and worker binary

**Type:** Feature · **Status:** Done · **Story:** STORY-09.1 · **Traces:** FR-ING-08, SPEC-08 §1/§3, SPEC-09 §2, C-1, C-3, C-4, ADR-0003, ADR-0005, ADR-0010, ADR-0059

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-09.1 for traceability; the backlog
> story remains the authoritative work item.

## Summary
`ragctl work` was a STORY-01.1 stub. This story turns it into the real River worker:
it consumes the `ingest`/`maintenance`/`platform` queues off the control-plane
Postgres (ADR-0005), each with independent concurrency, opening the right tenant's
own `tenant.DB` per job from the job's `tenant_id` (ADR-0003), and drains in-flight
work on SIGINT/SIGTERM. Every job **handler** already existed worker-free (ingestion
STORY-06.3, sync STORY-05, GC STORY-05.9), so this is wiring + the binary, not new
domain logic. First story of EPIC-09.

## Scope
- New `internal/worker` package:
  - `Worker` wrapping a `river.Client` on the control-plane pool: `New` (registers a
    handler per SPEC-08 §1 kind, configures the three queues), `Migrate` (applies
    River's own schema), `Start`/`Stop` (Stop drains), `Client` (producers/tests
    enqueue).
  - `QueueConcurrency` (per-queue `MaxWorkers`, zero → default 8/2/2); `selectQueues`
    (optional `--queues` restriction, safe fallback to all three).
  - Handlers: `ingestWorker` (STORY-06.3 `ingestdoc.Ingestor`), `syncWorker` (resolve
    connector by kind → decrypt creds → Sync into the sink), `gcWorker` (STORY-05.9
    retention sweep). Registered-but-TODO `todoWorker[T]` for `reindex_tenant`,
    `delete_source`, `provision_tenant`, `delete_tenant` — fail loud + permanent
    (`JobCancel`, no retry) so the queue structure is complete without building every
    handler.
  - Job-arg types (`IngestDocumentArgs`, `SyncSourceArgs`, `GCTenantArgs`, …), each
    carrying `tenant_id`; queue-name constants; seams (`SourceStore`, `SettingsSource`,
    `StateStoreFactory`, `EmbedderFactory`) + the `KeyedEmbedderFactory` production impl.
- `internal/cli/worker.go`: `runWorker` (build → River-migrate → start → block on
  signal → drain via `Stop`, bounded by `workerDrainTimeout`), `buildWorkerDeps`
  (composition root: resolver, source/settings reads, connector registry + per-kind
  state-store factory, parse/embed/store stages, object storage for read-back).
- `internal/cli/commands.go`: `WorkCmd` gains per-queue concurrency flags and a real
  `Run` — loads the startup DEK (fail-closed, SPEC-09 §2) then `runWorker`.
- Exit-code contract (ADR-0010): `work` stops returning `ErrNotImplemented`; it was the
  last stub, so `cmd/ragctl` extracts the pure `codeFor` mapping to keep the exit-2
  contract unit-testable, and its e2e/unit assertions move in the same change.
- Docs: ADR-0059, this issue, backlog (STATUS/TASKS/BACKLOG).
- Not in scope: job status mirroring to `jobs` (STORY-09.2), the scheduler
  (STORY-09.3), cancellation/uniqueness (STORY-09.4), per-tenant fairness (STORY-09.5),
  the delete-source handler (STORY-09.6); the job handlers themselves (untouched beyond
  wiring); no new API endpoint, no schema/goose migration.

## Resolution
- **Queues + isolated concurrency (SPEC-08 §1):** three `QueueConfig`s with separate
  `MaxWorkers`, so a saturated `maintenance` queue cannot consume `ingest` slots.
  Defaults 8/2/2, overridable per flag/env; `--queues` may restrict a worker to a
  subset (unknown/empty → all three).
- **Per-job TenantDB (ADR-0003, C-1):** each handler parses `tenant_id` and opens a
  fresh `tenant.DB` from the resolver; a job can only touch its own tenant's database.
  A bad/empty `tenant_id` is a permanent `JobCancel`, not a retry.
- **River migrator apart from the drift guard (ADR-0059):** `Worker.Migrate` runs
  `rivermigrate` on the control-plane pool; River's tables stay River-owned and are
  **not** part of the goose set, `ExpectedControlVersion`, or the ADR-0028 drift guard.
  No goose migration was added.
- **Graceful drain:** `runWorker` blocks on `signal.NotifyContext(SIGINT, SIGTERM)`,
  then `Worker.Stop` waits for in-flight jobs (bounded `workerDrainTimeout`). Handlers
  respecting ctx commit nothing partial (SPEC-05 §5), so a drained job ends `completed`.
- **Fail-closed startup (SPEC-09 §2):** `work` loads the DEK before opening any pool;
  missing DEK → exit 1 with a DEK error, never a partial start. **ponytail:** the
  unbuilt maintenance/platform kinds are registered as loud-fail TODO handlers rather
  than omitted — ceiling is "no real handler yet", upgrade path is each named story.
- **Exit-code contract (ADR-0010):** `codeFor(err) int` extracted in `cmd/ragctl` maps
  nil/help → 0, `ErrNotImplemented` → 2, anything else → 1; behaviour of `run`
  unchanged. Keeps exit-2 guarded now that no live command returns `ErrNotImplemented`.

## Tests
- Unit (`internal/worker`, hermetic — fake stores/resolver/embedder, no network/DB):
  `QueueConcurrency` defaulting; `selectQueues` restriction + fallback; job-arg
  round-trip and `Kind()`; the sink bridge; the `todoWorker` loud-fail. `cmd/ragctl`:
  `codeFor` maps nil/help/`ErrNotImplemented`/real error to 0/0/2/1 (table). `internal/cli`:
  `work` is implemented and never `ErrNotImplemented`, fails closed on the missing DEK;
  global flags accepted.
- e2e (`test/e2e/worker_e2e_test.go`, real control-plane Postgres, stubbed embedder +
  object storage): an enqueued `ingest_document` is CONSUMED off the queue, the worker
  opens the enrolled tenant's own DB from the job's `tenant_id` (the document lands in
  that tenant's `live_chunks`), and `Stop` BLOCKS on a gated in-flight job until it is
  released, after which the job is `completed` — not abandoned. `test/e2e/ragctl_e2e_test.go`:
  stack-free `TestWorkFailsClosedWithoutDEK` (exit 1, DEK named, no secret leak).
- Build/vet green (`-tags e2e` included); full unit suite green; no OpenAPI/schema/
  migration change.
