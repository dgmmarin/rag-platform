# Runbook: Incident response

**Traces:** SPEC-10 (observability), SRS §8. This is the entry point when something is
wrong and you are not yet sure what — it triages to the specific runbooks.

## 1. Detect and declare

- An alert fired (`deploy/prometheus/rules/ragctl.rules.yml`), or a user/operator reported
  an issue. Note the time, the alert(s), and the affected **tenant(s)** (most alerts are
  per-tenant or per-queue/provider).
- Set a rough severity: **SEV1** platform-wide or data-loss/exposure; **SEV2** one tenant or
  one subsystem degraded; **SEV3** minor/no user impact. Data exposure across tenants is
  always SEV1 (the tenant boundary is the database boundary, C-1).

## 2. Assess blast radius

- Open the dashboards (`deploy/grafana/dashboards/`): per-tenant (latency, grounded rate),
  jobs (failures, queue depth), providers (error rate), API (status codes, rate-limited).
- Pull the trace for a failing request (API → retrieval → provider) and the structured logs
  by `request_id` / `tenant_id` / `job_id` (SPEC-10 §1/§3). Logs never carry content at info
  level, so they are safe to share in an incident channel.
- Confirm isolation is intact: an incident must not leak one tenant's data into another's
  responses. If it might have, treat as SEV1 and preserve evidence.

## 3. Triage to a runbook

| Symptom | Runbook |
|---|---|
| Alert with a `runbook_url` | follow it → [observability](observability.md) section |
| A job wedged / queue backing up | [stuck job](stuck-job.md) |
| LLM / embedding / rerank failing | [provider outage](provider-outage.md) |
| Migration failed / tenant won't resolve | [failed migration](failed-migration.md) |
| Data lost/corrupted for a tenant | [backups and PITR](backup-and-pitr.md) |
| Tenant on the wrong/failing host | [move tenant](move-tenant.md) |
| Need to enrol / erase a tenant | [enrol](enrol-tenant.md) / [delete](tenant-deletion.md) |

## 4. Mitigate, then fix

- Prefer a reversible mitigation first (throttle a sync, scale workers, wait out a breaker,
  suspend a tenant to quiesce writes) over a risky change under pressure. Assess rollback
  before any irreversible action; escalate for anything whose rollback exceeds your role.
- Communicate status to stakeholders for SEV1/SEV2, including the degraded-but-safe modes
  (retrieval-only answers during a provider outage, NFR-REL-04).

## 5. Recover and close

- Confirm the alert cleared and the dashboards are healthy.
- Verify no tenant-isolation or data-loss residue (for data incidents, the deletion/PITR
  verification steps in [tenant deletion](tenant-deletion.md) / [backups](backup-and-pitr.md)).
- Record what happened, the timeline, and the root cause; file follow-ups (e.g. a dependency
  bump behind a `.ci/vuln-allowlist.txt` entry, a capacity change) as issues.
