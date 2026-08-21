# Architecture decision records

| ADR | Title | Status |
|---|---|---|
| [0001](0001-database-per-tenant.md) | One database per tenant | Accepted |
| [0002](0002-go-primary-language.md) | Go as the primary implementation language | Accepted |
| [0003](0003-explicit-tenantdb-handle.md) | Explicit TenantDB handle instead of pool-in-context | Accepted |
| [0004](0004-pgvector-for-vector-storage.md) | pgvector in the tenant database for vector storage | Accepted |
| [0005](0005-river-job-queue.md) | Postgres-backed job queue (River) | Accepted |
| [0006](0006-python-parsing-sidecar.md) | Python sidecar for document parsing | Accepted |
| [0007](0007-hybrid-retrieval-rrf.md) | Hybrid retrieval with reciprocal rank fusion, optional rerank | Accepted |
| [0008](0008-document-versioning-atomic-swap.md) | Document versions with atomic current-version swap | Accepted |

Template for new ADRs: `NNNN-short-title.md` with sections Status, Context, Options, Decision, Consequences. Reference requirement IDs from the SRS.
