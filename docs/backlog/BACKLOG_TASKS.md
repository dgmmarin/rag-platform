# Delivery backlog — task checklist

Task-level breakdown expanded from `backlog_import.csv`. Epic/story readiness is in
[`BACKLOG_STATUS.md`](BACKLOG_STATUS.md); full narrative in [`BACKLOG.md`](BACKLOG.md).

Checkboxes: `[x]` delivered, `[ ]` outstanding. Where the source CSV had no explicit task
breakdown, tasks are derived from the acceptance criteria.

---

## EPIC-01 · Project foundation — ✅ complete

### STORY-01.1 — Repository skeleton and build (ADR-0002)
- [x] go module layout (`cmd/`, `internal/`)
- [x] task runner (mise, superseding Makefile)
- [x] golangci-lint config (`.golangci.yml`)
- [x] Dockerfile (distroless)
- [x] `.editorconfig`

### STORY-01.2 — Local development stack
- [x] compose file (Postgres 16 + pgvector, MinIO, parser sidecar, app)
- [x] seed script creating control-plane DB
- [x] `.env.example`
- [x] README documents the stack

### STORY-01.3 — CI pipeline (NFR-MNT-03)
- [x] GitHub Actions workflow (`.github/workflows/ci.yml`)
- [x] lint + unit tests with coverage report
- [x] integration tests against Postgres service with pgvector
- [x] govulncheck + image build
- [x] module caching
- [x] coverage gate 70% on `internal/tenant,ingest,retrieve,connector` (skips not-yet-created packages)

### STORY-01.4 — Configuration and secrets loading (SPEC-09 §2)
- [x] `internal/config` (env + optional file)
- [x] KMS provider interface with `local` (age) and `aws`
- [x] `internal/crypto` (AES-GCM envelope, key-version header)
- [x] DEK loaded at startup; missing DEK fails startup
- [x] unit tests with vectors

### STORY-01.5 — Control-plane migrations tooling (SPEC-02)
- [x] goose integration
- [x] first migration (`control_plane.sql` split)
- [x] `ragctl migrate control` applies `migrations/control/*.sql`
- [x] CI check that schema file matches migrations (drift-guard test)

### STORY-01.6 — Logging, metrics, tracing scaffolding (FR-OBS-01/02/03, SPEC-10)
- [x] `internal/obs` package
- [x] slog JSON with request_id/tenant_id injection
- [x] `/metrics` endpoint
- [x] OTel tracer configured by env
- [x] `/healthz` and `/readyz` skeletons
- [x] HTTP middleware
- [x] test that tenant_id appears in log lines

---

## EPIC-02 · Tenancy core

### STORY-02.1 — Tenant registry and resolver (FR-TEN-03, FR-ACC-03, SPEC-01 §2–4, ADR-0003)
- [ ] `tenant.DB` wrapper
- [ ] pool cache with LRU (lazy create, idle eviction, capped)
- [ ] registry loader (cached 30 s)
- [ ] LISTEN/NOTIFY trigger on `tenants` for cache invalidation
- [ ] `Resolver.Open`: rw for active, ro for suspended, error otherwise
- [ ] unit + integration tests

### STORY-02.2 — Tenant schema migrations (FR-TEN-09, SPEC-01 §7)
- [ ] goose per-tenant runner (parallel, records `schema_version`)
- [ ] `tenant.sql` as migration 0001 with dimension placeholder
- [ ] row locking
- [ ] continues past failures, exits non-zero with list, rerun resumes
- [ ] `Open` fails closed on version mismatch
- [ ] tests with a deliberately failing tenant

### STORY-02.3 — Tenant provisioning job (FR-TEN-01/02, SPEC-01 §6)
- [ ] privileged provisioning connection config
- [ ] SQL for role/db creation (least privilege)
- [ ] job handler (encrypt password, run migrations, set active; idempotent)
- [ ] `POST /admin/tenants` / `ragctl enroll` enqueues `provision_tenant`
- [ ] audit event
- [ ] integration test on real Postgres

