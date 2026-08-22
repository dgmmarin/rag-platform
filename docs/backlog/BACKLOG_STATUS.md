# Delivery backlog — epics & stories status

Expanded from `backlog_import.csv`. Epic and story readiness only; the task-level
breakdown lives in [`BACKLOG_TASKS.md`](BACKLOG_TASKS.md). Full narrative in
[`BACKLOG.md`](BACKLOG.md).

**Legend**
- ✅ **Done** — implemented, unit + e2e tests green, lint clean. (EPIC-01 work is currently uncommitted, pending review.)
- 🚧 **In progress** — some stories delivered, epic not yet complete.
- 🔲 **Todo** — not started.

## Summary by epic

| Epic | Title | Points | Done pts | Status |
|---|---|--:|--:|---|
| EPIC-01 | Project foundation | 21 | 21 | ✅ Complete |
| EPIC-02 | Tenancy core | 34 | 26 | 🚧 In progress |
| EPIC-03 | Control plane services | 34 | 0 | 🔲 Todo |
| EPIC-04 | Public API surface | 21 | 0 | 🔲 Todo |
| EPIC-05 | Ingestion pipeline | 42 | 0 | 🔲 Todo |
| EPIC-06 | Connector framework and upload connector | 13 | 0 | 🔲 Todo |
| EPIC-07 | Web crawl, sitemap and API connectors | 39 | 0 | 🔲 Todo |
| EPIC-08 | Retrieval and answering | 39 | 0 | 🔲 Todo |
| EPIC-09 | Jobs, scheduling and maintenance | 21 | 0 | 🔲 Todo |
| EPIC-10 | Security, observability, operations | 26 | 0 | 🔲 Todo |
| EPIC-11 | Admin UI (reference) | 34 | 0 | 🔲 Todo |
| EPIC-12 | Evaluation harness and quality | 13 | 0 | 🔲 Todo |
| **Total** | | **337** | **47** | **14%** |

---

## EPIC-01 · Project foundation — ✅ 21/21 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-01.1 | Repository skeleton and build | 3 | ✅ Done | ADR-0002 |
| STORY-01.2 | Local development stack | 3 | ✅ Done | — |
| STORY-01.3 | CI pipeline | 5 | ✅ Done | NFR-MNT-03 |
| STORY-01.4 | Configuration and secrets loading | 3 | ✅ Done | SPEC-09 §2 |
| STORY-01.5 | Control-plane migrations tooling | 2 | ✅ Done | SPEC-02 |
| STORY-01.6 | Logging, metrics, tracing scaffolding | 5 | ✅ Done | FR-OBS-01/02/03, SPEC-10 |

**Delivered:** `internal/{cli,config,crypto,migrate,obs}`, `cmd/ragctl`; Docker/compose local
stack + seed; goose control-plane migration with drift guard; AES-256-GCM envelope crypto with
age/AWS KMS; slog/metrics/tracing scaffolding and `serve` HTTP skeleton; GitHub Actions CI driving
mise tasks with a config-driven 70% coverage gate. ADRs 0010–0014.

## EPIC-02 · Tenancy core — 🚧 26/34 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-02.1 | Tenant registry and resolver | 8 | ✅ Done | FR-TEN-03, FR-ACC-03, SPEC-01 §2–4, ADR-0003 |
| STORY-02.2 | Tenant schema migrations | 5 | ✅ Done | FR-TEN-09, SPEC-01 §7, ADR-0015 |
| STORY-02.3 | Tenant provisioning job | 8 | ✅ Done | FR-TEN-01/02, SPEC-01 §6, ADR-0016 |
| STORY-02.4 | Tenant suspension, deletion and grace period | 5 | ✅ Done | FR-TEN-04/05, SPEC-01 §8, ADR-0017 |
| STORY-02.5 | Tenant move (connection update) | 3 | 🔲 Todo | FR-TEN-07 |
| STORY-02.6 | Isolation test suite | 5 | 🔲 Todo | NFR-SEC-01, SPEC-01 §9 |

