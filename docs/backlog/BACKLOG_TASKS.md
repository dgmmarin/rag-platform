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
- [x] `tenant.DB` wrapper
- [x] pool cache with LRU (lazy create, idle eviction, capped)
- [x] registry loader (cached 30 s)
- [x] LISTEN/NOTIFY trigger on `tenants` for cache invalidation — control migration
  `00002` adds the `notify_tenant_changed` trigger; the resolver's `Listen` loop
  LISTENs on `tenant_changed` and invalidates the cache within ~1s (e2e-verified).
- [x] `Resolver.Open`: rw for active, ro for suspended, error otherwise (+ fail-closed schema check)
- [x] unit + integration tests (unit in `internal/tenant`; e2e golden path in `test/e2e`)

### STORY-02.2 — Tenant schema migrations (FR-TEN-09, SPEC-01 §7, ADR-0015)
- [x] goose per-tenant runner (parallel, one `Provider` per tenant, records `schema_version`) — `internal/migrate/tenant.go`
- [x] `tenant.sql` as migration 0001 with dimension placeholder (`vector(EMBEDDING_DIM)` substituted per tenant; drift-guard test)
- [x] row locking (`tenant_databases ... FOR UPDATE SKIP LOCKED`, in the same tx as the `schema_version` mirror)
- [x] continues past failures, exits non-zero with list of failed slugs, rerun resumes only those behind
- [x] `Open` fails closed on version mismatch — `expectedSchemaVersion` derived from embedded migrations (`migrate.ExpectedTenantVersion()`)
- [x] tests with a deliberately failing tenant (unit: drift/placeholder/version/validation; e2e golden path incl. failing tenant + resuming rerun)

### STORY-02.3 — Tenant provisioning job (FR-TEN-01/02, SPEC-01 §6) ✅ Done — ADR-0016
- [x] privileged provisioning connection config (`PROVISION_DB_URL`, `TENANT_DB_HOST/PORT/SSLMODE`)
- [x] SQL for role/db creation (least privilege), incl. `CREATE EXTENSION vector/pgcrypto/pg_trgm` as superuser
- [x] job handler (encrypt password, create role+db, run migrations, set active; idempotent) — `internal/provision`
- [x] `ragctl enroll` runs the provisioner synchronously; async `provision_tenant` enqueue + `POST /admin/tenants` deferred to EPIC-09 (River)
- [x] audit event (`tenant.create` / `tenant.provision`; no password logged)
- [x] integration test on real Postgres (role/db/extensions, migrations, active, password round-trip, idempotent re-run)

### STORY-02.4 — Tenant suspension, deletion and grace period (FR-TEN-04/05, SPEC-01 §8)
- [x] status transitions service (`internal/provision/transitions.go` state machine)
- [x] suspend → `tenant_unavailable` for viewers/API keys, admins can read
- [x] `delete_tenant` scheduled-at + grace timer (`ragctl tenant delete`; async River enqueue deferred to EPIC-09)
- [x] cancellation path (`--cancel` during grace)
- [x] after grace: drop database + role (object-storage prefix removal deferred until a client exists — EPIC-06, per ADR-0017)
- [x] verification test that no rows remain (e2e `TestTenantLifecycleGoldenPath`)

### STORY-02.5 — Tenant move (connection update) (FR-TEN-07)
- [x] `PATCH /admin/tenants/{id}` endpoint (evicts pool) — deferred to EPIC-04 (STORY-04.6);
  implemented as `Lifecycle.Move` + `ragctl tenant move` (mirrors 02.3/02.4 deferral, no HTTP router yet)
- [x] `Resolver.Close` — already fully evicts the pool and invalidates the registry cache (STORY-02.1);
  the control-plane connection write also fires `tenant_changed` so the next `Open` rebuilds against the new connection
- [x] `docs/runbooks/move-tenant.md`

