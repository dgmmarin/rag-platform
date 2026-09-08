# Runbooks

Operational runbooks for the RAG platform (STORY-10.8). OKF markdown; each covers when
to use it, preconditions, the procedure, and verification.

| Runbook | Covers |
|---|---|
| [observability.md](observability.md) | Alert → action index (every Prometheus alert's `runbook_url` resolves here); metrics/dashboards/traces first response. |
| [enrol-tenant.md](enrol-tenant.md) | Enrol a new tenant (`ragctl enroll`). |
| [move-tenant.md](move-tenant.md) | Relocate a tenant to another database/host (`ragctl tenant move`). |
| [tenant-deletion.md](tenant-deletion.md) | Erase a tenant and verify no residue (`ragctl tenant delete`). |
| [failed-migration.md](failed-migration.md) | A tenant migration failed mid-way; resume (`ragctl migrate tenants`). |
| [provider-outage.md](provider-outage.md) | An LLM / embedding / rerank provider is failing. |
| [stuck-job.md](stuck-job.md) | A job is wedged, failing, or a queue is backing up. |
| [backup-and-pitr.md](backup-and-pitr.md) | Backups and point-in-time recovery (pgBackRest). |
| [incident-response.md](incident-response.md) | Entry point when the cause is unknown — triages to the above. |

The alert `runbook_url` anchors in `deploy/prometheus/rules/ragctl.rules.yml` are checked
against the headings in `observability.md` by `internal/obs` (see
`runbook_links_test.go`), so an alert and its runbook cannot silently drift.
