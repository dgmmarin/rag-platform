# Runbook: Observability — alerts, metrics, and first response

**Traces:** SPEC-10 §2/§5, FR-OBS-02. **Alerts:** `deploy/prometheus/rules/ragctl.rules.yml`.
**Dashboards:** `deploy/grafana/dashboards/`.

This is the alert → action index: every alert in the committed Prometheus rules links
here (its `runbook_url` points at one of the sections below). Each section says what the
alert means, where to look, and the first steps; deeper procedures live in the sibling
runbooks (stuck job, provider outage) and are linked from here.

## Where to look first

- **Metrics:** `ragctl serve` exposes `/metrics` (API, retrieval, LLM/rerank provider
  metrics); `ragctl work` exposes `/metrics` on `RAGCTL_WORKER_METRICS_ADDR` (default
  `:9091`) for jobs, ingestion, and embedding-provider metrics (SPEC-10 §2).
- **Dashboards:** API, ingestion, jobs, providers, per-tenant (`deploy/grafana/dashboards/`).
- **Traces:** one trace covers API → retrieval → provider, and worker job → sidecar
  (SPEC-10 §3); filter by `tenant`/`job.id` to follow a single request.
- **Logs:** structured JSON, one line per request/job, carrying `request_id`, `tenant_id`,
  `job_id` — never content at info level (SPEC-10 §1).

## Query latency

**Alert:** `QueryLatencyP95HighPerTenant` — query p95 for a tenant > 800 ms for 10 min.

1. Open the **per-tenant** dashboard; confirm which tenant and whether it is retrieval or
   generation latency (`query_retrieval_duration_seconds` vs the overall request).
2. If retrieval is the culprit, check the tenant's corpus size and whether a reindex or
   large sync is running (jobs dashboard) — HNSW maintenance under heavy ingest slows reads.
3. If generation, check provider latency (see [Provider errors](#provider-errors)); a slow
   provider inflates end-to-end query time even when retrieval is fast.
4. Confirm the pool is not exhausted (`tenant_pools_open`) and the tenant DB host is healthy.
5. Mitigate: throttle the competing sync (per-tenant ingest cap), or scale the tenant's
   Postgres. Capacity work is tracked against NFR-PERF-01 (load test, `test/load/`).

## Grounded rate drop

**Alert:** `GroundedRateDropPerTenant` — a tenant's grounded answer rate fell > 20 points
day-over-day.

A drop means more queries are being **refused** (no chunk passed `min_score`, so no LLM
call — the grounding/refusal rule, ADR-0004/SPEC-06). Usual causes:

1. **Retrieval regression** — a bad or empty reindex, a source that stopped syncing, or a
   `min_score`/embedding-model change. Check the ingestion dashboard and the tenant's recent
   jobs; confirm `chunks.embedding_model` matches the tenant's configured model (Invariant 3).
2. **Content gap** — the questions shifted to topics the corpus does not cover. Sample the
   query log (`query_log`, tenant DB) for the refused questions.
3. Mitigate: re-run the failed/stale sync (see [Stuck job](stuck-job.md)); if a reindex went
   wrong, roll the corpus back per its table-swap procedure (SPEC-03 §5).

## Job failures

**Alert:** `JobFailuresHighPerKind` — > 5 jobs of one kind failed in 15 min.

1. Identify the kind and tenant from the alert label and the jobs dashboard.
2. Inspect the failing jobs in the control-plane `jobs` mirror (kind, last error, attempt).
3. Follow the deep procedure in **[Stuck job](stuck-job.md)** to triage a wedged or
   repeatedly-failing job (cancel, drain, requeue).
4. If `sync_source` jobs fail against one provider/source, cross-check
   [Provider errors](#provider-errors) and the source's credentials/allowlist.

## Queue depth

**Alert:** `JobQueueDepthHigh` — a queue's backlog > 500 for 30 min.

1. Check the jobs dashboard for which queue (`ingest` / `maintenance` / `platform`) is
   backed up and whether throughput dropped or arrivals spiked.
2. Confirm workers are running and not crash-looping (`ragctl work` logs); River leader
   elected; the control-plane pool healthy.
3. If under-provisioned, scale worker concurrency (`RAGCTL_INGEST_CONCURRENCY` etc.); if one
   tenant is flooding the queue, the per-tenant ingest cap protects the others (SPEC-08 §1).
4. A single wedged job blocking a queue → **[Stuck job](stuck-job.md)**.

## Provider errors

**Alert:** `ProviderErrorRateHigh` — a provider's error rate > 5 % for 5 min.

This is an external LLM / embedding / rerank provider degrading. Answers degrade gracefully
(retrieval-only on `llm.ErrCircuitOpen`, NFR-REL-04) rather than hard-failing. Follow the
full procedure in **[Provider outage](provider-outage.md)**.

## Related runbooks

- [Provider outage](provider-outage.md) · [Stuck job](stuck-job.md) ·
  [Incident response](incident-response.md)
- [Backups and PITR](backup-and-pitr.md) for a data-loss recovery.