### STORY-02.6 — Isolation test suite (NFR-SEC-01, SPEC-01 §9)
- [x] test harness enrolling two tenants (A and B) on the real stack
- [x] endpoint table generated from router — deferred to EPIC-04 (no router yet); asserted at the resolver/`tenant.DB` layer now, two-tenant fixture ready to plug the endpoint matrix in
- [x] A's credentials against B's IDs assert zero leakage (DB/resolver layer now: A can't read B's rows, A's ID never yields B's data, A's creds rejected by B's DB; 404/403 at HTTP deferred to EPIC-04)
- [x] lint rule forbidding `Unsafe()` outside allowed packages (golangci-lint `forbidigo`, allow `internal/provision`/`internal/migrate`; ADR-0018)
- [x] runs in CI (via `mise run lint` and `mise run e2e`, already invoked by CI)

---

## EPIC-03 · Control plane services

### STORY-03.1 — User accounts and sessions (FR-ACC-01, SPEC-09 §3) ✅ Done — ADR-0019
- [x] auth package (argon2id signup/login) — `internal/cp/auth`; PHC-encoded argon2id, constant-time verify; login collapses unknown-email/wrong-password to one error
- [x] session store + cookie; logout — Postgres `sessions` table (control migration `00004`); 128-bit cookie id stored only as sha256; HttpOnly + SameSite=Lax cookie; 12 h sliding idle timeout; logout revokes
- [x] lockout policy — 10 failures / 15 min on `users.failed_login_count`/`locked_until`; locked account refused before password check
- [x] CSRF on mutations — per-session double-submit token (`X-CSRF-Token` vs `sessions.csrf_token`); safe methods exempt; constant-time compare
- [x] middleware + tests — `RequireSession`/`CSRF`/handlers unit-tested with `httptest` (router wiring deferred to STORY-04.1); e2e signup→login→lookup→logout + lockout-after-10 against real control-plane Postgres

### STORY-03.2 — OIDC login (FR-ACC-01)
- [ ] configurable OIDC provider
- [ ] PKCE flow
- [ ] JIT user creation flag
- [ ] existing user linked by verified email

### STORY-03.3 — Tenant membership and roles (FR-ACC-02/06, SPEC-02 §4)
- [x] members CRUD
- [x] role matrix enforced by middleware on every route
- [x] owner cannot remove last owner
- [x] tests per role × route

### STORY-03.4 — API keys (FR-ACC-04/05)
- [x] create returns secret once
- [x] list shows prefix, scopes, last used
- [x] revoke takes effect immediately
- [x] scope enforced; expiry honoured

### STORY-03.5 — Tenant settings with JSON-schema validation (FR-TEN-08, SPEC-02 §5)
- [x] `GET/PATCH /v1/settings`
- [x] invalid documents rejected with field errors
- [x] `embedding.dim` immutable
- [x] change audited

### STORY-03.6 — Audit log (FR-ADM-05, SPEC-02 §6)
- [x] every listed action writes an audit row with actor, tenant, target — sanctioned `audit.Record` writer (actor/tenant/target, non-secret details); tenant.\*/settings.update write today, remaining events adopt it with their handlers (member.\*/apikey.\* in STORY-04.1, source.\* in EPIC-04/06, job.cancel in EPIC-09, admin.impersonate in STORY-03.8)
- [x] `GET /admin/audit?tenant=` for platform admins — handler + tenant-less `RequirePlatformAdmin` middleware (wired to the router in STORY-04.1)

