# ADR-0040: Connector framework — the SPEC-04 §1 interface, a factory registry, and a JSON-Schema config validator wired into the sources seam

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-SRC-13, FR-SRC-14, NFR-MNT-01, FR-ACC-03, C-3, C-4, SPEC-04 §1/§7 · **Decisions:** ADR-0003, ADR-0022, ADR-0028, ADR-0029

## Context
STORY-06.1 must deliver the connector framework: the common interface every
source kind implements (FR-SRC-13), a registry keyed by kind, and config
validation — and wire the kind-specific `ValidateConfig`/`Test` into the seam
STORY-04.3 left open (ADR-0029): the sources package's nil `Validator` port, where
`/test` returns the not_found seam envelope and create/update run generic
validation only.

Out of scope (separate stories): credential encryption/handling (STORY-06.2), the
upload connector and the `ingest_document` job (STORY-06.3), and every crawling/API
connector plus `Connector.Sync` itself (EPIC-07).

Two facts shape the design:

- SPEC-04 §1 is the authoritative interface — `Connector` with
  `Kind`/`ValidateConfig`/`Test`/`Sync`, plus `Document`, `Sink`, `SyncRun`,
  `Credentials`, `StateStore` and `Stats`. It fully specifies `Document`, `Sink`
  and `SyncRun`; it references `StateStore` (methods) and `Stats` (fields) without
  detailing them.
- The sources `Validator` seam (ADR-0029) is a local, injected interface
  (`ValidateConfig(kind, cfg)`, `Test(ctx, kind, cfg)`) so the sources package
  carries no connector import. The seam's `Test` takes no credentials; ADR-0029
  anticipated EPIC-06 supplying the registry and extending `Test` with decrypted
  credentials in STORY-06.2.

## Options
- **How much of the interface to define now.** (a) A reduced interface
  (`Kind`/`ValidateConfig`/`Test`) that EPIC-07 later extends with `Sync` —
  rejected: SPEC-04 §1 is authoritative and NFR-MNT-01 wants the contract frozen so
  EPIC-07 adds a connector with no framework change; extending the core interface
  when a connector lands is exactly the churn NFR-MNT-01 forbids. (b) The full
  SPEC-04 §1 interface, transcribed faithfully (chosen). No connector is
  implemented, so this is not scope creep — it is the named deliverable ("the
  connector interface").
- **The under-specified `StateStore`/`Stats`.** (a) Invent rich shapes now —
  rejected as speculative surface for types only `Sync` (EPIC-07) exercises. (b) A
  minimal, spec-grounded realisation, documented as provisional (chosen):
  `StateStore` gets `Get`/`Set` (SPEC-04 §1's "per-source key/value: cursor, etag
  cache"); `Stats` gets the connector-reported enumeration counters (the canonical
  jobs.stats shape is SPEC-05 §6 / `internal/ingest/sink.Stats`). Both are finalised
  when the first `Sync` lands.
- **Registry shape.** (a) A shared connector instance per kind — rejected: a
  connector may hold per-sync state. (b) A `func() Connector` factory per kind
  (chosen, SPEC-04 §1), fresh instance per `Lookup`. Duplicate/nil registration
  panics: registration is init-time, so a misconfiguration must fail at startup,
  not silently at request time.
- **Config validation.** Reuse `santhosh-tekuri/jsonschema/v6` and the
  leaf-error/field-path pattern already used for tenant settings (STORY-03.5,
  ADR-0022) rather than a second validation approach. `SchemaValidator` +
  `ConfigError`/`FieldError` are the shared helper connectors call from
  `ValidateConfig` (SPEC-04 §7 step 2). No new dependency for this.
- **Bridging the sources seam without coupling.** (a) Make the sources package
  import the connector framework — rejected: it breaks ADR-0029's injected,
  connector-free seam. (b) A `SourcesValidator` adapter in the connector package
  that satisfies the sources `Validator` method set *structurally*, injected at
  the composition root (`internal/cli`, which already imports both) (chosen).
- **Unregistered-kind behaviour (no connector built yet).** `ValidateConfig`
  returns nil (defer to generic validation) so a source whose connector does not
  exist can still be created; `Test` returns an injected sentinel
  (`sources.ErrConnectorUnavailable`, passed into `NewSourcesValidator`) so `/test`
  keeps reporting the not_found seam — preserving the established behaviour and the
  STORY-04.3 e2e — rather than 500-ing or falsely returning 200 ok (AGENTS.md
  Integrity: no fake success). Passing the sentinel in (rather than the connector
  package importing sources) keeps the dependency direction clean.
- **`rate.Limiter` for `SyncRun.Limiter`.** SPEC-04 §1 names `*rate.Limiter`, so
  `golang.org/x/time/rate` is added (a `golang.org/x` package, consistent with the
  existing `x/net`/`x/oauth2`/`x/text` deps; pinned at v0.3.0 for go 1.22).

## Decision
Add `internal/connector`: the full SPEC-04 §1 `Connector` interface and its
supporting types; a `Registry` (factory-per-kind, panic on dup/nil, sorted
`Kinds`) with a process-wide `DefaultRegistry()` behind package-level
`Register`/`Lookup`; a `SchemaValidator`/`ConfigError` JSON-Schema helper reusing
the STORY-03.5 pattern; and a `SourcesValidator` adapter that delegates to the
registry, defers config validation for unregistered kinds, and returns an injected
"unavailable" sentinel from `Test` for unregistered kinds. `internal/cli`
(`buildAPIServer`) wires `connector.NewSourcesValidator(connector.DefaultRegistry(),
sources.ErrConnectorUnavailable)` onto the sources service.

`StateStore` and `Stats` are minimal, documented as provisional pending `Sync`
(EPIC-07). No connector is registered in v1 yet.

## Consequences
- The connector interface, registry and config validation exist and are unit-
  tested (85% coverage, above the 70% gate on `internal/connector`), and the
  seam is proven end to end by an e2e (`test/e2e/connector_e2e_test.go`) that
  registers a fake `web_crawl` connector, wires the real `SourcesValidator` into
  the real router over the real control-plane Postgres, and shows an invalid
  config → 400, a valid config → 201, `/test` → the connector's Test, and an
  unregistered kind → deferred validation + the `/test` seam.
- Wiring a real connector (STORY-06.3 upload, EPIC-07 crawl/api) is its package +
  one `Register` call in an `init()` — no change to `internal/connector`, the
  sources package, or the router (NFR-MNT-01). Its kind-specific `ValidateConfig`
  and `Test` then activate automatically at the sources API.
- `/test` behaviour is unchanged for kinds without a connector (still the not_found
  seam), so the STORY-04.3 sources e2e stays valid; credentials are still not
  threaded into `Test` (STORY-06.2), and no credential ever reaches this package
  (C-4). Sources remain control-plane registry data (C-3); this package touches no
  database (ADR-0003).
- New direct dependency `golang.org/x/time` (rate limiter type on `SyncRun`).
- No schema/migration change and no new HTTP route (the `/test` route already
  existed), so `schemas/*.sql`, the drift guard and `api/openapi.yaml` are
  unchanged.
- `StateStore`/`Stats` may change shape when the first `Sync` is implemented; that
  is confined to `internal/connector` and its (then-existing) connectors.
