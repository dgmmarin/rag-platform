# Delivery backlog

Estimates in story points (1, 2, 3, 5, 8, 13). Sprint length assumed 2 weeks, team of 4–5 engineers (~30 pts/sprint). v1 scope ≈ 337 pts ≈ 11 sprints; a parallel track for the admin UI shortens this.

Conventions: every story lists **AC** (acceptance criteria) and **Traces** (SRS / spec IDs). Tasks are the engineering breakdown; teams may re-slice.

Suggested sprint order: EPIC-01 → 02 → 03 → 04 → 05 (+06 in parallel) → 07 → 08 → 09 → 10 → 11 → 12. EPIC-10 work items are spread from sprint 2 onward.

---

## EPIC-01 · Project foundation
*Goal: repo, CI, local stack, shared libraries. Enables everything else.* — 21 pts

**STORY-01.1 Repository skeleton and build** (3) — Traces: ADR-0002
- AC: `make build` produces single binary `ragctl` with subcommands `serve`, `work`, `migrate`, `enroll` (stubs). `make test`, `make lint` pass.
- Tasks: go module layout (`cmd/`, `internal/`), Makefile, golangci-lint config, Dockerfile (distroless), `.editorconfig`.

**STORY-01.2 Local development stack** (3)
- AC: `docker compose up` starts Postgres 16 + pgvector, MinIO, the parsing sidecar stub, and the app; README documents it.
- Tasks: compose file, seed script creating control-plane DB, `.env.example`.

**STORY-01.3 CI pipeline** (5) — Traces: NFR-MNT-03
- AC: on PR: lint, unit tests with coverage report, integration tests against Postgres service, govulncheck, image build. Coverage gate 70 % on `internal/tenant`, `internal/ingest`, `internal/retrieve`, `internal/connector`.
- Tasks: GitHub Actions (or equivalent) workflows, test Postgres service with pgvector, cache modules.

**STORY-01.4 Configuration and secrets loading** (3) — Traces: SPEC-09 §2
- AC: config from env + optional file; KMS provider interface with `local` (age) and `aws` implementations; DEK loaded at startup; missing DEK fails startup.
- Tasks: `internal/config`, `internal/crypto` (AES-GCM envelope, key version header), unit tests with vectors.

**STORY-01.5 Control-plane migrations tooling** (2) — Traces: SPEC-02
- AC: `ragctl migrate control` applies `migrations/control/*.sql` via goose; `control_plane.sql` split into initial migration.
- Tasks: goose integration, first migration, CI check that schema file matches migrations.

**STORY-01.6 Logging, metrics, tracing scaffolding** (5) — Traces: FR-OBS-01/02/03, SPEC-10
- AC: slog JSON with request_id/tenant_id injection; `/metrics` endpoint; OTel tracer configured by env; `/healthz` and `/readyz` skeletons.
- Tasks: `internal/obs` package, HTTP middleware, test that tenant_id appears in log lines.

---

## EPIC-02 · Tenancy core
*Goal: resolver, TenantDB, provisioning, tenant migrations.* — 34 pts

**STORY-02.1 Tenant registry and resolver** (8) — Traces: FR-TEN-03, FR-ACC-03, SPEC-01 §2–4, ADR-0003
- AC: `Resolver.Open` returns read-write DB for active, read-only for suspended, errors for other states; registry cached 30 s and invalidated by NOTIFY; pools lazily created, evicted after idle, capped.
- Tasks: `tenant.DB` wrapper, pool cache with LRU, registry loader, LISTEN/NOTIFY trigger on `tenants`, unit + integration tests.

**STORY-02.2 Tenant schema migrations** (5) — Traces: FR-TEN-09, SPEC-01 §7
- AC: `ragctl migrate tenants` applies pending migrations to all tenants in parallel, records `schema_version`, continues past failures, exits non-zero with list, rerun resumes. `Open` fails closed on version mismatch.
- Tasks: goose per-tenant runner, `tenant.sql` as migration 0001 with dimension placeholder, row locking, tests with a deliberately failing tenant.

**STORY-02.3 Tenant provisioning job** (8) — Traces: FR-TEN-01/02, SPEC-01 §6
- AC: `POST /admin/tenants` or `ragctl enroll` creates tenant row, enqueues `provision_tenant`; job creates role + database with least privilege, encrypts password, runs migrations, sets active; idempotent on retry.
- Tasks: privileged provisioning connection config, SQL for role/db creation, job handler, audit event, integration test on real Postgres.

