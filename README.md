# Multi-tenant company knowledge RAG platform — documentation package

Everything an agile team needs to start building, organised top-down so each layer traces to the one above.

```
docs/SRS.md                  Software Requirements Specification (FR-*, NFR-*, constraints, acceptance)
docs/specs/SPEC-01..10       Technical specifications (how each requirement group is realised)
docs/adr/0001..0008          Architecture decision records (why the specs look the way they do)
docs/backlog/BACKLOG.md      Epics and stories with acceptance criteria, estimates, traces
docs/backlog/backlog_import.csv  Same backlog as CSV for Jira / Linear / Azure DevOps import
schemas/control_plane.sql    Shared control-plane database schema
schemas/tenant.sql           Per-tenant (data-plane) database schema
```

## How to use
1. Review and approve the SRS; change IDs only by appending, never by renumbering.
2. Treat specs as living documents owned by the epic's engineers; update them in the same PR as the code.
3. Write a new ADR whenever a decision deviates from or extends an existing one.
4. Import the CSV into your tracker; re-slice stories into tasks during sprint planning.
5. Use the traceability matrix (SRS §7) and the `Traces` field on every story to check coverage before release.

## Local development stack

All commands go through [mise](https://mise.jdx.dev) tasks (`mise-tasks/`); the
`mise.toml` holds only tool pins. Local infrastructure is Docker
(`docker-compose.yml`); host processes are run with mprocs (`mprocs.yaml`).

### Prerequisites
- Docker + Docker Compose v2
- mise (`mise install` pulls the pinned Go, Python and mprocs)

### Bring the stack up
```bash
cp .env.example .env      # then edit ports if needed (see below)
mise run up               # Postgres 16 + pgvector, MinIO, parser sidecar — waits until healthy
```
`mise run up` runs `docker compose up -d --wait`. It starts:

| Service | Image / source | Default host port | Purpose |
|---|---|---|---|
| `postgres` | `pgvector/pgvector:pg16` | `5432` | control-plane + tenant DBs; pgvector installed by the seed |
| `minio` | `minio/minio` | `9000` (API), `9001` (console) | object storage |
| `parser` | `services/parser` (Python stub) | `8081` | parsing sidecar health stub (real one: STORY-05.3) |

The seed script (`deploy/seed/01-control-plane.sh`) runs once on first Postgres
init: it ensures the `control_plane` database exists and installs the `vector`
and `pgcrypto` extensions. The full control-plane schema is applied by
migrations (`ragctl migrate control`, STORY-01.5), not the seed.

### Overriding ports (collisions)
Every host port is parameterised in `docker-compose.yml` and documented in
`.env.example`. If `docker compose up` fails with "port is already allocated",
set the matching `*_PORT` in `.env`. `:9000` in particular commonly collides
with other local MinIO containers:
```bash
# in .env
MINIO_PORT=19000
MINIO_CONSOLE_PORT=19001
```
The e2e tests read the same env vars, so overrides are honoured end to end.

### Running the app
Normally the app runs on the host for fast rebuilds and a debugger:
```bash
mise run dev              # mprocs: infra (compose) + api/worker (once un-stubbed)
```
`ragctl serve` now starts the scaffolding HTTP server (STORY-01.6, SPEC-10):
after the fail-closed DEK check it serves `/healthz`, `/readyz` and `/metrics`
behind the observability middleware (JSON `slog` logs with `request_id`, the
`api_request_duration_seconds` histogram, an env-configured OpenTelemetry tracer
that is disabled unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set) and shuts down
gracefully on SIGINT/SIGTERM. Because it blocks, it runs on the host via `mprocs`
(or the opt-in `app` compose profile below), not as part of `docker compose up
--wait` (ADR-0011):
```bash
mise run up-app           # infra + built app image (docker compose --profile app)
```

### Other tasks
```bash
mise run test             # unit tests
mise run coverage         # unit tests + per-package coverage gate (70%); writes coverage/
mise run lint             # golangci-lint
mise run vuln             # govulncheck ./...
mise run e2e              # brings the stack up, asserts it is reachable (real services)
mise run logs             # tail infra logs
mise run down             # stop infra (keeps volumes; add -v manually to wipe)
```

## Continuous integration
CI (`.github/workflows/ci.yml`, traces NFR-MNT-03) runs on every pull request
and on pushes to `main`. Each job invokes the **same** `mise run <task>` a
developer runs locally, so there is nothing to reproduce by hand — run
`mise run lint`, `mise run coverage`, `mise run vuln`, and `mise run e2e` to see
exactly what CI sees.

| Job | Command | Notes |
|---|---|---|
| lint | `mise run lint` | golangci-lint (pinned) |
| coverage | `mise run coverage` | unit tests + 70% gate; uploads `coverage/` as an artifact |
| integration | `mise run e2e` | stands up the real compose stack (Postgres+pgvector, MinIO, parser) |
| vuln | `mise run vuln` | govulncheck; non-blocking while pinned to Go 1.22 (see ADR-0014) |
| image | `docker build -f Dockerfile .` | build only, no push |

**Coverage gate.** The gated packages live in `.ci/coverage-packages.txt`
(`internal/{tenant,ingest,retrieve,connector}`, threshold 70%). The gate logic
is a shell library (`mise-tasks/lib/coverage.sh`) shared by CI and local, and it
only enforces packages that already exist — packages listed but not yet created
are reported `SKIP`, so the gate is green today and starts enforcing
automatically as those packages land (ADR-0014). Its behaviour is TDD-covered by
`test/coverage/coverage_gate_test.sh`.

## Diagrams
Mermaid diagrams embedded in the docs (render natively on GitHub):

| Diagram | Type | Location |
|---|---|---|
| System architecture | flowchart | [SRS §2.1](docs/SRS.md#21-product-perspective) |
| Resolver flow | flowchart | [SPEC-01 §3](docs/specs/SPEC-01-tenancy-and-resolver.md#3-resolution-rules) |
| Control-plane ERD | erDiagram | [SPEC-02 §1](docs/specs/SPEC-02-control-plane.md#1-responsibilities) |
| Tenant data-model ERD | erDiagram | [SPEC-03 §1](docs/specs/SPEC-03-tenant-data-model.md#1-entities) |
| Ingestion pipeline | flowchart | [SPEC-05 §1](docs/specs/SPEC-05-ingestion-pipeline.md#1-flow-per-document) |
| Retrieval and answering | sequenceDiagram | [SPEC-06 §1](docs/specs/SPEC-06-retrieval-and-answering.md#1-pipeline) |
| Job lifecycle | stateDiagram | [SPEC-08 §3](docs/specs/SPEC-08-jobs-and-scheduling.md#3-status-mirroring) |

## Key decisions at a glance
- One PostgreSQL database per tenant, shared control plane (ADR-0001).
- Go services, Python parsing sidecar (ADR-0002, ADR-0006).
- Explicit `tenant.DB` handle, never a pool in context (ADR-0003).
- pgvector + full-text in the tenant DB, hybrid retrieval with RRF (ADR-0004, ADR-0007).
- River on Postgres for jobs (ADR-0005).
- Immutable document versions with atomic swap (ADR-0008).

## Suggested first sprint
EPIC-01 in full plus STORY-02.1 (resolver and TenantDB). That gives the team the skeleton, CI, local stack and the single most important abstraction before any feature work.
