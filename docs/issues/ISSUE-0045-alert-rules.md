# ISSUE-0045: Alert rules

**Type:** Feature · **Status:** Done (5/6 alerts; 6th deferred to ISSUE-0046) · **Story:** STORY-10.2 · **Traces:** SPEC-10 §5, ISSUE-0044

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs.

## Summary
Commits Prometheus alerting rules for the SPEC-10 §5 conditions, referencing the SPEC-10 §2
metric catalogue (STORY-10.1) and reusing the thresholds already annotated on the Grafana
dashboards so the two never drift. A hermetic Go test validates the rules parse and only
reference metrics `internal/obs` actually registers.

## Scope
- `deploy/prometheus/rules/ragctl.rules.yml`: five alerts in three groups —
  `QueryLatencyP95HighPerTenant`, `GroundedRateDropPerTenant` (`ragctl.slo`);
  `JobFailuresHighPerKind`, `JobQueueDepthHigh` (`ragctl.jobs`); `ProviderErrorRateHigh`
  (`ragctl.providers`). Each carries a `severity` label, `summary`/`description`, and a
  `runbook_url` (AC: runbook link per alert).
- `internal/obs/alertrules_test.go`: the runnable check — parses the YAML (`yaml.v3`,
  already in the module graph; no `promtool` dep) and asserts every alert is complete and
  references a real registered metric.
- `deploy/prometheus/README.md`: how to load the rules + validation + the deferred alert.
- Docs: this issue, SPEC-10 §5 note, backlog.

## Decisions
- **Artifact format = Prometheus alerting rules** (no new ADR). The committed metrics stack
  is Prometheus (`prometheus/client_golang`, `/metrics`) and the dashboards are Grafana over
  a Prometheus datasource, so a Prometheus rule file is the canonical fit — not a new
  infrastructure choice. It parallels how the dashboards are committed for the implied
  Grafana: no Alertmanager routing or running services are added, only the rule file the
  already-implied Prometheus loads. Placed under `deploy/prometheus/` alongside
  `deploy/grafana/`.

## Deferred alert (resolved: option A)
SPEC-10 §5 also lists **"Tenant migration mismatch count > 0"**, but the §2 catalogue
defines no such metric and STORY-10.1 registered none — a §5↔§2 gap. Decision (coordinator):
**defer** this one alert. It is tracked in **ISSUE-0046** (add a `tenant_schema_mismatch`
gauge — a periodic fleet scan comparing each tenant's `schema_version` to the expected
version — to the §2 catalogue, then add the alert). The other five alerts are fully settled
by SPEC and delivered here.

## Tests
- `internal/obs/alertrules_test.go`: rules parse; each alert has name/expr/severity/summary
  and a `runbook_url`; each expr references a metric `internal/obs` registers (histogram
  `_bucket`/`_count`/`_sum` families accepted); no duplicate alert names.
- Build/vet/gofmt green.

## Not in scope
- The tenant-migration-mismatch alert (deferred to ISSUE-0046).
- Alertmanager routing/receivers, and a running Prometheus/Alertmanager deployment.
- Runbook contents (`docs/runbooks/`) — STORY-10.8; the `runbook_url` targets are stable now.
