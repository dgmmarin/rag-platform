# ISSUE-0044: Metrics catalogue and dashboards

**Type:** Feature · **Status:** Done · **Story:** STORY-10.1 · **Traces:** FR-OBS-02, SPEC-10 §2/§5, ADR-0013, ADR-0067

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs.

## Summary
Grows the STORY-01.6 observability seam into the SPEC-10 §2 metric catalogue and commits
the SPEC-10 §5 Grafana dashboards. `obs.Metrics` now owns and registers the full set with
nil-safe typed emission methods; the always-`-` per-tenant label is fixed; the worker
gains a `/metrics` endpoint so the jobs plane is scrapeable; and five dashboards (API,
ingestion, jobs, providers, per-tenant) ship as JSON.

## Scope
- `internal/obs/metrics.go`: register the catalogue (`api_request_duration_seconds`,
  `api_rate_limited_total`, `query_retrieval_duration_seconds`, `query_grounded_total`,
  `jobs_queue_depth`, `jobs_duration_seconds`, `jobs_failed_total`, `tenant_pools_open`)
  with nil-safe methods, `RateLimitedCounter()` and a `SetPoolGauge` GaugeFunc.
- `internal/obs/log.go` + `middleware.go`: a request-scoped `tenantHolder` +
  `SetRequestTenant` so the resolved tenant (from the authenticated principal, FR-ACC-03)
  labels the request histogram and log line instead of `-`.
- `internal/cp/auth/apikey_verify.go`: `RequireScope` records the resolved tenant.
- `internal/tenant/resolver.go`: `NumPools()` for the pools gauge.
- `internal/retrieve/service.go`: emit `query_retrieval_duration_seconds{tenant,reranked}`.
- `internal/query/query.go`: emit `query_grounded_total{tenant,grounded}`.
- `internal/worker`: `metricsMiddleware` (jobs duration/failure per kind) +
  `SampleQueueDepths` (queue-depth sampler over `river_job`).
- `internal/ingest/sink`: emit `ingest_documents_total{tenant,source_kind,result}`,
  `ingest_chunks_total{tenant,provider}`, `embed_tokens_total{tenant,provider}` — the
  single chokepoint both ingestion paths (`ingestdoc` upload + `sync` connector) drive.
  Labels populated at the two `sink.New` sites (tenant id, source kind, embedding provider).
- `internal/llm`, `internal/ingest/embed`, `internal/rerank`: emit
  `provider_request_duration_seconds{provider,op,status}` + `provider_errors_total` at each
  package's SDK/retry boundary — the `resilient` wrapper (op `llm.complete`/`llm.stream`),
  the `batcher.Embed` (op `embed`), and a metered `Reranker` decorator (op `rerank`).
  `obs.Metrics` is threaded through the `llm.Factory` / `KeyedEmbedderFactory` /
  `KeyedRerankerFactory` at the composition root.
- `internal/cli`: wire the counters/gauge into `ragctl serve` and the provider/ingest
  metrics into the worker + `serve` factories; add the worker `/metrics` endpoint
  (`RAGCTL_WORKER_METRICS_ADDR`, default `:9091`) + the queue-depth sample loop.
- `deploy/grafana/`: five dashboards + README (ingestion/providers now query real series).
- Docs: ADR-0067, this issue, SPEC-10, backlog.

## Resolution
The full SPEC-10 §2 catalogue is now registered AND emitted. API-plane and jobs-plane
metrics are scraped from `ragctl serve` / `ragctl work` respectively; the per-tenant label
carries the resolved tenant id (bounded per tenant), so the per-tenant dashboards read real
series. Ingestion throughput (`ingest_documents_total` / `ingest_chunks_total` /
`embed_tokens_total`) is emitted from the ingestion sink, and provider metrics
(`provider_request_duration_seconds` / `provider_errors_total`) from the llm/embed/rerank
resilience boundaries, labelled by provider + operation (+ outcome for errors) — counts and
durations only, never document content or secrets (C-3/C-4). The worker exposes `/metrics`
(ADR-0067) so the worker-plane ingest/embed/job metrics are scrapeable. Nothing deferred.

## Tests
- `internal/obs`: catalogue exposition (retrieval/jobs/rate-limited/pools) with labels;
  nil-`Metrics` methods are no-ops; the middleware labels a request with a tenant a later
  layer sets and keeps `-` otherwise.
- `internal/tenant`: `NumPools` counts open pools.
- `internal/cp/auth`: `RequireScope` populates the request tenant end-to-end through the
  obs middleware.
- `internal/retrieve`: a successful `Search` records the reranked retrieval histogram;
  nil-metrics Search is safe.
- `internal/query`: a grounded answer and a refusal record `query_grounded_total`
  true/false.
- `internal/worker`: the `metricsMiddleware` records duration for every job and failure
  only for a failing one, passing the error through unchanged; nil-metrics is safe.
- `internal/ingest/sink`: a changed doc records `ingest_documents_total{result="changed"}`
  + chunks + tokens; unchanged/failed docs record their own result label and spend no
  chunks/tokens.
- `internal/llm`, `internal/ingest/embed`, `internal/rerank`: a successful call records
  `provider_request_duration_seconds{status="ok"}` under the right provider/op (the error
  path + `provider_errors_total` is covered by the obs-level unit test).
- Build/vet/gofmt green. The worker `/metrics` endpoint and queue-depth SQL are covered by
  the worker e2e (needs Postgres), not a unit test.

## Not in scope
- Alert *rules* (SPEC-10 §5) — STORY-10.2.