**Delivered (STORY-02.1):** `internal/tenant` — `DB` handle (the only tenant-SQL entry point, ADR-0003),
`Resolver.Open` applying the SPEC-01 §3 status rules (rw active / ro suspended / errors otherwise) with
fail-closed schema-version check, a 30s TTL-cached registry, and a lazy LRU pool cache with idle eviction
(SPEC-01 §4). Cache invalidation is driven by a `tenant_changed` LISTEN/NOTIFY loop backed by a new control
migration (`00002`) so suspension/move take effect within ~1s. Tenant identity in context is
observability-only. Unit tests + e2e golden path over the real Postgres (per-tenant role/database,
rw/ro/error resolution, and NOTIFY-driven invalidation). Coverage gate on `internal/tenant` met.

**Delivered (STORY-02.2):** `ragctl migrate tenants` — a parallel per-tenant goose runner in
`internal/migrate` (`tenant.go`, `tenant/00001_initial_schema.sql`) sharing the STORY-01.5 goose plumbing
and drift guard. One goose `Provider` per tenant (safe for the parallel fan-out); each tenant handled in a
single control-plane transaction that locks its `tenant_databases` row `FOR UPDATE SKIP LOCKED`, applies
pending migrations with the per-tenant role, and mirrors the version into `schema_version`. The `vector(N)`
dimension is a placeholder substituted per tenant from `settings.embedding_dim` via an in-memory FS; a
non-positive dimension fails closed. Failures are recorded and non-fatal — the command exits non-zero
listing failed slugs and a rerun resumes only those behind. The resolver's placeholder
`expectedSchemaVersion` is now derived from the embedded migrations (`migrate.ExpectedTenantVersion()`), so
`Open` fail-closed tracks what the runner applies. Extensions moved to provisioning (superuser) per SPEC-01
§6, since tenant migrations run as the least-privilege role. Unit tests (drift guard, placeholder
substitution, version derivation, input validation) + e2e golden path over the real Postgres including a
deliberately-failing tenant and a resuming rerun. ADR-0015.

**Delivered (STORY-02.3):** `internal/provision` — an idempotent tenant provisioner plus `ragctl enroll`.
A privileged (superuser) connection (`PROVISION_DB_URL`, falling back to the control-plane URL) creates the
least-privilege per-tenant role (`NOSUPERUSER NOCREATEDB NOCREATEROLE`) and its owned database, then installs
the three required extensions (`vector`, `pgcrypto`, `pg_trgm`) inside the new database — the superuser-only
step SPEC-01 §6/ADR-0015 assign to provisioning. The generated password is envelope-encrypted with the
platform `crypto.Cipher` (same DEK the resolver decrypts with, SPEC-09 §2) and recorded in `tenant_databases`;
migrations are applied via the STORY-02.2 runner (as the per-tenant role); the tenant is set active and a
`tenant.create`/`tenant.provision` audit event written in one control-plane transaction (never logging the
password). DDL identifiers are validated against a strict lowercase pattern and rejected if unsafe
(injection-proof). Re-running is idempotent: existing row/role/database are reused (the password is not
regenerated), so a retry — and the future River `provision_tenant` job (ADR-0005) — is safe. The async enqueue
is deferred to EPIC-09; enroll runs the same handler synchronously until then. Unit tests (identifier quoting,
password generation, SQL builders, validation/defaults, URL rewrite) + e2e golden path over the real Postgres
asserting role/db/extensions exist, migrations applied at the configured embedding dimension, status active,
password round-trip (decrypt + login as the role), and idempotent re-run. ADR-0016.

## EPIC-03 · Control plane services — 🔲 0/34 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-03.1 | User accounts and sessions | 5 | 🔲 Todo | FR-ACC-01, SPEC-09 §3 |
| STORY-03.2 | OIDC login | 5 | 🔲 Todo | FR-ACC-01 |
| STORY-03.3 | Tenant membership and roles | 5 | 🔲 Todo | FR-ACC-02/06, SPEC-02 §4 |
| STORY-03.4 | API keys | 5 | 🔲 Todo | FR-ACC-04/05 |
| STORY-03.5 | Tenant settings with JSON-schema validation | 3 | 🔲 Todo | FR-TEN-08, SPEC-02 §5 |
| STORY-03.6 | Audit log | 3 | 🔲 Todo | FR-ADM-05, SPEC-02 §6 |
| STORY-03.7 | Usage counters | 3 | 🔲 Todo | FR-ADM-06, SPEC-10 §6 |
| STORY-03.8 | Platform admin impersonation | 3 | 🔲 Todo | FR-ACC-07 |
| STORY-03.9 | Rate limiting | 2 | 🔲 Todo | NFR-SEC-07, SPEC-07 §1 |

