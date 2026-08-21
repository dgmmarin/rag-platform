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
