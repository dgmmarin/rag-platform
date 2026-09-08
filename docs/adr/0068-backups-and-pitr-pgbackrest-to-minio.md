# ADR-0068: Tenant-database backups and PITR via pgBackRest archiving to the platform MinIO/S3 object store

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** NFR-REL-03 · **Decisions:** ADR-0001 (one DB per tenant), C-5 (single-region per tenant)

## Context
NFR-REL-03: tenant databases SHALL be backed up daily with point-in-time recovery of at
least 7 days. No spec or ADR pinned a backup tool, a storage target, or a Postgres hosting
model. The committed stack is self-hosted Postgres (`pgvector/pgvector:pg16`) plus MinIO
(S3-compatible object storage); tenant databases are dedicated per tenant (ADR-0001) on
operator-run Postgres clusters, deployable single-region per tenant for data residency (C-5).

## Options / decisions
- **Backup tool = pgBackRest** (self-hosted). It is the de-facto standard for Postgres
  continuous archiving + PITR: per-stanza daily full backups plus WAL archiving via
  Postgres `archive_command`, time-based retention, and a one-command point-in-time restore
  (`pgbackrest restore --type=time`). Rejected alternatives: `pg_basebackup` + a hand-rolled
  `archive_command` (more DIY, weaker retention/verify ergonomics at fleet scale); Barman
  (comparable, but pgBackRest's S3 repo + bundling fit MinIO better).
- **Repository = the platform MinIO/S3 object store** (`repo1-type=s3`, path-style
  addressing). Reuses the object store already committed for document bytes and the same
  credential pattern (`MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD`); no second storage system is
  introduced. Rejected: a managed-Postgres provider's automated backups (RDS/Cloud SQL) —
  simpler where available, but it assumes a specific cloud provider the spec never names and
  does not match the self-hosted stack, and C-5 data-residency argues against baking in one
  provider. The pgBackRest S3 repo works against MinIO, AWS S3, or any S3-compatible store,
  so residency is a deployment choice, not a code change.
- **Retention = 7-day, time-based** (`repo1-retention-full-type=time`, `repo1-retention-full=7`):
  keep the full backups and the WAL needed to recover to any point in the last 7 days — the
  NFR-REL-03 floor. Operators raise the number for a longer window.
- **Stanza granularity = per Postgres cluster, not per database.** pgBackRest backs up a
  cluster and its WAL stream; PITR is cluster-wide. A cluster's daily full + archived WAL
  therefore covers *every* tenant database on it. One stanza per cluster: a single shared
  cluster is one stanza; tenants isolated onto dedicated clusters (C-5) each get their own
  stanza and repo path. This is how "backups for all tenant databases" is realised.
- **Wiring = an opt-in compose overlay, not the default dev stack.** `deploy/backup/`
  carries the config, a `Dockerfile.pgbackrest` (the base image + pgBackRest), and a
  `docker-compose.backup.yml` overlay that adds the `archive_command` and repo env to the
  base `postgres` service and reuses the base `minio`. The default `docker compose up`
  (and CI's e2e) is unchanged — enabling `archive_mode` with no repo would break plain dev —
  so the backup layer is loaded explicitly (`-f docker-compose.yml -f
  deploy/backup/docker-compose.backup.yml`). Production applies the same config to each
  tenant Postgres cluster.

## Consequences
- Every tenant Postgres cluster archives WAL continuously to MinIO/S3 and takes a daily full,
  giving ≥7-day PITR per NFR-REL-03; restore is a single `pgbackrest restore --type=time`.
- A monthly restore drill (`deploy/backup/restore-drill.sh`) proves the backups are
  restorable, not just present; it self-skips where pgBackRest/Postgres are absent so it is
  safe to invoke in CI, and runs for real where the tooling exists.
- No second storage system and no new Go dependency; the only new operational dependency is
  the pgBackRest binary in the Postgres image (installed by `Dockerfile.pgbackrest`).
- The `ragctl` binary is unchanged (ADR-0009): backups are infrastructure/ops, not a CLI
  subcommand — the drill is an ops script run on cadence, and the runbook documents it.
- Local MinIO over plain HTTP needs a TLS front (or `repo1-storage-verify-tls=n` against a
  TLS MinIO) for the S3 repo; production MinIO/S3 uses TLS. Documented in `deploy/backup/README.md`.
