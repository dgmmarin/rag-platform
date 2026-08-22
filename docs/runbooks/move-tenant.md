# Runbook: Move a tenant to a different database/host

**Traces:** FR-TEN-07, SPEC-01 §4/§8. **Command:** `ragctl tenant move`.

Moving a tenant means repointing its control-plane connection record
(`tenant_databases`) at a new host, database, role, or password. Because each
tenant has a dedicated database (C-1 / ADR-0001), a move is a data relocation
followed by a registry update — the platform does not copy data for you. This
runbook covers the operator-driven copy and the registry repoint.

The registry change fires the `tenant_changed` trigger, so every running resolver
evicts the tenant's pool and rebuilds against the new connection within ~1s
(SPEC-01 §3/§4). No code change and no restart is required.

## When to use

- Rebalancing a tenant onto a less-loaded Postgres host.
- Migrating a tenant to a new region for data residency (C-5).
- Rotating a tenant's database role/password.
- Replacing failing storage under a tenant.

## Preconditions

- You can reach the control plane and both the old and new Postgres hosts.
- The DEK is available to `ragctl` (same key the resolver uses), so a rotated
  password is sealed with a ciphertext the resolver can open (SPEC-09 §2). Set
  the usual `KMS_PROVIDER` / key env the other `ragctl` commands use.
- `PROVISION_DB_URL` (or `CONTROL_PLANE_URL`) points at the control plane.

## Procedure

### 1. (Recommended) Suspend the tenant to quiesce writes

A move is safest when the tenant is not being written to, so the copy is
consistent and no writes are lost in the cutover window.

```
ragctl tenant suspend --slug <slug>
```

Suspended tenants resolve read-only for end users (SPEC-01 §3); admins can still
read. Skip this only if you are doing a live logical replication cutover.

### 2. Provision the destination and copy the data (out of band)

The platform does **not** copy tenant data during a move. Create the destination
database and role, then restore the tenant database into it. Preserve the schema
exactly (the resolver enforces `schema_version == expected` on `Open`, SPEC-01
§7). Typical approaches:

- **Dump/restore:** `pg_dump` the current tenant database and `pg_restore` into a
  freshly created database on the new host, owned by the new least-privilege
  role.
- **Physical/logical replication:** stand up a replica on the new host and
  promote it during the cutover.

Create the destination role with least privilege (mirroring provisioning):

```sql
CREATE ROLE <new_role> LOGIN PASSWORD '<new_password>'
  NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE DATABASE <new_db> OWNER <new_role>;
-- then restore the tenant dump into <new_db>
```

Verify the restored database has the expected `goose_db_version` and the
`vector`/`pgcrypto`/`pg_trgm` extensions.

### 3. Repoint the registry with `ragctl tenant move`

Update only the fields that change. Anything you omit is left as-is.

```
# Move host + database + role + password:
ragctl tenant move \
  --slug <slug> \
  --db-host <new_host> \
  --db-port <new_port> \
  --db-name <new_db> \
  --db-user <new_role> \
  --db-ssl-mode require \
  --db-password "<new_password>"
```

Prefer passing the password via the `DB_PASSWORD` environment variable rather
than a flag, so it does not land in your shell history:

```
DB_PASSWORD="<new_password>" ragctl tenant move --slug <slug> \
  --db-host <new_host> --db-name <new_db> --db-user <new_role>
```

Rotate only the password (same host/db):

```
DB_PASSWORD="<new_password>" ragctl tenant move --slug <slug>
```

The command re-encrypts a supplied password with the platform DEK, writes the
new record in one transaction, and records a `tenant.move` audit event with the
changed connection metadata (never the password). It requires at least one field
to change and rejects an empty request (fail closed).

### 4. Resume and verify

```
ragctl tenant resume --slug <slug>
```

Confirm the resolver is now serving from the new connection:

- The next query/ingest for the tenant succeeds within ~1s of the move (pool
  rebuilt against the new connection).
- `select current_database()` through a tenant handle returns the new database.
- The `tenant.move` audit row is present:

```sql
select action, details, created_at
from audit_log a join tenants t on t.id = a.tenant_id
where t.slug = '<slug>' and a.action = 'tenant.move'
order by created_at desc limit 1;
```

### 5. Decommission the old database

Once traffic is confirmed on the new host and you have a retention window of
backups, drop the old database and role on the **old** host:

```sql
DROP DATABASE IF EXISTS <old_db> WITH (FORCE);
DROP ROLE IF EXISTS <old_role>;
```

## Rollback

If the destination is wrong or the new credentials do not work, run
`ragctl tenant move` again pointing back at the original host/db/role (and its
password). The old database still exists until step 5, so rollback is just
another repoint. Keep the tenant suspended until the correct connection serves
reads.

## Notes and gotchas

- **Schema version must match.** If the restored database is behind, `Open`
  fails closed with `ErrSchemaOutdated`; run `ragctl migrate tenants --tenant
  <slug>` against the new connection before resuming.
- **Password/DEK symmetry.** The move seals the password with whatever DEK
  `ragctl` loads; the resolver must load the same key version, or it cannot
  decrypt and pool creation fails. Rotate the DEK separately (STORY-10.4) — not
  as part of a move.
- **No cross-database references.** IDs copied into the tenant database are
  informational (SPEC-03 §2); a move does not touch the control plane's own IDs.
- **HTTP route.** `PATCH /admin/tenants/{id}` (the FR-TEN-07 API form) lands in
  EPIC-04 (STORY-04.6); until then `ragctl tenant move` is the supported path
  and calls the same `Lifecycle.Move` handler.
