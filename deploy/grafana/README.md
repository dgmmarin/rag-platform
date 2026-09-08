# Grafana dashboards

Prometheus dashboards for the RAG platform, one per plane, tracing **SPEC-10 §2**
(the metric catalogue) and **SPEC-10 §5** (the initial dashboards and alerts).
Delivered by STORY-10.1 (see `docs/issues/ISSUE-0044-metrics-catalogue-and-dashboards.md`).

## Dashboards

| File | UID | Covers |
|---|---|---|
| `dashboards/api.json` | `ragctl-api` | Request latency p95, request rate by status, rate-limited requests, tenant pools open. |
| `dashboards/ingestion.json` | `ragctl-ingestion` | Documents ingested, chunks written, embedding tokens (per provider). |
| `dashboards/jobs.json` | `ragctl-jobs` | Queue depth, job failures per kind, job duration p95, throughput. |
| `dashboards/providers.json` | `ragctl-providers` | Provider error rate, request latency p95, request rate. |
| `dashboards/per-tenant.json` | `ragctl-per-tenant` | Per-tenant query latency p95, retrieval p95, grounded rate, query rate. |

Each dashboard exposes a `datasource` variable — pick the Prometheus instance
scraping the API server (`ragctl serve`) and the worker (`ragctl work`, whose
`/metrics` endpoint listens on `RAGCTL_WORKER_METRICS_ADDR`, default `:9091`).

## Metric emission (STORY-10.1)

The whole SPEC-10 §2 catalogue is emitted. `ragctl serve` exposes the API-plane and
retrieval/answering metrics (`api_request_duration_seconds`, `api_rate_limited_total`,
`tenant_pools_open`, `query_retrieval_duration_seconds`, `query_grounded_total`) plus the
LLM/rerank provider metrics. `ragctl work` exposes the jobs plane (`jobs_duration_seconds`,
`jobs_failed_total`, `jobs_queue_depth`), ingestion throughput (`ingest_documents_total`,
`ingest_chunks_total`, `embed_tokens_total`) and the embedding provider metrics. Provider
metrics (`provider_request_duration_seconds`, `provider_errors_total`) therefore appear on
whichever process made the call — scrape both.

## Provisioning

Point Grafana's dashboard provisioner at `dashboards/`, e.g.:

```yaml
# /etc/grafana/provisioning/dashboards/ragctl.yaml
apiVersion: 1
providers:
  - name: ragctl
    type: file
    options:
      path: /var/lib/grafana/dashboards/ragctl
```

The SPEC-10 §5 alert thresholds are annotated on the relevant panels; the alert
*rules* themselves are STORY-10.2.
