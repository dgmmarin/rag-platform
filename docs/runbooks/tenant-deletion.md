# Runbook: Delete a tenant

**Traces:** FR-TEN-05, SPEC-01 §8, SPEC-09 §7. **Command:** `ragctl tenant delete`.

Deleting a tenant is a hard, GDPR-grade erase: the tenant's dedicated database and role
are dropped, its object-storage prefix `tenants/<id>/` is deleted, and the completion is
recorded in the audit log (SPEC-09 §7). Deletion is scheduled with a grace window so it
can be cancelled before it runs (ADR-0017).

## Preconditions

- Provisioning access (`PROVISION_DB_URL` / `CONTROL_PLANE_URL`) and the DEK, as for
  [enrol](enrol-tenant.md).
- Confirmed authorisation to erase this tenant's data (it is irreversible once run).

## Procedure

1. **Schedule** the deletion (stashes the prior status, sets the grace deadline):
   ```
   ragctl tenant delete --slug <slug>
   ```
   The tenant becomes unavailable to queries during the grace window. To abort:
   ```
   ragctl tenant delete --slug <slug> --cancel     # restores the prior active/suspended status
   ```
2. **Run** after the grace period (or immediately, if policy allows and you pass the run
   flag): this drops the database + role and deletes the object-storage prefix.
   ```
   ragctl tenant delete --slug <slug> --run
   ```
   Running before the grace deadline is refused — that guard is intentional.
3. **Evict** any cached pool: the registry change fires `tenant_changed`, so resolvers
   drop the tenant within ~1 s; no restart needed.

## Verify no residue (SRS §8.4)

After the run, confirm nothing remains:

- **Database/role:** `SELECT datname FROM pg_database WHERE datname = '<tenant_db>'` and the
  role query both return zero rows on the tenant's Postgres host.
- **Object storage:** listing the `tenants/<id>/` prefix in the object store returns empty.
- **Control plane:** the registry row is `deleted`; **no tenant content** was ever in the
  control plane (C-3), so only registry/audit rows exist, and the audit log carries the
  deletion with a completion timestamp (GDPR evidence).

## Notes

- Async River-driven deletion is a later refinement; today deletion runs synchronously
  through the lifecycle service (ADR-0016/0017).
- A tenant scheduled for deletion that is cancelled returns to its **prior** status
  (`active`/`suspended`), not unconditionally active.