**STORY-02.4 Tenant suspension, deletion and grace period** (5) — Traces: FR-TEN-04/05, SPEC-01 §8
- AC: suspend → queries by viewers/API keys return `tenant_unavailable`, admins can read; delete → status deleting, grace timer, cancel possible, after grace database and role dropped and object-storage prefix removed; audit trail complete.
- Tasks: status transitions service, `delete_tenant` job with scheduled-at, cancellation path, verification test that no rows remain.

**STORY-02.5 Tenant move (connection update)** (3) — Traces: FR-TEN-07
- AC: `PATCH /admin/tenants/{id}` with new connection details evicts pool; next request uses new DB; runbook documented.
- Tasks: endpoint, `Resolver.Close`, docs/runbooks/move-tenant.md.

**STORY-02.6 Isolation test suite** (5) — Traces: NFR-SEC-01, SPEC-01 §9
- AC: automated suite enrols two tenants, exercises every tenant-scoped endpoint with A's credentials against B's IDs, asserts 404/403 and zero data leakage; runs in CI.
- Tasks: test harness, endpoint table generated from router, lint rule forbidding `Unsafe()` outside allowed packages.

---

## EPIC-03 · Control plane services
*Goal: users, auth, membership, API keys, settings, audit, usage.* — 34 pts

**STORY-03.1 User accounts and sessions** (5) — Traces: FR-ACC-01, SPEC-09 §3
- AC: signup/login with argon2id; session cookie; logout; lockout policy; CSRF on mutations.
- Tasks: auth package, session store, middleware, tests.

**STORY-03.2 OIDC login** (5) — Traces: FR-ACC-01
- AC: configurable OIDC provider; PKCE flow; JIT user creation flag; existing user linked by verified email.

**STORY-03.3 Tenant membership and roles** (5) — Traces: FR-ACC-02/06, SPEC-02 §4
- AC: members CRUD; role matrix enforced by middleware on every route; owner cannot remove last owner; tests per role × route.

**STORY-03.4 API keys** (5) — Traces: FR-ACC-04/05
- AC: create returns secret once; list shows prefix, scopes, last used; revoke takes effect immediately; scope enforced; expiry honoured.

**STORY-03.5 Tenant settings with JSON-schema validation** (3) — Traces: FR-TEN-08, SPEC-02 §5
- AC: `GET/PATCH /v1/settings`; invalid documents rejected with field errors; `embedding.dim` immutable; change audited.

**STORY-03.6 Audit log** (3) — Traces: FR-ADM-05, SPEC-02 §6
- AC: every listed action writes an audit row with actor, tenant, target; `GET /admin/audit?tenant=` for platform admins.

**STORY-03.7 Usage counters** (3) — Traces: FR-ADM-06, SPEC-10 §6
- AC: queries, docs, chunks, tokens aggregated daily with ≤ 60 s lag; `GET /v1/usage`.

**STORY-03.8 Platform admin impersonation** (3) — Traces: FR-ACC-07
- AC: platform admin can act as tenant admin; banner in UI; every action audited with impersonation flag.

**STORY-03.9 Rate limiting** (2) — Traces: NFR-SEC-07, SPEC-07 §1
- AC: per-key and per-tenant token buckets; 429 with Retry-After; metrics.

---

## EPIC-04 · Public API surface
*Goal: HTTP layer, OpenAPI, error model.* — 21 pts

**STORY-04.1 HTTP server, routing, middleware chain** (5) — Traces: SPEC-07 §1
- AC: request ID, auth, tenant resolution, rate limit, logging, recovery, CORS; error envelope; graceful shutdown.

**STORY-04.2 OpenAPI generation and contract tests** (5) — Traces: SPEC-07 §3
- AC: `api/openapi.yaml` generated from code; served at `/v1/openapi.json`; CI validates recorded responses.

**STORY-04.3 Sources endpoints** (3) — Traces: FR-SRC-01/14
- AC: CRUD, `sync`, `test` wired to connector registry; 409 on concurrent sync.

**STORY-04.4 Documents endpoints** (3) — Traces: FR-SRC-02, FR-ADM-03
- AC: multipart upload to object storage + job; list/get/delete; chunks debug endpoint for admins.