### STORY-02.4 — Tenant suspension, deletion and grace period (FR-TEN-04/05, SPEC-01 §8)
- [ ] status transitions service
- [ ] suspend → `tenant_unavailable` for viewers/API keys, admins can read
- [ ] `delete_tenant` job with scheduled-at + grace timer
- [ ] cancellation path
- [ ] after grace: drop database + role, remove object-storage prefix
- [ ] verification test that no rows remain

### STORY-02.5 — Tenant move (connection update) (FR-TEN-07)
- [ ] `PATCH /admin/tenants/{id}` endpoint (evicts pool)
- [ ] `Resolver.Close`
- [ ] `docs/runbooks/move-tenant.md`

### STORY-02.6 — Isolation test suite (NFR-SEC-01, SPEC-01 §9)
- [ ] test harness enrolling two tenants
- [ ] endpoint table generated from router
- [ ] A's credentials against B's IDs assert 404/403, zero leakage
- [ ] lint rule forbidding `Unsafe()` outside allowed packages
- [ ] runs in CI

---

## EPIC-03 · Control plane services

### STORY-03.1 — User accounts and sessions (FR-ACC-01, SPEC-09 §3)
- [ ] auth package (argon2id signup/login)
- [ ] session store + cookie; logout
- [ ] lockout policy
- [ ] CSRF on mutations
- [ ] middleware + tests

### STORY-03.2 — OIDC login (FR-ACC-01)
- [ ] configurable OIDC provider
- [ ] PKCE flow
- [ ] JIT user creation flag
- [ ] existing user linked by verified email

### STORY-03.3 — Tenant membership and roles (FR-ACC-02/06, SPEC-02 §4)
- [ ] members CRUD
- [ ] role matrix enforced by middleware on every route
- [ ] owner cannot remove last owner
- [ ] tests per role × route

### STORY-03.4 — API keys (FR-ACC-04/05)
- [ ] create returns secret once
- [ ] list shows prefix, scopes, last used
- [ ] revoke takes effect immediately
- [ ] scope enforced; expiry honoured

### STORY-03.5 — Tenant settings with JSON-schema validation (FR-TEN-08, SPEC-02 §5)
- [ ] `GET/PATCH /v1/settings`
- [ ] invalid documents rejected with field errors
- [ ] `embedding.dim` immutable
- [ ] change audited

### STORY-03.6 — Audit log (FR-ADM-05, SPEC-02 §6)
- [ ] every listed action writes an audit row with actor, tenant, target
- [ ] `GET /admin/audit?tenant=` for platform admins

### STORY-03.7 — Usage counters (FR-ADM-06, SPEC-10 §6)
- [ ] queries, docs, chunks, tokens aggregated daily with ≤ 60 s lag
- [ ] `GET /v1/usage`

### STORY-03.8 — Platform admin impersonation (FR-ACC-07)
- [ ] platform admin can act as tenant admin
- [ ] banner in UI
- [ ] every action audited with impersonation flag

### STORY-03.9 — Rate limiting (NFR-SEC-07, SPEC-07 §1)
- [ ] per-key and per-tenant token buckets
- [ ] 429 with Retry-After
- [ ] metrics

---

## EPIC-04 · Public API surface

### STORY-04.1 — HTTP server, routing, middleware chain (SPEC-07 §1)
- [ ] request ID, auth, tenant resolution, rate limit, logging, recovery, CORS
- [ ] error envelope
- [ ] graceful shutdown

### STORY-04.2 — OpenAPI generation and contract tests (SPEC-07 §3)
- [ ] `api/openapi.yaml` generated from code
- [ ] served at `/v1/openapi.json`
- [ ] CI validates recorded responses

### STORY-04.3 — Sources endpoints (FR-SRC-01/14)
- [ ] CRUD, `sync`, `test` wired to connector registry
- [ ] 409 on concurrent sync

### STORY-04.4 — Documents endpoints (FR-SRC-02, FR-ADM-03)
- [ ] multipart upload to object storage + job
- [ ] list/get/delete
- [ ] chunks debug endpoint for admins

### STORY-04.5 — Jobs endpoints (FR-ADM-02)
- [ ] list with filters, get, cancel

