# ISSUE-0015: Connector interface, registry and config validation

**Type:** Feature · **Status:** Done · **Story:** STORY-06.1 · **Traces:** FR-SRC-13, FR-SRC-14, NFR-MNT-01, SPEC-04 §1/§7, ADR-0040

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-06.1 for traceability; the backlog
> story remains the authoritative work item. STORY-06.1 starts EPIC-06 (5/13).

## Summary
The ingestion connector framework: the single interface every source kind
implements (FR-SRC-13), a registry keyed by kind, and JSON-Schema config
validation (SPEC-04 §1/§7) — and the wiring of the kind-specific `ValidateConfig`/
`Test` into the sources API `Validator` seam that STORY-04.3 (ADR-0029) left nil,
so adding a connector needs no change outside its package + registration
(NFR-MNT-01).

## Scope
- `internal/connector/connector.go`: the full SPEC-04 §1 `Connector` interface
  (`Kind`/`ValidateConfig`/`Test`/`Sync`) and its supporting types (`Kind`
  constants, `Credentials`, `Document`, `Sink`, `SyncRun`, `StateStore`, `Stats`,
  `ErrUnsupportedKind`).
- `internal/connector/registry.go`: `Registry` (factory-per-kind, panic on nil/
  duplicate, sorted `Kinds`), process-wide `DefaultRegistry()` with package-level
  `Register`/`Lookup`, and the `SourcesValidator` adapter for the sources seam.
- `internal/connector/schema.go`: `SchemaValidator` + `ConfigError`/`FieldError`
  (JSON-Schema config validation, reusing the STORY-03.5/ADR-0022 pattern over
  `santhosh-tekuri/jsonschema/v6`).
- `internal/cli/api_server.go`: wire
  `connector.NewSourcesValidator(connector.DefaultRegistry(), sources.ErrConnectorUnavailable)`
  onto the sources service.
- Not in scope: credential encryption/handling (STORY-06.2), the upload connector
  and `ingest_document` job (STORY-06.3), and `Connector.Sync` / any concrete
  connector (EPIC-07).

## Resolution
- **Full interface, built to spec.** SPEC-04 §1 is authoritative and NFR-MNT-01
  wants the contract frozen, so the whole `Connector` interface is transcribed
  (including `Sync`), giving EPIC-07 a stable target. No connector is implemented,
  so this is the named deliverable, not scope creep. `StateStore` (Get/Set) and
  `Stats` (enumeration counters) are minimal and documented provisional — only
  `Sync` (EPIC-07) exercises them, and the canonical jobs.stats shape is SPEC-05 §6.
- **Registry.** A `func() Connector` factory per kind (fresh instance per Lookup,
  since a connector may hold per-sync state); duplicate/nil registration panics
  (init-time misconfiguration fails loudly). `DefaultRegistry()` backs the
  package-level `Register`/`Lookup` connectors use from `init()`.
- **Config validation.** `SchemaValidator` compiles a JSON Schema once and returns
  a sorted `*ConfigError` field list, mirroring tenant-settings validation — one
  validation approach, no new dependency.
- **Seam wiring without coupling.** `SourcesValidator` satisfies the sources
  `Validator` port *structurally*; the sources package keeps no connector import,
  and the dependency is injected in `internal/cli`. For an unregistered kind
  `ValidateConfig` returns nil (defer to generic validation; a source whose
  connector is not built yet can still be created) and `Test` returns the injected
  `sources.ErrConnectorUnavailable` sentinel (so `/test` keeps the not_found seam),
  rather than 500-ing or faking a 200 (AGENTS.md Integrity).
- **Dependency.** `golang.org/x/time` v0.3.0 added (direct) for
  `SyncRun.Limiter *rate.Limiter` per SPEC-04 §1.

## Verification
- TDD: `internal/connector/{schema,registry,validator}_test.go` written and
  watched **red** (undefined `Kind`/`SchemaValidator`/`Registry`/… — build failed
  for the right reason) then **green**. Cover the schema validator (valid/missing-
  required/unknown-property/wrong-type/non-object/bad-JSON/bad-schema), the registry
  (lookup found/unknown, fresh-instance-per-lookup, duplicate/nil panic, sorted
  Kinds, package-level Register/Lookup), and the `SourcesValidator` (delegate/defer/
  propagate/unavailable). `go test -cover ./internal/connector/` = **85.4%** (gate
  is 70% on `internal/connector`).
- e2e (`test/e2e/connector_e2e_test.go`, build tag `e2e`) against the **real
  control-plane Postgres** (via `mise run up`) and the **real** `internal/api`
  router over a real listener: registers a fake `web_crawl` connector into a fresh
  registry, wires the real `SourcesValidator` into the sources service, and drives
  the API-key admin chain — invalid connector config → **400**, valid → **201**
  (persisted), `/test` → **200** with the connector's `Test` invoked exactly once,
  and an unregistered kind (`api`) → **201** (deferred validation) with `/test` →
  **404** seam. Ran **green in ~19 s**.
- `go build ./...` clean; `go vet ./internal/connector/... ./internal/cli/...` and
  `go vet -tags e2e ./test/e2e/...` clean; `gofmt -l` clean; full unit suite
  (`go test ./...`) green with a clean environment; `go mod verify` OK.

## Notes / not in scope
- No schema/migration change and no new HTTP route (the `/test` route already
  existed), so `schemas/*.sql`, the STORY-01.5 drift guard and `api/openapi.yaml`
  are unchanged (no `mise run openapi` needed).
- The `internal/connector` package touches no database, object storage or network,
  so the `Unsafe()` forbidigo ban is trivially satisfied.
- Ceiling (ponytail): `StateStore`/`Stats` are minimal placeholders; their concrete
  shapes are finalised when the first connector implements `Sync` (EPIC-07). This is
  confined to `internal/connector` and its connectors.
- **Pre-existing environment caveats (not this change):**
  - `docker compose exec` is wedged in this environment (a bare
    `docker compose exec -T postgres psql -c 'select 1'` is killed after the
    timeout), so `TestSourcesGoldenPath` and `TestTenantIsolationSuite` — which
    assert via `docker compose exec` — cannot complete here (documented already in
    ISSUE-0014). The connector e2e avoids it (control pool + HTTP only) and ran
    green. This story touched none of `internal/tenant`/`internal/api`/
    `internal/worker`, so the isolation suite is not triggered by it.
  - `golangci-lint` v2.13.1 requires Go ≥ 1.26 while the toolchain is 1.22.12, and
    no binary is on PATH, so `mise run lint` falls back to `go vet` (its offline
    fallback), which is clean; `internal/cli` unit tests fail only under mise's
    `.env` injection (leaked `CONTROL_PLANE_URL`/age key) and pass with a clean
    environment. Both pre-date this change.
