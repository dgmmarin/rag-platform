# Prometheus alerting rules

Alert rules for the RAG platform (SPEC-10 §5), delivered by STORY-10.2
(`docs/issues/ISSUE-0045-alert-rules.md`). They reference the SPEC-10 §2 metric
catalogue emitted by `ragctl serve` and `ragctl work` (STORY-10.1) and use the
same thresholds annotated on the Grafana dashboards in `../grafana/dashboards`, so
alerts and dashboards never drift.

## File

`rules/ragctl.rules.yml` — Prometheus alerting-rule groups.

| Alert | Group | SPEC-10 §5 condition |
|---|---|---|
| `QueryLatencyP95HighPerTenant` | `ragctl.slo` | query p95 per tenant > 800 ms for 10 min |
| `GroundedRateDropPerTenant` | `ragctl.slo` | grounded rate per tenant drops > 20 points day-over-day |
| `JobFailuresHighPerKind` | `ragctl.jobs` | job failures per kind > 5 in 15 min |
| `JobQueueDepthHigh` | `ragctl.jobs` | queue depth > 500 for 30 min |
| `ProviderErrorRateHigh` | `ragctl.providers` | provider error rate > 5 % for 5 min |

Each alert carries a `severity` label, `summary`/`description` annotations, and a
`runbook_url` (AC: runbook link per alert). The runbook targets live at
`docs/runbooks/observability.md#<anchor>`, authored by STORY-10.8; the links are
stable now so no alert changes when the runbooks land.

## Loading

Point Prometheus at the file:

```yaml
# prometheus.yml
rule_files:
  - /etc/prometheus/rules/ragctl.rules.yml
```

Scrape both service planes so every referenced metric is present: `ragctl serve`
(API, retrieval, LLM/rerank provider metrics) and `ragctl work`
(`RAGCTL_WORKER_METRICS_ADDR`, default `:9091` — jobs, ingestion, embedding
provider metrics).

## Validation

`internal/obs/alertrules_test.go` parses this file and asserts every alert is
complete (name / expr / severity / summary / runbook link) and references a metric
`internal/obs` actually registers — so a renamed or mistyped metric that would make
an alert silently never fire fails the build. It runs under `mise run test` /
`mise run coverage`; no `promtool` dependency (a plain `yaml.v3` parse, already in
the module graph).

## Not yet covered

SPEC-10 §5 also lists a **tenant migration mismatch count > 0** alert, but the §2
catalogue defines no such metric and STORY-10.1 registered none. Rather than point
an alert at a non-existent series, that alert is deferred until a
`tenant_schema_mismatch` gauge is added to the catalogue (a fleet schema-version
scan) — see ISSUE-0045 and the backlog note.