### STORY-04.6 — Admin tenant endpoints (FR-TEN-01/05/07)
- [ ] create, list, patch (status/connection/settings), delete with grace

---

## EPIC-05 · Ingestion pipeline

### STORY-05.1 — Document and version store (FR-ING-02, ADR-0008, SPEC-03)
- [ ] `Put` with unchanged hash only touches `last_seen_at`
- [ ] changed hash inserts version
- [ ] `current_version` flipped in same tx as chunks
- [ ] `live_chunks` reflects swap instantly

### STORY-05.2 — Go parsers: HTML, Markdown, text, CSV, JSON (FR-ING-01, SPEC-05 §2)
- [ ] HTML boilerplate removed (readability)
- [ ] headings preserved, tables → markdown
- [ ] golden-file tests for 10 representative pages

### STORY-05.3 — Parsing sidecar (Python) and Go client (FR-ING-11, ADR-0006)
- [ ] `POST /parse` for PDF/DOCX/PPTX/XLSX returns blocks with headings and tables
- [ ] Go client with timeout, retries, tracing
- [ ] fixtures for 6 document types
- [ ] container image + health endpoint

### STORY-05.4 — Structure-aware chunker (FR-ING-03/04, SPEC-05 §3)
- [ ] respects headings, keeps tables/code intact
- [ ] target/overlap configurable
- [ ] `heading_path` populated, context line prepended for embedding
- [ ] property tests on size bounds

### STORY-05.5 — Embedding provider interface and implementations (FR-ING-05, NFR-MNT-02)
- [ ] `Embedder` interface; OpenAI, Voyage, Cohere, TEI
- [ ] batching, retry/backoff, `Retry-After`, circuit breaker
- [ ] token usage reported; provider allowlist enforced

### STORY-05.6 — Sink implementation and commit semantics (FR-ING-07, NFR-REL-02, SPEC-05 §5)
- [ ] per-document transaction
- [ ] `Complete` marks unseen documents deleted on full sync only
- [ ] worker crash test leaves no partial document

### STORY-05.7 — Job stats and error capture (FR-ING-10)
- [ ] stats JSON populated as specified
- [ ] per-document errors capped at 100

### STORY-05.8 — Reindex job with table swap (FR-ING-09, SPEC-03 §5, SPEC-05 §7)
- [ ] reindex to new model/dimension while queries continue on old table
- [ ] resumable; swap atomic
- [ ] old table dropped after verification flag

### STORY-05.9 — Garbage collection job (SPEC-03 §4)
- [ ] old versions, deleted documents, query logs, stale crawl pages removed per retention
- [ ] metrics on rows removed

---

## EPIC-06 · Connector framework and upload connector

### STORY-06.1 — Connector interface, registry, config validation (FR-SRC-13, NFR-MNT-01, SPEC-04 §1)
- [ ] `Register`/`Lookup` by kind
- [ ] JSON-schema validation
- [ ] `Test` wiring
- [ ] unit tests with a fake connector

### STORY-06.2 — Credential encryption and handling (FR-SRC-10, SPEC-04 §6)
- [ ] credentials encrypted on write, never returned
- [ ] decrypted only inside sync, zeroed after
- [ ] error messages sanitised

### STORY-06.3 — Upload connector and ingest_document job (FR-SRC-02, SPEC-04 §5)
- [ ] upload → object storage → job → document
- [ ] re-upload creates new version
- [ ] size limit from settings; MIME sniffing

---

## EPIC-07 · Web crawl, sitemap and API connectors

### STORY-07.1 — Web crawler core (FR-SRC-03/04, SPEC-04 §2)
- [ ] BFS with depth/pages limits, allow/deny rules, robots.txt
- [ ] per-host delay, concurrency
- [ ] URL normalisation, canonical handling
- [ ] crawl state persisted; resumable

### STORY-07.2 — SSRF protection and egress rules (NFR-SEC-04, SPEC-09 §4)
- [ ] private ranges blocked incl. on redirects and DNS rebinding
- [ ] tests for each class
- [ ] size/timeout limits