## EPIC-04 · Public API surface — 🔲 0/21 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-04.1 | HTTP server, routing, middleware chain | 5 | 🔲 Todo | SPEC-07 §1 |
| STORY-04.2 | OpenAPI generation and contract tests | 5 | 🔲 Todo | SPEC-07 §3 |
| STORY-04.3 | Sources endpoints | 3 | 🔲 Todo | FR-SRC-01/14 |
| STORY-04.4 | Documents endpoints | 3 | 🔲 Todo | FR-SRC-02, FR-ADM-03 |
| STORY-04.5 | Jobs endpoints | 2 | 🔲 Todo | FR-ADM-02 |
| STORY-04.6 | Admin tenant endpoints | 3 | 🔲 Todo | FR-TEN-01/05/07 |

## EPIC-05 · Ingestion pipeline — 🔲 0/42 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-05.1 | Document and version store | 5 | 🔲 Todo | FR-ING-02, ADR-0008, SPEC-03 |
| STORY-05.2 | Go parsers: HTML, Markdown, text, CSV, JSON | 5 | 🔲 Todo | FR-ING-01, SPEC-05 §2 |
| STORY-05.3 | Parsing sidecar (Python) and Go client | 8 | 🔲 Todo | FR-ING-11, ADR-0006 |
| STORY-05.4 | Structure-aware chunker | 5 | 🔲 Todo | FR-ING-03/04, SPEC-05 §3 |
| STORY-05.5 | Embedding provider interface and implementations | 5 | 🔲 Todo | FR-ING-05, NFR-MNT-02 |
| STORY-05.6 | Sink implementation and commit semantics | 5 | 🔲 Todo | FR-ING-07, NFR-REL-02, SPEC-05 §5 |
| STORY-05.7 | Job stats and error capture | 2 | 🔲 Todo | FR-ING-10 |
| STORY-05.8 | Reindex job with table swap | 5 | 🔲 Todo | FR-ING-09, SPEC-03 §5, SPEC-05 §7 |
| STORY-05.9 | Garbage collection job | 2 | 🔲 Todo | SPEC-03 §4 |

## EPIC-06 · Connector framework and upload connector — 🔲 0/13 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-06.1 | Connector interface, registry, config validation | 5 | 🔲 Todo | FR-SRC-13, NFR-MNT-01, SPEC-04 §1 |
| STORY-06.2 | Credential encryption and handling | 3 | 🔲 Todo | FR-SRC-10, SPEC-04 §6 |
| STORY-06.3 | Upload connector and ingest_document job | 5 | 🔲 Todo | FR-SRC-02, SPEC-04 §5 |

## EPIC-07 · Web crawl, sitemap and API connectors — 🔲 0/39 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-07.1 | Web crawler core | 8 | 🔲 Todo | FR-SRC-03/04, SPEC-04 §2 |
| STORY-07.2 | SSRF protection and egress rules | 3 | 🔲 Todo | NFR-SEC-04, SPEC-09 §4 |
| STORY-07.3 | HTML content extraction quality | 5 | 🔲 Todo | FR-SRC-05 |
| STORY-07.4 | Conditional fetch and change detection | 3 | 🔲 Todo | FR-ING-02 |
| STORY-07.5 | Sitemap connector | 3 | 🔲 Todo | FR-SRC-06 |
| STORY-07.6 | HTTP API connector: auth and pagination | 8 | 🔲 Todo | FR-SRC-07, SPEC-04 §4 |
| STORY-07.7 | HTTP API connector: templating and incremental sync | 5 | 🔲 Todo | FR-SRC-07/08 |
| STORY-07.8 | Source "test connection" for all kinds | 2 | 🔲 Todo | FR-SRC-14 |
| STORY-07.9 | Connector documentation | 2 | 🔲 Todo | — |

