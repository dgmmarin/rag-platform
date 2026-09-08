# ISSUE-0047: Backups and PITR verification

**Type:** Feature · **Status:** Done · **Story:** STORY-10.5 · **Traces:** NFR-REL-03, ADR-0068

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs.

## Summary
Backs up every tenant Postgres cluster daily with ≥7-day point-in-time recovery (NFR-REL-03)
using pgBackRest archiving to the platform MinIO/S3 object store (ADR-0068), and proves the
backups are restorable with a monthly restore-drill script and a runbook.

## Scope
- `deploy/backup/pgbackrest.conf`: the backup configuration — S3 repo on MinIO (path-style),
  7-day time-based retention, WAL archiving, one stanza per Postgres cluster (covers every
  tenant DB on the cluster). Deliverable 1.
- `deploy/backup/Dockerfile.pgbackrest` + `deploy/backup/docker-compose.backup.yml`: an opt-in
  overlay that layers `archive_command` + the repo env onto the base `postgres` service and
  reuses the base `minio` service, leaving the default `docker compose up` untouched.
- `deploy/backup/restore-drill.sh`: the monthly PITR drill — restores a stanza to a throwaway
  target at a point in time, starts it, and asserts recovery (`pg_controldata` + `SELECT 1` +
  `pg_is_in_recovery() = f`). Deliverable 2 / the runnable check. Self-skips where
  pgBackRest/Postgres are absent. Invoked via `mise run backup-drill`.
- `deploy/backup/README.md`: documents the configuration, the stanza-per-cluster fleet model,
  init, schedule, retention, TLS note.
- `docs/runbooks/backup-and-pitr.md`: restore + PITR procedure and the monthly drill cadence.
  Deliverable 3.
- `mise-tasks/backup-drill` + a `backup-drill` CI job (mise-task-driven, ADR-0014).
- Docs: ADR-0068, this issue, backlog.

## Decisions
- pgBackRest → MinIO/S3, self-hosted, 7-day time-based retention, per-cluster stanza,
  daily full + WAL archiving — see ADR-0068 (records the managed-Postgres path as the
  rejected alternative and the C-5 residency rationale).

## Tests / runnable check
- `mise run backup-drill`: `bash -n` structural validation of `restore-drill.sh` (always
  runs, catches syntax errors) then executes it — the real PITR drill where pgBackRest +
  Postgres + a configured stanza exist, otherwise a clean SKIP (exit 0). Wired as a CI job so
  it is exercised on every PR. No `promtool`/`shellcheck` dependency required (shellcheck used
  if present); no new Go deps.
- `go build ./...` / `go vet ./...` / `gofmt` unaffected (no Go changed).

## Not in scope
- A running production Postgres/backup deployment (this commits config + drill + runbook).
- Automating the monthly cadence (documented; wired to the operator's scheduler/cron).
- Backup of the control-plane database (NFR-REL-03 names tenant databases; the same stanza
  pattern applies and is noted in the runbook).
