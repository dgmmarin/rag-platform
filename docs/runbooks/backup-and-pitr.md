# Runbook: Tenant database backups and point-in-time recovery

**Traces:** NFR-REL-03, ADR-0068. **Config:** `deploy/backup/`. **Drill:** `mise run backup-drill`.

Every tenant Postgres cluster is backed up with **pgBackRest** to the platform
**MinIO/S3** object store: a daily full backup plus continuous WAL archiving,
retained **7 days** for point-in-time recovery (NFR-REL-03). Because each tenant has
a dedicated database (C-1 / ADR-0001) and pgBackRest works at the cluster/WAL level,
a cluster's backup covers every tenant database on it; one stanza per cluster (see
`deploy/backup/README.md`).

## When to use

- A tenant database is lost, corrupted, or a bad migration/write must be rewound.
- The scheduled **monthly restore drill** (proves backups are restorable, not just present).
- Verifying the 7-day PITR window after a config change.

## Preconditions

- The pgBackRest repo for the cluster is configured and reachable (S3/MinIO creds via
  `PGBACKREST_REPO1_S3_*`), and a stanza exists (`pgbackrest --stanza=<cluster> info`).
- You are on the Postgres host / in the Postgres container (pgBackRest + `pg_ctl` present).
- For a real recovery you have a maintenance window; for the drill you restore to a
  THROWAWAY path/port and never touch the live cluster.

## Backup health check

```
pgbackrest --stanza=<cluster> info      # last full/incr, WAL archive range, sizes
pgbackrest --stanza=<cluster> check     # archive_command + repo round-trip
```

`info` must show a full backup within the last day and a continuous WAL range covering the
last 7 days. If `check` fails, WAL is not reaching the repo — investigate `archive_command`
and the S3 creds before anything ages out.

## Monthly restore drill (verification)

Run `mise run backup-drill` (wraps `deploy/backup/restore-drill.sh`). It restores the stanza
to a throwaway target, starts it on a scratch port, and asserts recovery
(`pg_controldata` + `SELECT 1` + `pg_is_in_recovery() = f`), then tears the target down.
It runs the real drill where pgBackRest + Postgres exist and self-skips (exit 0) elsewhere.
To drill a specific point in time:

```
STANZA=<cluster> TARGET_TIME='2026-09-08 09:30:00+00' bash deploy/backup/restore-drill.sh
```

Record the drill outcome (pass/skip) in the ops log each month.

## Real point-in-time recovery

1. **Stop application traffic** to the affected tenant(s) — suspend the tenant(s) on the
   cluster (`ragctl tenant suspend --slug <slug>`) so no writes race the restore.
2. **Stop Postgres** on the target cluster.
3. **Restore** to the recovery point (omit `--type=time` to restore to the latest):
   ```
   pgbackrest --stanza=<cluster> --type=time \
     --target="YYYY-MM-DD HH:MM:SS+00" --target-action=promote restore
   ```
   (Restore into the cluster's data dir, or into a scratch dir first to validate — see the
   drill.)
4. **Start Postgres**; it replays WAL to the target and promotes. Confirm with
   `SELECT pg_is_in_recovery()` → `f` and `pg_controldata`.
5. **Verify tenant data** (row counts / a known record) before resuming traffic.
6. **Resume** the tenant(s) (`ragctl tenant resume --slug <slug>`).
7. **Re-establish backups:** take a fresh full backup so the new timeline is protected, and
   confirm WAL archiving resumed (`pgbackrest --stanza=<cluster> check`).

## Notes

- **Retention window:** recovery is only possible within the last 7 days
  (`repo1-retention-full=7`, time-based). Raise it in `pgbackrest.conf` for a longer window.
- **Control-plane DB:** NFR-REL-03 names tenant databases; back up the control-plane cluster
  with the same stanza pattern (its own stanza + repo path) if it is not already covered.
- **Config:** `deploy/backup/README.md` documents the stanza/fleet model, credentials, and the
  compose overlay used to exercise this locally.