**STORY-04.5 Jobs endpoints** (2) — Traces: FR-ADM-02
- AC: list with filters, get, cancel.

**STORY-04.6 Admin tenant endpoints** (3) — Traces: FR-TEN-01/05/07
- AC: create, list, patch (status/connection/settings), delete with grace.

---

## EPIC-05 · Ingestion pipeline
*Goal: documents → versions → chunks → embeddings, atomically.* — 42 pts

**STORY-05.1 Document and version store** (5) — Traces: FR-ING-02, ADR-0008, SPEC-03
- AC: `Put` with unchanged hash only touches `last_seen_at`; changed hash inserts version; `current_version` flipped in same tx as chunks; `live_chunks` reflects swap instantly.

**STORY-05.2 Go parsers: HTML, Markdown, text, CSV, JSON** (5) — Traces: FR-ING-01, SPEC-05 §2
- AC: HTML boilerplate removed (readability), headings preserved, tables → markdown; golden-file tests for 10 representative pages.

**STORY-05.3 Parsing sidecar (Python) and Go client** (8) — Traces: FR-ING-11, ADR-0006
- AC: `POST /parse` for PDF/DOCX/PPTX/XLSX returns blocks with headings and tables; Go client with timeout, retries, tracing; fixtures for 6 document types; container image; health endpoint.

**STORY-05.4 Structure-aware chunker** (5) — Traces: FR-ING-03/04, SPEC-05 §3
- AC: respects headings, keeps tables/code intact, target/overlap configurable, `heading_path` populated, context line prepended for embedding; property tests on size bounds.

**STORY-05.5 Embedding provider interface and implementations** (5) — Traces: FR-ING-05, NFR-MNT-02
- AC: `Embedder` interface; OpenAI, Voyage, Cohere, TEI; batching, retry/backoff, `Retry-After`, circuit breaker; token usage reported; provider allowlist enforced.

**STORY-05.6 Sink implementation and commit semantics** (5) — Traces: FR-ING-07, NFR-REL-02, SPEC-05 §5
- AC: per-document transaction; `Complete` marks unseen documents deleted on full sync only; worker crash test leaves no partial document.

**STORY-05.7 Job stats and error capture** (2) — Traces: FR-ING-10
- AC: stats JSON populated as specified; per-document errors capped at 100.

**STORY-05.8 Reindex job with table swap** (5) — Traces: FR-ING-09, SPEC-03 §5, SPEC-05 §7
- AC: reindex to new model/dimension while queries continue on old table; resumable; swap atomic; old table dropped after verification flag.

**STORY-05.9 Garbage collection job** (2) — Traces: SPEC-03 §4
- AC: old versions, deleted documents, query logs, stale crawl pages removed per retention; metrics on rows removed.

---

## EPIC-06 · Connector framework and upload connector
*Goal: interface, registry, credentials, upload path.* — 13 pts

**STORY-06.1 Connector interface, registry, config validation** (5) — Traces: FR-SRC-13, NFR-MNT-01, SPEC-04 §1
- AC: `Register`/`Lookup` by kind; JSON-schema validation; `Test` wiring; unit tests with a fake connector.

**STORY-06.2 Credential encryption and handling** (3) — Traces: FR-SRC-10, SPEC-04 §6
- AC: credentials encrypted on write, never returned, decrypted only inside sync, zeroed after; error messages sanitised.

**STORY-06.3 Upload connector and ingest_document job** (5) — Traces: FR-SRC-02, SPEC-04 §5
- AC: upload → object storage → job → document; re-upload creates new version; size limit from settings; MIME sniffing.

---

## EPIC-07 · Web crawl, sitemap and API connectors
*Goal: the three primary external sources.* — 39 pts

**STORY-07.1 Web crawler core** (8) — Traces: FR-SRC-03/04, SPEC-04 §2
- AC: BFS with depth/pages limits, allow/deny rules, robots.txt, per-host delay, concurrency, URL normalisation, canonical handling; crawl state persisted; resumable.

**STORY-07.2 SSRF protection and egress rules** (3) — Traces: NFR-SEC-04, SPEC-09 §4
- AC: private ranges blocked incl. on redirects and DNS rebinding; tests for each class; size/timeout limits.

