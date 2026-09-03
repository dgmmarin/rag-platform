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
| [0009](0009-kong-cli-framework.md) | Kong for the CLI and platform entrypoint | Accepted |
| [0010](0010-ragctl-exit-codes-and-error-sentinels.md) | ragctl exit-code contract and error sentinels | Accepted |
| [0011](0011-local-app-container-profile.md) | Local app container is an opt-in compose profile | Accepted |
| [0012](0012-secret-envelope-encryption-and-kms.md) | Secret envelope encryption format and KMS providers | Accepted |
| [0013](0013-observability-scaffolding.md) | Observability scaffolding — obs package, private registry, no-op-by-default tracing | Accepted |
| [0014](0014-ci-drives-mise-tasks.md) | CI drives mise tasks; coverage gate in a shell lib, skips absent packages | Accepted |
| [0015](0015-tenant-migration-runner.md) | Per-tenant migration runner — goose Provider, dimension placeholder, fail-closed version | Accepted |
| [0016](0016-tenant-provisioning.md) | Tenant provisioning — privileged connection, idempotent handler, synchronous enroll until River | Accepted |
| [0017](0017-tenant-lifecycle-and-deletion.md) | Tenant suspension/deletion — grace deadline, tombstone-on-delete, deferred object storage | Accepted |
| [0018](0018-isolation-suite-unsafe-lint-and-connect-lockdown.md) | Isolation suite — forbidigo-enforced Unsafe() ban and PUBLIC-CONNECT lockdown per tenant database | Accepted |
| [0019](0019-password-auth-and-sessions.md) | Password authentication and server-side sessions — argon2id, hashed session tokens, double-submit CSRF | Accepted |
| [0020](0020-oidc-login-identity-linking-and-pkce-state.md) | OIDC login — identity-linking model, PKCE/state/nonce handling, and JIT provisioning | Accepted |
| [0021](0021-api-key-format-and-verification.md) | API key format, scopes, and verification — `rk_<hexprefix>_<secret>`, sha256-at-rest, SQL-side revocation | Accepted |
| [0022](0022-tenant-settings-schema-defaults-and-embedding-dim-immutability.md) | Tenant settings — embedded JSON Schema, defaults overlay, flat `embedding_dim` mirror for immutability | Accepted |
| [0023](0023-audit-log-read-api-and-platform-admin-scope.md) | Audit log — sanctioned `audit.Record` writer, tenant-scoped read API, tenant-less `RequirePlatformAdmin` middleware | Accepted |
| [0024](0024-usage-counters-in-memory-buffer-and-accumulating-upsert.md) | Usage counters — in-memory `usage.Counter`, 30 s accumulating-upsert flush, tenant-scoped `GET /v1/usage` | Accepted |
| [0025](0025-platform-admin-impersonation-grants.md) | Platform admin impersonation — explicit `impersonation_sessions` grant recording real admin + impersonated user, time-bounded, revocable, audited | Accepted |
| [0026](0026-rate-limiting-in-process-token-bucket.md) | Rate limiting — in-process token bucket per API key and per tenant, `settings.limits.qps`-driven, fail-closed `429` with `Retry-After`/`RateLimit-*` | Accepted |
| [0027](0027-public-router-composition-and-error-envelope.md) | Public router — dependency-injected `internal/api` package, credential-keyed rate-limiter-after-auth chain, and one SPEC-07 §1 error envelope across router-mounted middleware/handlers | Accepted |
| [0028](0028-openapi-generated-from-go-and-jsonschema-contract-tests.md) | OpenAPI spec built from a Go route table (no codegen toolchain), served at `/v1/openapi.json`, kept honest by a drift guard and a jsonschema contract test | Accepted |
| [0029](0029-sources-endpoints-control-plane-store-and-connector-job-seams.md) | Sources endpoints — control-plane store, `409`-by-index concurrent-sync guard, and connector/worker seams | Accepted |
| [0030](0030-documents-endpoints-tenant-content-via-resolver-and-storage-ingest-seams.md) | Documents endpoints — tenant content reached through the resolver, with object-storage and document-store/ingest-worker seams (upload enqueues `ingest_document`; no partial document row) | Accepted |
| [0031](0031-jobs-endpoints-cancel-semantics-and-river-canceller-seam.md) | Jobs endpoints — queued-job cancel is effective now (guarded mirror-row flip); running-job cancel is a fail-closed River `Canceller` seam (EPIC-09); terminal → 409 | Accepted |
| [0032](0032-admin-tenant-endpoints-over-existing-lifecycle.md) | Admin tenant endpoints — a thin platform-scoped HTTP layer over the existing provisioner/lifecycle/settings backend (create/list/patch/delete-with-grace); not tenant-scoped | Accepted |
| [0033](0033-document-version-store-write-side-in-documents-package.md) | Document/version write store — `TenantStore.Put` in `internal/documents` (not the gated `internal/ingest`); embedded-at-commit, unchanged-hash touches `last_seen_at`, changed-hash inserts a version + chunks and flips `current_version` in one transaction (atomic `live_chunks` swap) | Accepted |
| [0034](0034-go-parsers-normalised-contract-and-semantic-html-boilerplate-heuristic.md) | Go parsers (`internal/ingest/parse`) — one `Normalised{Title, Blocks}` shape mirroring the sidecar JSON, canonical-MIME `Registry` dispatch with an `ErrUnsupportedMIME` routing seam, and a semantic HTML boilerplate heuristic over `golang.org/x/net/html` (content-root + chrome-skip, body fallback) instead of a full readability/density library | Accepted |
| [0035](0035-parsing-sidecar-implementation-and-go-client.md) | Parsing sidecar + Go client — Flask/gunicorn over focused per-format libraries (PyMuPDF/python-docx/python-pptx/openpyxl) not a mega-extractor; sidecar emits the SPEC-05 §2 `rows` shape so it decodes into `parse.Normalised`; 415/422 terminal vs 429/5xx retryable; client retries with Retry-After + W3C trace propagation (no otelhttp dep) in `internal/ingest/sidecar` | Accepted |
| [0036](0036-structure-aware-chunker.md) | Structure-aware chunker (`internal/ingest/chunk`) — heading-bounded chunks (one `heading_path` each), atomic tables/code split only above 2×target on row/line boundaries, word-granular overlap, an embed-only context line (`Chunk.EmbedText` vs stored `Content`), and an injectable approximate tokeniser instead of a tiktoken dependency | Accepted |
| [0037](0037-embedding-provider-interface-and-implementations.md) | Embedding provider interface — `Result{Vectors,Tokens}`-returning `Embedder` (widened from SPEC's `[][]float32` to surface token usage), dependency-free `net/http` OpenAI/Voyage/Cohere/TEI clients behind a `registry`, a batching/bounded-concurrency/circuit-breaker/`Retry-After`-retry wrapper that is itself an `Embedder`, and a fail-closed `settings.providers_allowed` check in `internal/ingest/embed` | Accepted |
| [0038](0038-ingestion-sink-and-commit-semantics.md) | Ingestion sink (`internal/ingest/sink`) — per-document orchestration composing the built parse/chunk/embed/store stages (owns no SQL), an atomic hash-before-embed short-circuit (`documents.TouchIfUnchanged`), full-sync-only `Complete` soft-delete (`SoftDeleteUnseen`), and a snooze-vs-record-vs-fail error classification with crash safety from the store's per-document transaction plus hash-skip on retry | Accepted |
| [0039](0039-reindex-table-swap-resumable-operation.md) | Reindex table swap (`internal/ingest/reindex` + `documents/reindex.go`) — a resumable (document_id cursor, idempotent per-version insert) build of `chunks_new` at the new dimension through `*tenant.DB` while queries keep serving `chunks`, a verify-before-swap/verify-before-drop coverage gate, the one-transaction SPEC-03 §5 rename swap, and the reconciliation of a dimension change with ADR-0022 (physical `vector(N)` moves in the swap; the worker moves the `embedding_dim` mirror as the sanctioned finalize step) | Accepted |

Template for new ADRs: `NNNN-short-title.md` with sections Status, Context, Options, Decision, Consequences. Reference requirement IDs from the SRS.
