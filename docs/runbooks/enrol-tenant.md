# Runbook: Enrol a tenant

**Traces:** FR-TEN-01, SPEC-01 §6, SPEC-02. **Command:** `ragctl enroll`.

Enrolling creates a tenant's dedicated Postgres database and role (C-1 / ADR-0001),
seals its generated password with the platform DEK (SPEC-09 §2), applies the tenant
schema, and registers the tenant in the control plane. After enrol the resolver can
open the tenant on demand within one cache TTL.

## Preconditions

- `PROVISION_DB_URL` (a privileged/superuser connection that can `CREATE DATABASE` and
  `CREATE ROLE`), or `CONTROL_PLANE_URL` if it doubles as the provisioning target — the
  same resolution `ragctl` uses elsewhere.
- The DEK is available (`KMS_PROVIDER` and the key env the other `ragctl` commands use),
  so the generated tenant password is sealed with a ciphertext the resolver can open.
- The target Postgres host has `pgvector` available (retrieval lives in the tenant DB,
  ADR-0004) and capacity for another database (single-region per tenant, C-5).

## Procedure

1. **Enrol:**
   ```
   ragctl enroll --slug <slug> --name "<Display Name>"
   ```
   This provisions the database + per-tenant role, seals the password, applies the tenant
   migrations to the current expected schema version, and inserts the registry row.
2. **Verify the tenant resolves** — a query/admin call for the tenant should now succeed
   (the resolver opens it read-write once status is `active`). A schema-version mismatch
   fails closed (`ErrSchemaOutdated`) — run [Failed migration](failed-migration.md) if so.
3. **Provision access:** mint the tenant's API keys with the scopes it needs
   (`query` / `ingest` / `admin`) via the admin API; keys are shown once.
4. **Add sources:** create the tenant's sources (upload / web_crawl / sitemap / api) and
   let them sync on schedule (SRS §8.2).

## Notes

- Enrol is idempotent-ish per slug: re-enrolling an existing slug is rejected, not silently
  overwritten — pick a fresh slug or use `ragctl tenant` lifecycle commands to change an
  existing tenant.
- To place a tenant on a specific Postgres host (e.g. a separate instance for residency,
  SRS §8.1), point the provisioning host/port config at it before enrol; to relocate an
  existing tenant, use [Move a tenant](move-tenant.md).
- The embedding dimension is fixed at provisioning (SPEC-03 §5); changing it later is a
  reindex with table swap, not an in-place edit.
