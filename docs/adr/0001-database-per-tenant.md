# ADR-0001: One database per tenant

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** C-1, FR-TEN-02, FR-TEN-05, FR-TEN-07, NFR-SEC-01, NFR-SCAL-02

## Context
The platform stores content for many companies. Tenant isolation is the primary security property; deletion, export and data residency must be simple to prove. Expected tenant count is tens to low hundreds (A-1).

## Options
1. Shared tables with `tenant_id` column and filter on every query.
2. Shared database, one schema (or vector collection) per tenant.
3. One database per tenant, with a shared control-plane database.

## Decision
Option 3. Every tenant gets a dedicated PostgreSQL database with an identical schema. A shared control-plane database stores the tenant registry, connection details, users, sources and job history, and never stores tenant content.

## Consequences
- Isolation is enforced by the connection, not by application filters. A missing `WHERE tenant_id` cannot leak data.
- Delete = drop database; export = dump; move = change connection string.
- Migrations run N times and must be tracked per tenant and resumable (FR-TEN-09).
- Per-tenant connection pools are required; pools are opened lazily and evicted when idle (SPEC-01).
- Cross-tenant reporting goes through the control plane or metrics, not SQL joins.
- Higher fixed cost per tenant; unsuitable for thousands of self-serve tenants — revisit if A-1 changes.