### STORY-07.3 — HTML content extraction quality (FR-SRC-05)
- [ ] include/exclude selectors; readability fallback; title extraction
- [ ] 20-page golden corpus with ≥ 90% boilerplate removed

### STORY-07.4 — Conditional fetch and change detection (FR-ING-02)
- [ ] ETag/Last-Modified used
- [ ] unchanged pages cost a HEAD/304 and no parse

### STORY-07.5 — Sitemap connector (FR-SRC-06)
- [ ] sitemap + sitemap index parsing
- [ ] `lastmod` incremental
- [ ] shares fetch/extract code with crawler

### STORY-07.6 — HTTP API connector: auth and pagination (FR-SRC-07, SPEC-04 §4)
- [ ] api_key_header, bearer, basic, oauth2 client-credentials (token refresh)
- [ ] pagination none/page/offset/cursor/link-header
- [ ] rate limiting and Retry-After
- [ ] fixture server tests for each combination

### STORY-07.7 — HTTP API connector: templating and incremental sync (FR-SRC-07/08)
- [ ] `text/template` rendering with helpers; `uri_template`
- [ ] metadata JSONPath extraction
- [ ] `incremental_param` with cursor in State
- [ ] weekly full sync for deletion detection

### STORY-07.8 — Source "test connection" for all kinds (FR-SRC-14)
- [ ] each connector's `Test` validates reachability and credentials within 10 s with actionable errors

### STORY-07.9 — Connector documentation
- [ ] `docs/connectors/*.md` with config reference and examples per kind

---

## EPIC-08 · Retrieval and answering

### STORY-08.1 — Hybrid retrieval query (FR-RET-01/02/08, ADR-0007, SPEC-06 §2)
- [ ] single SQL round trip with RRF
- [ ] filters (source, uri prefix, date, metadata tags)
- [ ] `hnsw.ef_search` tuning
- [ ] p95 ≤ 120 ms at 1 M chunks in benchmark

### STORY-08.2 — Retrieve endpoint (FR-RET-08)
- [ ] returns ranked chunks with scores and metadata; respects filters and top_k

### STORY-08.3 — Reranker interface and providers (FR-RET-03)
- [ ] Cohere + LLM-based reranker
- [ ] per-tenant toggle
- [ ] fallback to fused order on provider failure

### STORY-08.4 — LLM provider interface (NFR-MNT-02, NFR-REL-04)
- [ ] Anthropic, OpenAI, OpenAI-compatible (vLLM/Ollama)
- [ ] streaming and non-streaming; retries
- [ ] token accounting; allowlist enforced

### STORY-08.5 — Prompt assembly, citations and grounding refusal (FR-RET-04/05, SPEC-06 §4–5)
- [ ] answers cite `[n]`; citations mapped to chunks; unreferenced chunks dropped
- [ ] below-floor → grounded=false without LLM call
- [ ] language matching; token budget respected

### STORY-08.6 — Query endpoint with streaming (FR-RET-06, SPEC-06 §6)
- [ ] JSON and SSE modes
- [ ] citations emitted before text
- [ ] usage in `done`

### STORY-08.7 — Conversation history and question rewrite (FR-RET-07)
- [ ] follow-up questions resolved into standalone query before retrieval
- [ ] toggle per tenant
- [ ] eval shows no regression on single-turn

### STORY-08.8 — Query log and feedback (FR-RET-09/10)
- [ ] every query logged asynchronously with retrieved IDs and scores
- [ ] feedback endpoint
- [ ] both visible in admin

---

## EPIC-09 · Jobs, scheduling and maintenance

### STORY-09.1 — River integration and worker binary (FR-ING-08, ADR-0005, SPEC-08 §1)
- [ ] queues ingest/maintenance/platform with separate concurrency
- [ ] job args carry tenant_id; worker opens TenantDB per job
- [ ] graceful shutdown drains

### STORY-09.2 — Job status mirroring to `jobs` table (FR-ADM-02, SPEC-08 §3)
- [ ] transitions, attempts, stats, errors mirrored
- [ ] admin reads only `jobs`