**STORY-07.3 HTML content extraction quality** (5) — Traces: FR-SRC-05
- AC: include/exclude selectors; readability fallback; title extraction; 20-page golden corpus with ≥ 90 % boilerplate removed (manual review once, then regression).

**STORY-07.4 Conditional fetch and change detection** (3) — Traces: FR-ING-02
- AC: ETag/Last-Modified used; unchanged pages cost a HEAD/304 and no parse.

**STORY-07.5 Sitemap connector** (3) — Traces: FR-SRC-06
- AC: sitemap + sitemap index parsing; `lastmod` incremental; shares fetch/extract code with crawler.

**STORY-07.6 HTTP API connector: auth and pagination** (8) — Traces: FR-SRC-07, SPEC-04 §4
- AC: api_key_header, bearer, basic, oauth2 client-credentials (token refresh); pagination none/page/offset/cursor/link-header; rate limiting and Retry-After; fixture server tests for each combination.

**STORY-07.7 HTTP API connector: templating and incremental sync** (5) — Traces: FR-SRC-07/08
- AC: `text/template` rendering with helpers; `uri_template`; metadata JSONPath extraction; `incremental_param` with cursor in State; weekly full sync for deletion detection.

**STORY-07.8 Source "test connection" for all kinds** (2) — Traces: FR-SRC-14
- AC: each connector's `Test` validates reachability and credentials within 10 s and returns actionable errors.

**STORY-07.9 Connector documentation** (2)
- AC: `docs/connectors/*.md` with config reference and examples per kind.

---

## EPIC-08 · Retrieval and answering
*Goal: hybrid search, rerank, grounded answers with citations.* — 39 pts

**STORY-08.1 Hybrid retrieval query** (8) — Traces: FR-RET-01/02/08, ADR-0007, SPEC-06 §2
- AC: single SQL round trip with RRF; filters (source, uri prefix, date, metadata tags); `hnsw.ef_search` tuning; p95 ≤ 120 ms at 1 M chunks in benchmark.

**STORY-08.2 Retrieve endpoint** (2) — Traces: FR-RET-08
- AC: returns ranked chunks with scores and metadata; respects filters and top_k.

**STORY-08.3 Reranker interface and providers** (5) — Traces: FR-RET-03
- AC: Cohere + LLM-based reranker; per-tenant toggle; fallback to fused order on provider failure.

**STORY-08.4 LLM provider interface** (5) — Traces: NFR-MNT-02, NFR-REL-04
- AC: Anthropic, OpenAI, OpenAI-compatible (vLLM/Ollama); streaming and non-streaming; retries; token accounting; allowlist enforced.

**STORY-08.5 Prompt assembly, citations and grounding refusal** (8) — Traces: FR-RET-04/05, SPEC-06 §4–5
- AC: answers cite `[n]`; citations mapped to chunks; unreferenced chunks dropped; below-floor → grounded=false without LLM call; language matching; token budget respected.

**STORY-08.6 Query endpoint with streaming** (5) — Traces: FR-RET-06, SPEC-06 §6
- AC: JSON and SSE modes; citations emitted before text; usage in `done`.

**STORY-08.7 Conversation history and question rewrite** (3) — Traces: FR-RET-07
- AC: follow-up questions resolved into standalone query before retrieval; toggle per tenant; eval shows no regression on single-turn.

**STORY-08.8 Query log and feedback** (3) — Traces: FR-RET-09/10
- AC: every query logged asynchronously with retrieved IDs and scores; feedback endpoint; both visible in admin.

---

## EPIC-09 · Jobs, scheduling and maintenance
*Goal: River integration, scheduler, cancellation, mirroring.* — 21 pts

**STORY-09.1 River integration and worker binary** (5) — Traces: FR-ING-08, ADR-0005, SPEC-08 §1
- AC: queues ingest/maintenance/platform with separate concurrency; job args carry tenant_id; worker opens TenantDB per job; graceful shutdown drains.

**STORY-09.2 Job status mirroring to `jobs` table** (3) — Traces: FR-ADM-02, SPEC-08 §3
- AC: transitions, attempts, stats, errors mirrored; admin reads only `jobs`.

**STORY-09.3 Scheduler for cron sources and daily GC** (5) — Traces: FR-SRC-11, SPEC-08 §2
- AC: leader-elected loop; `next_run_at` computed from cron; full sync every Nth run; GC daily per tenant; no duplicate enqueues under two replicas.

