# ADR-0015: Per-tenant migration runner — goose Provider, dimension placeholder, fail-closed version

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** FR-TEN-09, SPEC-01 §6/§7, C-1/ADR-0001, SPEC-09

## Context
STORY-02.2 delivers `ragctl migrate tenants`: apply the tenant schema to every tenant's dedicated database (C-1/ADR-0001), track each tenant's version, run in parallel, continue past per-tenant failures, exit non-zero listing them, and resume on rerun (FR-TEN-09, SPEC-01 §7). The resolver (STORY-02.1) already fails closed on a version mismatch (`ErrSchemaOutdated`) but carried a placeholder `expectedSchemaVersion = 1` constant that had to be replaced with the real, drift-proof value.

Three sub-decisions were open:
1. **Where the runner lives.** SPEC-01 §1 originally sketched `internal/tenant/migrate.go`.
2. **How one schema serves tenants with different embedding dimensions** (SPEC-01 §6/§7: substitute `vector(N)` from `tenants.settings.embedding_dim`).
3. **How the fail-closed expected version stays in sync** with the migrations actually shipped.

## Options
- **Runner location:** (a) in `internal/tenant`; (b) in `internal/migrate` next to the control-plane runner.
- **Migration engine for parallel runs:** (a) goose's global functions (`goose.SetBaseFS`/`goose.UpContext`) as the control runner uses; (b) goose's per-instance `Provider` (`goose.NewProvider`).
- **Dimension substitution:** (a) `text/template` the SQL; (b) a placeholder token replaced at read time via a substituting `fs.FS` so goose stays the only engine; (c) `ALTER TABLE ... TYPE vector(N)` after the fact.
- **Expected version:** (a) hand-maintained constant bumped per migration; (b) derived from the embedded migration files.

## Decision
- **Runner in `internal/migrate` (`tenant.go`, `tenant/*.sql`).** It shares the goose plumbing, the `upSection`/`normalizeSQL` drift-guard helpers, and the embed pattern with the STORY-01.5 control runner. The tenant package keeps no migration logic and imports only `migrate.ExpectedTenantVersion()`. SPEC-01 §1 is updated to match.
- **One goose `Provider` per tenant.** The global goose API keeps process-wide dialect/baseFS state, which is unsafe for the parallel fan-out. `NewProvider(dialect, db, fsys)` is per-instance and takes the `fs.FS`, which is exactly the seam the placeholder needs.
- **Placeholder token + substituting `fs.FS` (option b).** Migration 0001 uses `vector(EMBEDDING_DIM)`; the runner builds a small in-memory FS (`memFS`, no `testing/fstest` in the production build) whose contents have the token replaced with the tenant's dimension, then hands it to goose via `fs.Sub`. A non-positive dimension is rejected (fail closed). `schemas/tenant.sql` documents the shape at the default dimension (1536); `TestTenantSchemaMatchesMigrations` proves the two never drift.
- **Version derived from files (option b).** `ExpectedTenantVersion()` returns the highest embedded migration version; the resolver's `expectedSchemaVersion` is set from it at init. Adding a migration bumps the fail-closed check automatically.
- **Atomicity and resumability.** Each tenant is handled in one control-plane transaction: lock `tenant_databases FOR UPDATE SKIP LOCKED`, apply pending migrations against the tenant DB with its per-tenant role, mirror the resulting goose version into `schema_version`, commit. `SKIP LOCKED` makes a tenant already owned by a concurrent runner a clean skip, not a failure. Failures are collected, not fatal; the command exits non-zero with the failed slugs; a rerun resumes only tenants behind (successful ones are already at target).
- **Extensions are provisioning's job, not the migration's.** `CREATE EXTENSION vector/pgcrypto/pg_trgm` is superuser-only; tenant migrations run as the least-privilege per-tenant role (SPEC-09). Migration 0001 assumes the extensions exist; the privileged provisioning connection installs them at database-create time (SPEC-01 §6). The e2e test installs them as the superuser, standing in for STORY-02.3 provisioning.

## Consequences
- Adding a tenant migration is: drop a `internal/migrate/tenant/NNNN_*.sql` file (with `EMBEDDING_DIM` where a vector column is declared), update `schemas/tenant.sql` to match, and the drift test + auto-derived expected version keep everything honest. No constant to bump, no code change.
- The parallel runner shares no global engine state, so `--parallel` is safe.
- A crash mid-run cannot leave `schema_version` claiming a version the tenant DB never reached: the mirror write and the version-producing goose Up are ordered so `schema_version` only advances after a successful apply, and the resolver fails closed on any lag.
- Provisioning (STORY-02.3) must install the three extensions on the privileged connection; this is now written into SPEC-01 §6.
- Cross-DB nuance: goose's own `goose_db_version` is the source of truth inside each tenant DB; the control plane's `schema_version` is an informational mirror (consistent with C-4: no cross-database FKs).
