# Tenant database backups and PITR

Backup configuration for the RAG platform's tenant Postgres databases (STORY-10.5,
NFR-REL-03, ADR-0068): **pgBackRest** archiving to the platform **MinIO/S3** object
store, daily full backups + continuous WAL archiving, retained **7 days** for
point-in-time recovery.

## Files

| File | Purpose |
|---|---|
| `pgbackrest.conf` | Non-secret backup policy: S3 repo, path-style, 7-day time retention, WAL archiving, the `[main]` stanza. Baked into the image; secrets come from env. |
| `Dockerfile.pgbackrest` | Base Postgres+pgvector image + the `pgbackrest` binary + its dirs + the config. |
| `docker-compose.backup.yml` | Opt-in overlay: adds `archive_command` and repo env to the base `postgres` service, reusing the base `minio` service. |
| `restore-drill.sh` | Monthly PITR restore drill (the verification). `mise run backup-drill`. |

## Stanzas and the tenant fleet

pgBackRest backs up a Postgres **cluster** and its WAL stream; PITR is cluster-wide.
A cluster's daily full + archived WAL therefore covers **every tenant database on that
cluster** (one DB per tenant, ADR-0001) with 7-day PITR. So "backups for all tenant
databases" is realised as **one stanza per cluster**:

- **Shared cluster** (local dev, small deployments): all tenant DBs live in one cluster →
  a single stanza (`[main]`).
- **Isolated clusters** (tenants pinned to dedicated clusters for residency, C-5): each
  cluster gets its own stanza and a distinct `repo1-path` so WAL never mixes. Add a section
  per cluster to `pgbackrest.conf` and point `archive_command --stanza=<name>` at it.

## Credentials (no secrets in the repo)

pgBackRest reads any option from a `PGBACKREST_<OPTION>` environment variable, so the S3
endpoint/bucket/region/key/secret are supplied as `PGBACKREST_REPO1_S3_*` — the overlay sets
them from the same MinIO creds the app uses (`MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD`).
Nothing secret is committed to `pgbackrest.conf`.

## Bring up with backups (local)

```
docker compose -f docker-compose.yml -f deploy/backup/docker-compose.backup.yml up -d
# one-time repo init:
docker compose exec postgres pgbackrest --stanza=main stanza-create
docker compose exec postgres pgbackrest --stanza=main check
docker compose exec postgres pgbackrest --stanza=main backup   # first full
```

The default `docker compose up` (no overlay) is unchanged — enabling `archive_mode` without a
repository would break plain dev, so the backup layer is explicit.

## Schedule (production)

Run on each tenant cluster via the operator's scheduler/cron (pgBackRest is designed to be
cron-driven):

- **Daily full:** `pgbackrest --stanza=<cluster> --type=full backup`
- **Intra-day incrementals (optional):** `... --type=incr backup`
- **WAL:** archived continuously by Postgres `archive_command` (set by the overlay /
  production Postgres config).
- **Retention:** `repo1-retention-full-type=time` + `repo1-retention-full=7` expires backups
  and WAL older than the 7-day PITR window automatically on each backup.

## Restore / PITR

See `../../docs/runbooks/backup-and-pitr.md`. The monthly drill (`restore-drill.sh`, via
`mise run backup-drill`) restores to a throwaway target and asserts recovery.

## TLS note

`repo1-s3-uri-style=path` addresses MinIO/S3. pgBackRest uses TLS for the S3 repo; production
MinIO/S3 must present a valid certificate. A local MinIO on plain HTTP needs a TLS front, or
set `PGBACKREST_REPO1_STORAGE_VERIFY_TLS=n` against a TLS-terminated MinIO for a local drill.
