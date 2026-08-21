# ADR-0013: Observability scaffolding — obs package, private registry, no-op-by-default tracing

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** FR-OBS-01/02/03, SPEC-10, ADR-0003, ADR-0009

## Context
STORY-01.6 introduces the platform's logging, metrics and tracing seam and makes
`ragctl serve` boot a real (if minimal) HTTP server so there is behaviour to
observe. A few choices shape everything built on top and are worth recording
before the full router (STORY-04.1), metric catalogue (STORY-10.1) and span
instrumentation (STORY-10.3) land.

## Options
1. **Global Prometheus default registry** vs a **per-instance registry** owned by
   an `obs.Metrics` value.
2. **Request/tenant identity via context values** (`request_id`, `tenant_id`) vs
   threading a logger explicitly through every call.
3. **Tracing always-on** vs **env-configured, disabled by default**.
4. **A separate serve entrypoint** vs **`ragctl serve` starting the server**.

## Decision
- **Private registry.** `obs.NewMetrics()` owns its own `prometheus.Registry`
  and the `api_request_duration_seconds` histogram, exposed via `MetricsHandler`.
  Tests and multiple in-process servers never fight over the global default
  registry, and registration panics from duplicate metrics are contained.
- **Context carries identity only.** `request_id`/`tenant_id` live in the request
  context; `obs.With(ctx, logger)` pulls them into log records, and the tenant
  label/field is `-` until tenant resolution (STORY-02) sets it. This respects
  ADR-0003: context carries identity, never a DB handle or pool. The tenant is
  never taken from a client parameter (FR-ACC-03) — the middleware leaves the
  seam empty for the authenticated-principal resolver to fill.
- **Tracing is env-configured and disabled by default.** With no
  `OTEL_EXPORTER_OTLP_ENDPOINT`, `SetupTracing` installs only the W3C
  tracecontext+baggage propagator and returns a no-op shutdown, so local dev and
  tests need no collector. An endpoint enables a batched OTLP/gRPC exporter with a
  parent-based ratio sampler.
- **`ragctl serve` is the entrypoint.** Per ADR-0009 the platform is *started*
  through the CLI. After the fail-closed DEK check (STORY-01.4), serve builds the
  logger + tracer + a minimal `http.ServeMux` (`/healthz`, `/readyz`, `/metrics`
  behind the obs middleware) and shuts down gracefully on SIGINT/SIGTERM. No
  separate server binary or flag-parsing path is introduced.

## Consequences
- The serve command no longer returns `ErrNotImplemented`; its exit-2 stub
  assertions moved to the served-HTTP e2e path (updated in the same change, per
  ADR-0010).
- Later stories extend without reshaping the seam: STORY-10.1 registers more
  metrics on the same registry, STORY-10.3 adds spans, STORY-02/09 register real
  `Readyz` `Check`s (control-plane DB, River, provider ping).
- Structured logs go to stderr so they never intermix with a command's stdout.
- Dependency pins: OpenTelemetry SDK v1.31.0 and prometheus/client_golang v1.20.5
  were chosen because they still declare `go 1.22`; newer releases require a newer
  toolchain, which ADR-0002/the repo's Go-1.22 pin forbids bumping here.
