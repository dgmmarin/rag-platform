# ADR-0059: River integration and the `ragctl work` binary — the client on the control-plane pool, River's own migrator kept apart from the goose drift guard, isolated per-queue concurrency, and per-job TenantDB with a graceful drain

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-ING-08, SPEC-08 §1/§3, SPEC-09 §2, C-3, C-4, ADR-0003, ADR-0005 · **Decisions:** ADR-0010, ADR-0038

## Context
FR-ING-08 / SPEC-08 §1: the platform runs asynchronous jobs (document ingestion,
source syncs, maintenance) on a Postgres-backed River queue with three queues —
`ingest`, `maintenance`, `platform` — each with **independent** worker concurrency,
so a long reindex on `maintenance` cannot starve syncs on `ingest`. Every job carries
its `tenant_id`; the worker opens the tenant's own database **per job**; graceful
shutdown drains in-flight work rather than abandoning it.

Producers already exist and enqueue River jobs directly and transactionally (ADR-0005:
River state is authoritative, the control-plane `jobs` table mirrors it). What was
missing until this story is the *consumer*: `ragctl work` was a STORY-01.1 stub. Every
job **handler** also already exists as a plain function or service (ingestion is the
STORY-06.3 `ingestdoc.Ingestor`, sync is the STORY-05 sink pipeline, GC is the
STORY-05.9 retention sweep) — deliberately built worker-free so this story is *wiring*,
not new domain logic. Scope: the River client + the `work` binary + the composition
root that assembles the handlers; not the handlers themselves, and not job mirroring
(STORY-09.2), the scheduler (STORY-09.3) or cancellation/uniqueness (STORY-09.4).

## Options / decisions
- **The River client runs on the CONTROL-PLANE pool; tenant data is reached only
  per-job through the resolver.** River's queue tables live in the control plane
  (ADR-0005); the client is built with `riverpgxv5` over that pool and NEVER a tenant
  pool (C-3). A handler receives its job, parses `tenant_id`, and opens a **fresh**
  `tenant.DB` from `tenant.Resolver` (ADR-0003; pools are cached, so this is cheap) —
  that per-job open is the AC and the structural tenant boundary (C-1): a job can only
  ever touch the database its own `tenant_id` resolves to. A bad/empty `tenant_id` is a
  permanent `JobCancel`, not a retry loop.

- **River's own migrator is applied at `work` startup and kept ENTIRELY SEPARATE from
  the goose-managed `control_plane` schema and its drift guard.** `Worker.Migrate` runs
  `rivermigrate` (River's tables: `river_job`, `river_leader`, `river_queue`,
  `river_migration`, …) up on the control-plane pool. These are River-owned and
  versioned by River, so they are **not** part of the goose migration set,
  `ExpectedControlVersion`, or the ADR-0028 schema-drift guard — folding them in would
  make the drift guard flap on every River upgrade and couple our migration numbering to
  a third party's. `work` applies them itself (idempotent; River records applied
  versions) rather than adding a goose migration. This is the decision the worker code
  forward-references as "ADR-0059".

- **Three queues, isolated concurrency, defaults tuned to the workload.** Each queue is
  a River `QueueConfig` with its own `MaxWorkers` (`QueueConcurrency`: ingest 8,
  maintenance 2, platform 2), overridable per flag/env (`--ingest-concurrency`, …). A
  zero takes the default. Because River fetches per queue against a separate worker
  budget, a saturated `maintenance` queue cannot consume `ingest` slots (SPEC-08 §1). A
  `--queues` selection may restrict which queues a given worker consumes (e.g. a
  dedicated ingest-only worker); an empty or unknown selection safely falls back to all
  three.

- **Kinds whose handlers are out of scope are REGISTERED but fail loud and permanent.**
  `ingest_document` and `sync_source` are wired end-to-end and `gc_tenant` to the
  retention sweep; the remaining SPEC-08 kinds (`reindex_tenant`, `delete_source`,
  `provision_tenant`, `delete_tenant`) are registered with a `todoWorker` that logs and
  returns `JobCancel` naming the owning story. This completes the queue structure so an
  accidentally-enqueued job of an unbuilt kind fails **loudly and permanently** (no
  silent vanish, no infinite retry) instead of ballooning this story to build every
  handler. Each names the story that will replace it.

- **`work` loads the DEK at startup and fails closed, exactly like `serve`.** Per
  SPEC-09 §2 the worker needs the data-encryption key (it decrypts source credentials
  for syncs, C-4), so `WorkCmd.Run` loads the startup cipher first and returns before
  opening any pool or consuming any job when the DEK is missing. `work` is therefore no
  longer a wired-but-unimplemented stub: it stops returning `ErrNotImplemented`, and per
  ADR-0010 its exit-2 assertions move in the same change. It was the **last** stub, so
  no live command returns `ErrNotImplemented` any more; the exit-code contract's exit-2
  mapping is preserved as a documented contract and kept guarded by unit-testing the
  pure `codeFor` mapping in `cmd/ragctl` directly (rather than through a live stub), with
  a stack-free `TestWorkFailsClosedWithoutDEK` e2e mirroring serve's fail-closed path.

- **Graceful shutdown drains via `client.Stop`, bounded by a drain deadline.**
  `runWorker` blocks until SIGINT/SIGTERM (`signal.NotifyContext`), then calls
  `Worker.Stop`, which waits for in-flight jobs to finish before returning (a bounded
  `workerDrainTimeout`; `StopAndCancel` would abandon instead). Handlers that respect
  their context exit between documents committing nothing partial (SPEC-05 §5), so a
  drained job ends `completed`, never abandoned mid-flight — the graceful-shutdown AC.

- **The composition root owns all connector-/provider-specific knowledge.**
  `internal/cli.buildWorkerDeps` assembles the resolver, control-plane source/settings
  reads, the connector `Registry` + a per-kind state-store factory (resumable crawl
  store for web_crawl/sitemap, generic connector-state store otherwise), the parse /
  embed / store stages, and object storage for `ingest_document` byte read-back. The
  `worker` package takes these as seams (`SourceStore`, `SettingsSource`,
  `StateStoreFactory`, `EmbedderFactory`, `Fetcher`), so adding a connector or provider
  needs no worker change (NFR-MNT-01) and an e2e can inject network-free stubs for the
  two external effects (embedding provider, object storage).

## Consequences
- The queue is live: producers' rows are now consumed, ingestion and sync run
  end-to-end off Postgres, and unbuilt kinds fail safe until their story lands.
- River upgrades touch only River's own migration set; the goose drift guard and
  `ExpectedControlVersion` stay stable, and there is no migration to review for this
  story.
- Tenant isolation holds end-to-end: the client never touches a tenant pool, and each
  job reaches exactly one tenant database through the resolver (C-1, C-3).
- The exit-code contract (ADR-0010) survives the disappearance of the last stub: exit 2
  remains defined and CI-gated via the `codeFor` unit test, so a future re-introduced
  stub is still mapped correctly.
- The `work` binary is long-lived and DEK-gated like `serve`; its real golden path
  (consuming jobs off the live control plane, proving per-job TenantDB and drain) is
  `test/e2e/worker_e2e_test.go`.
