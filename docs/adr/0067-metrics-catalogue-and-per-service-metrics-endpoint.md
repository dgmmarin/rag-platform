# ADR-0067: The full metric catalogue lives in `obs.Metrics`, and every ragctl service — the worker included — exposes a Prometheus `/metrics` endpoint

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** FR-OBS-02, SPEC-10 §2/§5 · **Decisions:** ADR-0013

## Context
FR-OBS-02: all services SHALL expose Prometheus-compatible metrics including
per-tenant query latency, ingestion throughput, provider errors and job queue
depth (the SPEC-10 §2 catalogue). STORY-01.6/ADR-0013 delivered the seam — a
private `prometheus.Registry` inside `obs.Metrics`, one representative histogram,
and a `/metrics` handler wired into `ragctl serve`. Two structural gaps remained:

1. The catalogue held one metric, not the SPEC-10 §2 set, and the tenant label was
   hard-wired to `-` (nothing populated the resolved tenant, and the outer obs
   middleware could not see a value an inner layer set on its own child context).
2. The worker (`ragctl work`) — a first-class service that owns the whole jobs
   plane (queue depth, per-kind duration/failure) — exposed no HTTP surface at all,
   so its metrics could not be scraped.

## Options / decisions
- **One catalogue, one registry, per process.** `obs.Metrics` owns and registers the
  whole SPEC-10 §2 set with typed, nil-safe emission methods. A nil `*obs.Metrics`
  makes every method a no-op, so metrics stay an *optional* dependency threaded into
  a subsystem (retrieve, query, worker) exactly like the existing nil-safe `Usage`
  recorder and rate-limit counter — a subsystem wired without metrics still runs.
  The registry stays private (not the global default) so tests and multiple server
  instances never fight, as in ADR-0013.
- **The tenant label is fixed with a request-scoped mutable holder.** The obs
  middleware installs a `*tenantHolder` in the request context; the API-key scope
  middleware writes the resolved tenant into it via `obs.SetRequestTenant` after it
  authenticates the principal (never a client parameter — FR-ACC-03). The outer
  middleware reads the holder after the chain returns, so `api_request_duration_seconds`
  and the log line carry the real tenant. The label is the tenant id (bounded, one
  series per tenant), satisfying the SPEC-10 §2 cardinality guard's intent; a
  human-slug alias is a Grafana label-rewrite concern.
- **`tenant_pools_open` is a GaugeFunc over the resolver's live count**, registered at
  the composition root against a `NumPools()` the concrete resolver exposes (the
  `Resolver` interface is not widened — a type assertion keeps the gauge optional). A
  callback gauge can never drift from the pool cache.
- **The worker gets its own `/metrics` endpoint.** `ragctl work` builds an `obs.Metrics`,
  serves it on `RAGCTL_WORKER_METRICS_ADDR` (default `:9091`, empty disables) via the
  existing `obs.NewServeMux`, and never lets a metrics-serving failure bring the worker
  down. Job duration/failure come from a `metricsMiddleware` (innermost in the River
  `WorkerMiddleware` chain, so it times just the handler); queue depth from a one-minute
  sampler over River's own `river_job` table (state = `available`).

## Consequences
- The catalogue is defined once and scraped from both service planes; adding an emitter
  is a nil-safe method call, no registry plumbing.
- Per-tenant dashboards now key off a real tenant label instead of `-`.
- The worker exposes an HTTP listener it did not before — a new, small operational
  surface (one port to scrape, no auth; it serves only `/healthz`, `/readyz`, `/metrics`).
- Queue depth reads River internals (`river_job`); a documented ceiling — it counts only
  the `available` backlog, and the SQL is covered by the worker e2e, not a unit test.
- The ingest-pipeline and provider-client metrics (`ingest_*`, `embed_tokens_total`,
  `provider_*`) are emitted at a single chokepoint each: the ingestion sink (both the
  upload and connector-sync paths drive `sink.Put`), and the llm/embed/rerank resilience
  wrappers (`resilient`, `batcher.Embed`, a metered `Reranker` decorator). `obs.Metrics` is
  threaded through the provider factories (`llm.Factory`, `KeyedEmbedderFactory`,
  `KeyedRerankerFactory`) so the composition root sets it once. Provider labels are
  provider + operation (+ outcome) only — no per-request content (C-3/C-4). Because
  ingestion/embedding run in the worker while LLM/rerank run in `serve`, provider metrics
  land on whichever process made the call; both expose `/metrics`.