**STORY-09.4 Cancellation and uniqueness** (3) — Traces: SPEC-08 §4
- AC: one active sync per source; cancel queued immediately; running jobs stop between documents with status cancelled.

**STORY-09.5 Per-tenant concurrency caps and fairness** (3)
- AC: a tenant with 10 queued syncs cannot occupy more than N workers; other tenants' jobs proceed; test with synthetic load.

**STORY-09.6 Delete-source job** (2) — Traces: FR-SRC-12
- AC: removes documents, versions, chunks, crawl state for the source; stats reported.

---

## EPIC-10 · Security, observability, operations
*Goal: production readiness.* — 26 pts

**STORY-10.1 Metrics catalogue and dashboards** (5) — Traces: FR-OBS-02, SPEC-10 §2/5
- AC: all listed metrics emitted; Grafana dashboards (API, ingestion, jobs, providers, per-tenant) committed as JSON.

**STORY-10.2 Alert rules** (2) — Traces: SPEC-10 §5
- AC: alert rules committed; runbook link per alert.

**STORY-10.3 Distributed tracing end to end** (3) — Traces: FR-OBS-03
- AC: one trace covers API → retrieval → provider; worker job → sidecar; sampled at configurable rate.

**STORY-10.4 DEK rotation command** (3) — Traces: NFR-SEC-03, SPEC-09 §2
- AC: `ragctl keys rotate-dek` re-encrypts all secrets under a new key version with zero downtime; old key retained until completion.

**STORY-10.5 Backups and PITR verification** (3) — Traces: NFR-REL-03
- AC: documented backup configuration for all tenant databases; monthly restore drill script; runbook.

**STORY-10.6 Security scanning in CI and dependency policy** (2) — Traces: SPEC-09 §6
- AC: govulncheck, pip-audit, image scan block merges on high severity.

**STORY-10.7 Load and isolation testing** (5) — Traces: NFR-PERF-01, SRS §8
- AC: k6 (or similar) scenario: 50 concurrent queries across 4 tenants at 1 M chunks; p95 retrieval ≤ 300 ms; results committed.

**STORY-10.8 Runbooks** (3)
- AC: enrol tenant, move tenant, failed migration, provider outage, stuck job, tenant deletion, incident response.

---

## EPIC-11 · Admin UI (reference)
*Goal: minimal UI over the API for tenant admins and platform admins. Can run in parallel from sprint 4.* — 34 pts

**STORY-11.1 App shell, auth, tenant switcher** (5)
**STORY-11.2 Sources list/create/edit with per-kind forms and test-connection** (8) — Traces: FR-ADM-01
**STORY-11.3 Jobs list and detail with cancel** (5) — Traces: FR-ADM-02
**STORY-11.4 Documents and chunks browser** (5) — Traces: FR-ADM-03
**STORY-11.5 Members, API keys, settings pages** (5)
**STORY-11.6 Query playground with citations and feedback** (3)
**STORY-11.7 Platform admin: tenants list, enrol, suspend, delete** (3)

---

## EPIC-12 · Evaluation harness and quality
*Goal: measure and gate retrieval quality.* — 13 pts

**STORY-12.1 Eval cases CRUD and import (CSV)** (3) — Traces: FR-ADM-04
**STORY-12.2 `ragctl eval run` with recall@k, grounded rate, latency** (5) — Traces: SPEC-06 §8
**STORY-12.3 LLM-as-judge correctness scoring (optional flag)** (3)
**STORY-12.4 Eval report in admin UI and CI gate for settings changes** (2)

---

## v2 candidates (not estimated)
- S3 connector (FR-SRC-09); JS-rendered crawling; webhook/CDC ingestion.
- Structured product extraction and SQL-assisted answers (FR-ING-12).
- Tenant export archive (FR-TEN-06) — promote to v1 if a customer requires it.
- Dedicated vector DB option when a tenant exceeds pgvector comfort zone (revisit ADR-0004).
- Billing integration on top of usage_daily.

## Definition of done (all stories)
- Code reviewed; unit + integration tests; lint clean; no new high vulnerabilities.
- Tenant isolation tests pass when any tenant-scoped code changes.
- Logs/metrics/traces added for new paths.
- Docs updated (spec, connector doc or runbook as applicable); ADR written if a decision was made.
- Demoable in the local stack.
