# ADR-0016: Tenant provisioning — privileged connection, idempotent handler, synchronous enroll until River

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** FR-TEN-01/02, SPEC-01 §6, C-1/ADR-0001, C-4/ADR-0012, ADR-0005, ADR-0015, SPEC-09

## Context
STORY-02.3 implements tenant provisioning: create a dedicated least-privilege role and database for a tenant on the target Postgres, install the superuser-only extensions the tenant schema assumes, encrypt and record the connection details in the control plane, apply the tenant migrations, and set the tenant active (FR-TEN-01/02, SPEC-01 §6). ADR-0015 already established that extension creation is provisioning's job (superuser-only, cannot live in a tenant migration that runs as the least-privilege role); this story is where that lands. The STORY-02.2 runner (`internal/migrate`) already applies migrations per tenant.

Open sub-decisions:
1. **Which connection does the DDL.** `CREATE ROLE` / `CREATE DATABASE` / `CREATE EXTENSION` need superuser; the resolver/data plane must not.
2. **Async job vs. synchronous call.** SPEC-01 §6 says steps 2–4 run as a `provision_tenant` job (ADR-0005, River). River is EPIC-09 and not yet present.
3. **Idempotency/retry semantics** so a partially-failed provision can be re-run safely (a job is retried; `ragctl enroll` may be re-invoked).
4. **How the encrypt side matches the resolver's decrypt side** (SPEC-09 §2, C-4).

## Decision
- **A separate `internal/provision` package** holds the provisioner; adding it required no change to `internal/tenant` (which stays the data-plane-only path, ADR-0003). It depends on `internal/migrate` (reuse, not reimplement, the STORY-02.2 runner) and on the `crypto.Cipher` behaviour via small `Encrypter`/`Decrypter` interfaces.
- **A privileged (superuser) connection**, configured by `PROVISION_DB_URL` (falling back to the control-plane URL), does all DDL and the control-plane registry writes. Extensions are installed by re-pointing that same superuser URL at the new tenant database. The per-tenant least-privilege role is created `NOSUPERUSER NOCREATEDB NOCREATEROLE` and owns only its own database (C-1 isolation, SPEC-09). Tenant migrations then run as that role via the STORY-02.2 runner.
- **Injection-proof DDL.** Role/database names cannot be parameterised in DDL, so names are constrained to a plain lowercase identifier (`^[a-z_][a-z0-9_]{0,62}$`) and rejected — not escaped — if they do not match (fail closed). Names are derived from the sanitised slug plus a short suffix from the tenant UUID; the generated password is drawn from a DSN-safe alphabet and single-quote-escaped in the `CREATE ROLE ... PASSWORD` literal.
- **Idempotent handler.** Provisioning reuses an existing `tenants` row by slug and an existing `tenant_databases` row by tenant, so a retry does **not** regenerate the password (which would desync the stored ciphertext from the live role). It skips role/database creation when they already exist (`pg_roles`/`pg_database` checks; `CREATE DATABASE` has no `IF NOT EXISTS` and cannot run in a transaction), re-installs extensions with `IF NOT EXISTS`, re-applies pending migrations, and re-sets the tenant active. Activation and the audit write happen in one control-plane transaction.
- **Synchronous `ragctl enroll` now; async enqueue deferred to EPIC-09.** Because River is not yet present, `ragctl enroll` calls the idempotent handler directly. When the worker lands, `POST /admin/tenants` / `enroll` will enqueue a `provision_tenant` job whose handler is this same idempotent function (the `job_kind` enum already carries `provision_tenant`, SPEC-08 §1). No scope was invented; this is recorded as deferred rather than stubbed.
- **Encrypt matches decrypt.** The provisioner encrypts the generated password with the platform `crypto.Cipher` (same DEK generation the resolver loads at startup, ADR-0012), so the resolver's `Decrypter` opens it unchanged. The e2e proves the round trip by decrypting the stored ciphertext with the same DEK and logging in as the role.
- **Audit on provisioning** (C-3: control-plane only): an `audit_log` row is written with `tenant.create` (first provision) or `tenant.provision` (retry), carrying connection metadata only — never the password (SPEC-09 §2).

## Consequences
- Provisioning is safe to retry, which is the precondition for wiring it as a River job in EPIC-09 with no handler changes.
- The privileged connection is isolated to provisioning and never touches the data plane; the tenant role remains least-privilege.
- New config: `PROVISION_DB_URL`, `TENANT_DB_HOST`, `TENANT_DB_PORT`, `TENANT_DB_SSLMODE` (recorded for how the resolver reaches the tenant DB; `disable` is used locally where the cluster has no TLS). These are never logged.
- SPEC-01 §6 already describes this flow (it was updated in ADR-0015); no spec change was needed beyond confirming provisioning owns extension creation, which this story now implements.
- `Provision`'s DB orchestration is covered by the required integration/e2e test against the real stack rather than mocks; the pure helpers (identifier quoting, password generation, SQL builders, validation, URL rewrite) are unit-tested.
