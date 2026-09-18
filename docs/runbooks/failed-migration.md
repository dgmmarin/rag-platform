# Runbook: A tenant migration failed mid-way

**Traces:** SPEC-01 §7, SRS §8.6. **Command:** `ragctl migrate tenants`.

Tenant schema migrations run per tenant with goose (STORY-02.2). The running binary
requires a specific expected tenant schema version; the resolver **fails closed**
(`ErrSchemaOutdated`) against any tenant behind it, so a half-migrated fleet refuses to
serve stale schemas rather than corrupting data. Migrations are resumable: re-running
applies only the remainder.

## Symptoms

- `ragctl migrate tenants` exited non-zero, or was interrupted, part-way through the fleet.
- Queries/admin calls for some tenants fail with a schema-outdated error while others work.
- A newly-enrolled or newly-migrated tenant will not resolve.

## Procedure

1. **Assess** which tenants are behind. Each tenant's registry row records its
   `schema_version`; compare to the binary's expected version. The ones below it are the
   remainder.
2. **Do not roll the binary forward past a partially-migrated fleet** expecting it to serve
   — the fail-closed guard is protecting you. Fix the migration first.
3. **Investigate the failing tenant.** Migrations are per tenant, so one tenant's failure
   (e.g. a data-dependent migration, a lock, low disk) does not corrupt the others. Read the
   migrate output / tenant Postgres logs for that tenant's error.
4. **Resume** — re-run the same command; already-migrated tenants are no-ops and it picks up
   the remainder (idempotent/resumable, the SRS §8.6 "failed mid-way and resumed" AC):
   ```
   ragctl migrate tenants
   ```
   To target a single recovered tenant, migrate just it (per the command's tenant selector)
   before re-running the fleet.
5. **Verify** every tenant is at the expected version and resolves (a query/admin call
   succeeds for a previously-failing tenant).

## Fleet schema-mismatch alert

The `TenantSchemaMismatch` alert (SPEC-10 §5) fires when `tenant_schema_mismatch > 0`: one or
more active tenants have a `schema_version` behind the running binary's expected tenant migration
version. The worker refreshes this gauge every 60 s from the control-plane registry (no tenant DB
read). Requests to a behind tenant fail closed with `ErrSchemaOutdated` (SPEC-01 §7) until it is
migrated.

To clear it: run the **Procedure** above (`ragctl migrate tenants`), then confirm the gauge
returns to 0 on the next scrape. A non-zero gauge after a full migrate run means a tenant failed
to apply — check the command's per-tenant output and the failed-migration steps above.

## Notes

- A migration that fails because of bad tenant *data* needs the data fixed (or the migration
  made tolerant) before it can apply — resuming alone will keep failing on that tenant while
  the rest proceed.
- If a migration left a tenant's data damaged (not just unapplied), recover it from backup:
  see [Backups and PITR](backup-and-pitr.md).
- The expected version is derived from the embedded migrations, so it cannot silently drift
  from what `ragctl migrate tenants` applies (SPEC-01 §7).