## EPIC-08 · Retrieval and answering — 🔲 0/39 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-08.1 | Hybrid retrieval query | 8 | 🔲 Todo | FR-RET-01/02/08, ADR-0007, SPEC-06 §2 |
| STORY-08.2 | Retrieve endpoint | 2 | 🔲 Todo | FR-RET-08 |
| STORY-08.3 | Reranker interface and providers | 5 | 🔲 Todo | FR-RET-03 |
| STORY-08.4 | LLM provider interface | 5 | 🔲 Todo | NFR-MNT-02, NFR-REL-04 |
| STORY-08.5 | Prompt assembly, citations and grounding refusal | 8 | 🔲 Todo | FR-RET-04/05, SPEC-06 §4–5 |
| STORY-08.6 | Query endpoint with streaming | 5 | 🔲 Todo | FR-RET-06, SPEC-06 §6 |
| STORY-08.7 | Conversation history and question rewrite | 3 | 🔲 Todo | FR-RET-07 |
| STORY-08.8 | Query log and feedback | 3 | 🔲 Todo | FR-RET-09/10 |

## EPIC-09 · Jobs, scheduling and maintenance — 🔲 0/21 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-09.1 | River integration and worker binary | 5 | 🔲 Todo | FR-ING-08, ADR-0005, SPEC-08 §1 |
| STORY-09.2 | Job status mirroring to `jobs` table | 3 | 🔲 Todo | FR-ADM-02, SPEC-08 §3 |
| STORY-09.3 | Scheduler for cron sources and daily GC | 5 | 🔲 Todo | FR-SRC-11, SPEC-08 §2 |
| STORY-09.4 | Cancellation and uniqueness | 3 | 🔲 Todo | SPEC-08 §4 |
| STORY-09.5 | Per-tenant concurrency caps and fairness | 3 | 🔲 Todo | — |
| STORY-09.6 | Delete-source job | 2 | 🔲 Todo | FR-SRC-12 |

## EPIC-10 · Security, observability, operations — 🔲 0/26 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-10.1 | Metrics catalogue and dashboards | 5 | 🔲 Todo | FR-OBS-02, SPEC-10 §2/5 |
| STORY-10.2 | Alert rules | 2 | 🔲 Todo | SPEC-10 §5 |
| STORY-10.3 | Distributed tracing end to end | 3 | 🔲 Todo | FR-OBS-03 |
| STORY-10.4 | DEK rotation command | 3 | 🔲 Todo | NFR-SEC-03, SPEC-09 §2 |
| STORY-10.5 | Backups and PITR verification | 3 | 🔲 Todo | NFR-REL-03 |
| STORY-10.6 | Security scanning in CI and dependency policy | 2 | 🔲 Todo | SPEC-09 §6 |
| STORY-10.7 | Load and isolation testing | 5 | 🔲 Todo | NFR-PERF-01, SRS §8 |
| STORY-10.8 | Runbooks | 3 | 🔲 Todo | — |

## EPIC-11 · Admin UI (reference) — 🔲 0/34 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-11.1 | App shell, auth, tenant switcher | 5 | 🔲 Todo | — |
| STORY-11.2 | Sources list/create/edit with per-kind forms and test-connection | 8 | 🔲 Todo | FR-ADM-01 |
| STORY-11.3 | Jobs list and detail with cancel | 5 | 🔲 Todo | FR-ADM-02 |
| STORY-11.4 | Documents and chunks browser | 5 | 🔲 Todo | FR-ADM-03 |
| STORY-11.5 | Members, API keys, settings pages | 5 | 🔲 Todo | — |
| STORY-11.6 | Query playground with citations and feedback | 3 | 🔲 Todo | — |
| STORY-11.7 | Platform admin: tenants list, enrol, suspend, delete | 3 | 🔲 Todo | — |

## EPIC-12 · Evaluation harness and quality — 🔲 0/13 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-12.1 | Eval cases CRUD and import (CSV) | 3 | 🔲 Todo | FR-ADM-04 |
| STORY-12.2 | `ragctl eval run` with recall@k, grounded rate, latency | 5 | 🔲 Todo | SPEC-06 §8 |
| STORY-12.3 | LLM-as-judge correctness scoring (optional flag) | 3 | 🔲 Todo | — |
| STORY-12.4 | Eval report in admin UI and CI gate for settings changes | 2 | 🔲 Todo | — |

---

_Suggested next: STORY-02.5 (tenant move / connection update) — a `PATCH /admin/tenants/{id}` that updates a
tenant's DB connection and evicts its pool via `Resolver.Close`, plus a move-tenant runbook (FR-TEN-07). The
resolver's pool-cache eviction and LISTEN/NOTIFY invalidation from STORY-02.1 carry straight over. Then
STORY-02.6 (isolation test suite) closes out EPIC-02._
