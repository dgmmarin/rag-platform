# ADR-0066: Distributed tracing end to end — a server span at the API edge and a per-job span in the worker, joining the existing provider spans into one trace

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-OBS-03, SPEC-10 §2/§5, SPEC-08 §5 · **Decisions:** ADR-0013

## Context
FR-OBS-03: one trace must cover API → retrieval → provider, and a worker job → sidecar,
sampled at a configurable rate. The OTel provider, W3C propagation and a parent-based
ratio sampler were installed by STORY-01.6/ADR-0013, and the leaf callers already open
spans (embedding, LLM, reranker, sidecar). Two edges were missing spans, so those leaf
spans had no common ancestor: the HTTP entry point and the worker job.

## Options / decisions
- **The API middleware opens a server span per request.** It first `Extract`s any inbound
  W3C trace context (so a caller's trace continues through the platform), then starts a
  `SpanKindServer` span whose context flows into the handler — so the retrieval and
  provider spans made downstream are its children, giving one API → retrieval → provider
  trace. The span carries `http.method`, `http.route`, `http.status_code` and the resolved
  `tenant`, and is marked error on 5xx. Manual `otel.Tracer` spans are used rather than
  adding an `otelhttp` dependency (none is present, and the middleware already owns the
  request lifecycle). ponytail: the span name is the raw path (matches the metrics route),
  high-cardinality for id-bearing paths — upgrade path is a routed pattern.
- **A worker `traceMiddleware` opens a span per job.** Registered between the tenant
  limiter and the mirror (so a snooze precedes it, and the span parents the mirror writes
  and the handler), it carries `tenant.id`, `job.id`, `job.kind`, `job.attempt` and
  `source.id` (SPEC-08 §5), records the handler error, and passes the error through
  unchanged. Its context flows into the handler, so the sidecar/provider spans a job makes
  are children — one worker job → sidecar trace.
- **Sampling stays configuration-driven.** The parent-based `TraceIDRatioBased` sampler
  (TracingConfig.SamplerRatio / env) already governs capture; both new spans are no-ops
  until a provider is installed, so tracing off costs nothing.

## Consequences
- A request and the work it triggers are each a single connected trace from edge to
  provider/sidecar, with tenant/job identity on the spans for per-tenant triage.
- No new dependency; the middleware and worker own their spans.
- The API span name is path-based for now, a documented cardinality ceiling.