### STORY-03.7 — Usage counters (FR-ADM-06, SPEC-10 §6) ✅ Done — ADR-0024
- [x] queries, docs, chunks, tokens aggregated daily with ≤ 60 s lag — sanctioned `usage.Counter.Add(tenantID, Delta)` buffers per-`(tenant, UTC-day)` increments (the six `usage_daily` columns) with no hot-path I/O; `Counter.Run` flushes every 30 s (SPEC-10 §6) and drains on shutdown, each bucket written with one accumulating upsert (`col = usage_daily.col + excluded.col`) so replicas/flushes sum rather than overwrite; failed flush retains counts (at-least-once), empty tenant/zero delta dropped (fail closed). Per-producer wiring (retrieval/ingestion/answering) adopts `Add` with each producer; `Run` wired into `ragctl serve`/`work` in EPIC-04/09.
- [x] `GET /v1/usage` — `usage.Service.List` (tenant-required, fails closed; range defaults to last 30 days; inverted range rejected) + `Handlers.List` serving `?from&to` (SPEC-07) with the tenant taken from the resolved context (FR-ACC-03, 401 if unresolved, 400 on malformed/inverted range). No migration (`usage_daily` already matches SPEC-02 §2; drift guard green). Router wiring is STORY-04.1. Unit tests (delta merge, day truncation, zero/empty-tenant drop, buffer clear / error-retains-counts, concurrent-adds-exactly-once, accumulating-upsert SQL contract, reader/handler branches, flush-then-final-drain loop) + e2e golden path over real control-plane Postgres (two flushes accumulate on one row, `GET /v1/usage` returns the resolved tenant's rows, no-tenant 401).

### STORY-03.8 — Platform admin impersonation (FR-ACC-07) ✅ Done — ADR-0025
- [x] platform admin can act as tenant admin — `ImpersonationService.Start/End` opens an explicit, audited, time-bounded (`expires_at`, 1 h default) and revocable (`ended_at`) `impersonation_sessions` grant that records BOTH the real admin (`admin_user_id`) and the impersonated principal (`tenant_id` + `impersonated_user_id`) — never a silent identity swap, so actions stay attributable to the admin (FR-ACC-07, C-3). Only platform admins may start it: the Start/End handlers assume `RequireSession` + the STORY-03.6 tenant-less `RequirePlatformAdmin` and take the acting admin from the session, never a body field (FR-ACC-03). Fail closed: missing args / unknown id → `ErrNoImpersonation` (404); `Impersonation.Active(now)` treats an ended or expired grant as inactive.
- [ ] banner in UI — deferred to the admin UI (EPIC-11); request-time application of a live grant + banner rides the same `RequirePlatformAdmin` gate in STORY-04.1/EPIC-11. This story delivers the durable grant + audit primitive those layers consult.
- [x] every action audited with impersonation flag — Start writes `admin.impersonate`, End writes an `admin.impersonate.end` companion, both through the sanctioned `audit.Record` writer (via an injected `AuditFunc` seam), carrying actor = real admin, target = impersonated user, tenant = impersonated tenant, and `details.impersonation=true` (SPEC-02 §4/§6), non-secret ids only (C-3). New schema via goose control migration `00006_impersonation_sessions.sql` mirrored into `schemas/control_plane.sql` (drift guard green). Unit tests (Start/End/`Active`/handler branches via fakes + `httptest`) + e2e golden path over real control-plane Postgres (non-admin 403 no grant, admin grant carries+persists both identities, `admin.impersonate` attributed to the admin with the flag, End stamps + audits `admin.impersonate.end`). Router wiring is STORY-04.1.

### STORY-03.9 — Rate limiting (NFR-SEC-07, SPEC-07 §1) ✅ Done — ADR-0026
- [x] per-key and per-tenant token buckets — new `internal/cp/ratelimit` package: a lazily-refilled token `bucket` (injected clock, no per-bucket goroutine) and a `Limiter` holding one bucket per `key:<id>`/`tenant:<id>` with idle sweeping (`Run`/`idleTTL`). `Middleware.Handler` checks BOTH the per-tenant bucket (aggregate ceiling, looser burst) and, when the request carried an API key, the per-key bucket; both steady-rate at the tenant's `settings.limits.qps` (SPEC-07 §1). The limit key is derived from the resolved tenant + authenticated key id in context (FR-ACC-03) — `auth.RequireScope` now also injects the key id (`auth.WithKeyID`/`KeyIDFromCtx`). Control-plane-only (C-3): reads opaque ids + settings JSON, never tenant data.
- [x] 429 with Retry-After — on refusal the middleware sets `Retry-After` (whole seconds, rounded up) plus `RateLimit-Limit`/`RateLimit-Remaining`/`RateLimit-Reset`, writes the SPEC-07 §1 error envelope (`rate_limited`), and never reaches the inner handler. Fail closed: a settings-lookup error → 429 (limiting never silently disabled), a missing/malformed qps → configured floor (default 10), no resolved tenant → 401.
- [x] metrics — optional `prometheus.Counter` (`Rejected`) incremented on each 429; a nil counter is safe. Configurable via `internal/config` (`RATE_LIMIT_DEFAULT_QPS`/`RATE_LIMIT_KEY_BURST`/`RATE_LIMIT_TENANT_BURST`). No migration (limit read from existing `tenants.settings.limits.qps`, SPEC-02 §5; drift guard green). Router wiring is STORY-04.1. Unit tests (bucket burst/refill/cap with injected clock, per-key/per-tenant isolation + ceiling, 429 headers, session-no-key, no-tenant/lookup-error fail-closed, settings qps extraction, metric increment, eviction loop) + e2e golden path over the real control-plane Postgres driving the real `RequireScope` → rate-limit chain (over-limit key → 429 with headers, a second key of the same tenant unaffected). ADR-0026.

---

## EPIC-04 · Public API surface

### STORY-04.1 — HTTP server, routing, middleware chain (SPEC-07 §1)
- [x] request ID, auth, tenant resolution, rate limit, logging, recovery, CORS — new dependency-injected `internal/api` package (Go 1.22 `net/http.ServeMux`, no new dependency). Global chain outer → inner: `obs.Middleware` (request-id/logging/tracing/metrics; stamps `X-Request-Id` on every response) → `Recover` (panic → `500` envelope, logged server-side, never leaks the panic value/stack) → CORS (preflight `OPTIONS` `204`). Per route: auth/scope/CSRF then the credential-keyed rate limiter *inside* auth (deliberate, intent-preserving divergence from SPEC-07 §1's abstract "rate limit → auth" — the bucket keys off the resolved credential + tenant, FR-ACC-03; ADR-0027). Tenant resolution for the one mounted tenant-scoped route (`GET /v1/usage`) comes from the API key via `RequireScopeAdmin`; session-based `/v1` resolution is a documented seam. Handlers/middleware injected via `Deps` so the chain is unit-testable with stubs; the single control-plane pool (never a tenant pool — C-3) is opened once in `cli.buildAPIServer`. Sources/documents/jobs/settings/members/api-keys/admin-tenant routes are intentionally unregistered `not_found` seams (04.2–04.6), not stubs.
- [x] error envelope — one SPEC-07 §1 object shape `{"error":{"code","message"}}` (`api.WriteError` also threads `request_id`); router-mounted auth/audit/usage `writeError` converged onto it (fixed an anon `/admin/audit` bare-string body an e2e caught). Unknown route + not-implemented seams return the `not_found` envelope; recovery returns `internal`.
- [x] graceful shutdown — `ragctl serve` (sole entrypoint, ADR-0009) builds the router, runs the rate-limiter idle sweep and the usage-counter flush loop on a `signal.NotifyContext`, and calls `http.Server.Shutdown` on signal. Unit tests (envelope/request-id, `chain` order, `Recover` panic→500 + pass-through, healthz/readyz open, login reaches handler, audit platform-admin 403 + envelope, usage `scope → rate-limit → handler` order, over-limit `429`, unauthenticated `401`, unknown route `404`, seam groups `404`) + e2e golden path over the real control-plane Postgres (`test/e2e/api_router_e2e_test.go`). No migration/schema change (drift guard green). ADR-0027.

### STORY-04.2 — OpenAPI generation and contract tests (SPEC-07 §3) ✅ Done — ADR-0028
- [x] `api/openapi.yaml` generated from code — built in `internal/api/openapi.go` from a single `liveRoutes()` table (mirrors STORY-04.1's mounted routes) and `ErrorCodes()` (the SPEC-07 §1 `Code*` constants); the `ErrorEnvelope` schema matches the `errorEnvelope` Go type and its `code` enum *is* `ErrorCodes()`. Regenerated by `mise run openapi` (`ragctl openapi`, no DB/config); a drift-guard unit test fails CI when the checked-in YAML is stale. No `oapi-codegen`/`swag` toolchain — divergence recorded in ADR-0028; only `gopkg.in/yaml.v3` promoted to a direct dep.
- [x] served at `/v1/openapi.json` — `OpenAPIHandler()` marshals the same in-code document to JSON; mounted open (no auth) alongside the operational endpoints.
- [x] CI validates recorded responses — a jsonschema contract test (unit + e2e golden path over the real control-plane Postgres, `test/e2e/openapi_e2e_test.go`) drives real router error responses (401 scope gate, 404 unknown-route) and validates them against the `ErrorEnvelope` schema extracted from the *served* spec, with a bare-string negative control; both run via `mise run test` / `mise run e2e` in CI.

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