### STORY-09.3 — Scheduler for cron sources and daily GC (FR-SRC-11, SPEC-08 §2)
- [ ] leader-elected loop; `next_run_at` computed from cron
- [ ] full sync every Nth run; GC daily per tenant
- [ ] no duplicate enqueues under two replicas

### STORY-09.4 — Cancellation and uniqueness (SPEC-08 §4)
- [ ] one active sync per source
- [ ] cancel queued immediately
- [ ] running jobs stop between documents with status cancelled

### STORY-09.5 — Per-tenant concurrency caps and fairness
- [ ] a tenant with 10 queued syncs cannot occupy more than N workers
- [ ] other tenants' jobs proceed
- [ ] test with synthetic load

### STORY-09.6 — Delete-source job (FR-SRC-12)
- [ ] removes documents, versions, chunks, crawl state for the source
- [ ] stats reported

---

## EPIC-10 · Security, observability, operations

### STORY-10.1 — Metrics catalogue and dashboards (FR-OBS-02, SPEC-10 §2/5)
- [ ] all listed metrics emitted
- [ ] Grafana dashboards (API, ingestion, jobs, providers, per-tenant) committed as JSON

### STORY-10.2 — Alert rules (SPEC-10 §5)
- [ ] alert rules committed
- [ ] runbook link per alert

### STORY-10.3 — Distributed tracing end to end (FR-OBS-03)
- [ ] one trace covers API → retrieval → provider; worker job → sidecar
- [ ] sampled at configurable rate

### STORY-10.4 — DEK rotation command (NFR-SEC-03, SPEC-09 §2)
- [ ] `ragctl keys rotate-dek` re-encrypts all secrets under a new key version with zero downtime
- [ ] old key retained until completion

### STORY-10.5 — Backups and PITR verification (NFR-REL-03)
- [ ] documented backup configuration for all tenant databases
- [ ] monthly restore drill script
- [ ] runbook

### STORY-10.6 — Security scanning in CI and dependency policy (SPEC-09 §6)
- [ ] govulncheck as a hard gate on high severity _(runs non-blocking today via STORY-01.3)_
- [ ] pip-audit
- [ ] image scan blocks merges on high severity

### STORY-10.7 — Load and isolation testing (NFR-PERF-01, SRS §8)
- [ ] k6 scenario: 50 concurrent queries across 4 tenants at 1 M chunks
- [ ] p95 retrieval ≤ 300 ms; results committed

### STORY-10.8 — Runbooks
- [ ] enrol tenant, move tenant, failed migration, provider outage, stuck job, tenant deletion, incident response

---

## EPIC-11 · Admin UI (reference)

### STORY-11.1 — App shell, auth, tenant switcher
- [ ] app shell, auth, tenant switcher

### STORY-11.2 — Sources list/create/edit with per-kind forms and test-connection (FR-ADM-01)
- [ ] per-kind source forms + test-connection

### STORY-11.3 — Jobs list and detail with cancel (FR-ADM-02)
- [ ] jobs list/detail with cancel

### STORY-11.4 — Documents and chunks browser (FR-ADM-03)
- [ ] documents and chunks browser

### STORY-11.5 — Members, API keys, settings pages
- [ ] members, API keys, settings pages

### STORY-11.6 — Query playground with citations and feedback
- [ ] query playground with citations and feedback

### STORY-11.7 — Platform admin: tenants list, enrol, suspend, delete
- [ ] platform admin tenants management

---

## EPIC-12 · Evaluation harness and quality

### STORY-12.1 — Eval cases CRUD and import (CSV) (FR-ADM-04)
- [ ] eval cases CRUD + CSV import

### STORY-12.2 — `ragctl eval run` with recall@k, grounded rate, latency (SPEC-06 §8)
- [ ] `ragctl eval run` (recall@k, grounded rate, latency)

### STORY-12.3 — LLM-as-judge correctness scoring (optional flag)
- [ ] LLM-as-judge correctness scoring (optional flag)

### STORY-12.4 — Eval report in admin UI and CI gate for settings changes
- [ ] eval report in admin UI + CI gate on settings changes
