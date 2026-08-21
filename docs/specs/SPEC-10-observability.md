# SPEC-10: Observability

**Implements:** FR-OBS-01..04, FR-ADM-06

**Scaffolding (STORY-01.6):** the `internal/obs` package delivers the reusable
seam for §1–4 — a JSON `slog` logger with the base fields, a Prometheus registry
exposing the representative `api_request_duration_seconds` histogram at
`/metrics`, HTTP middleware that injects/propagates `request_id` and emits one
structured line per request with `duration_ms`, an env-configured OpenTelemetry
tracer (disabled by default, W3C tracecontext propagation) with a shutdown hook,
and `/healthz` + `/readyz` handlers (readiness is a skeleton with a `Check` seam).
`ragctl serve` starts this minimal server. The full metric catalogue (§2) is
STORY-10.1, span instrumentation of request paths (§3) is STORY-10.3, and the
real readiness checks (§4) land with STORY-02/09. The tenant label/field is `-`
until tenant resolution (STORY-02) fills it.

## 1. Logging
`log/slog` JSON. Mandatory fields where applicable: `ts, level, msg, service, request_id, tenant_id, job_id, source_id, user_id|api_key_id, duration_ms, err`. Content fields (question, document text) are never logged at info level.

## 2. Metrics (Prometheus)
| Metric | Labels |
|---|---|
| `api_request_duration_seconds` (hist) | route, status, tenant |
| `query_retrieval_duration_seconds` (hist) | tenant, reranked |
| `query_grounded_total` (counter) | tenant, grounded |
| `ingest_documents_total` (counter) | tenant, source_kind, result |
| `ingest_chunks_total`, `embed_tokens_total` | tenant, provider |
| `provider_request_duration_seconds`, `provider_errors_total` | provider, op, status |
| `jobs_queue_depth` (gauge) | queue |
| `jobs_duration_seconds` (hist), `jobs_failed_total` | kind |
| `tenant_pools_open` (gauge) | — |
Cardinality guard: tenant label is the slug; if tenants exceed 500, switch to per-tenant metrics in the control plane only.

## 3. Tracing
OpenTelemetry SDK; spans: `api.request`, `tenant.resolve`, `retrieval.hybrid_sql`, `retrieval.rerank`, `llm.complete`, `connector.sync`, `ingest.document`, `embed.batch`, `sidecar.parse`. Trace context propagated to the Python sidecar via W3C headers.

## 4. Health
- `/healthz`: process up.
- `/readyz`: control-plane DB reachable, River client started, at least one configured embedding provider responds to a cached ping (refreshed every 60 s).

## 5. Dashboards and alerts (initial)
- Query latency p95 per tenant > 800 ms for 10 min.
- Grounded rate per tenant drops > 20 points day-over-day.
- Job failures per kind > 5 in 15 min.
- Queue depth > 500 for 30 min.
- Provider error rate > 5 % for 5 min.
- Tenant migration mismatch count > 0.

## 6. Usage accounting
API and worker increment in-memory counters flushed every 30 s to `usage_daily` via `insert ... on conflict do update set col = col + excluded.col`.
