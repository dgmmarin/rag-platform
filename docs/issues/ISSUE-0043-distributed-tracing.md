# ISSUE-0043: Distributed tracing end to end

**Type:** Feature · **Status:** Done · **Story:** STORY-10.3 · **Traces:** FR-OBS-03, SPEC-10 §2/§5, SPEC-08 §5, ADR-0013, ADR-0066

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs.

## Summary
Adds the two missing trace edges so leaf provider/sidecar spans join into one trace: an
API server span in the obs HTTP middleware (with W3C context extraction) and a per-job
span in the worker carrying tenant_id/job_id/source_id (SPEC-08 §5). Sampling stays
configurable (existing SamplerRatio).

## Scope
- `internal/obs/middleware.go`: extract inbound trace context, open a `SpanKindServer`
  span with method/route/status/tenant; error on 5xx.
- `internal/worker/tracing.go`: `traceMiddleware` (a `river.WorkerMiddleware`) opening a
  `job:<kind>` span with tenant.id/job.id/job.kind/job.attempt/source.id; registered
  between the limiter and the mirror.
- Docs: ADR-0066, this issue, backlog.
- Not in scope: renaming spans to routed patterns (cardinality ponytail); new sampler
  modes (ratio sampler already configurable).

## Resolution
Provider (embed/llm/rerank) and sidecar spans already existed; the new API and job spans
give them a common ancestor via context propagation, so API → retrieval → provider and
worker job → sidecar each render as one trace.

## Tests
- Unit (`internal/obs`, hermetic, tracetest recorder): an API request records one server
  span with http.method/route/status_code.
- Unit (`internal/worker`, hermetic, tracetest recorder): a job records one span with
  tenant.id/source.id/job.kind; a failing job marks the span Error and passes the error
  through unchanged.
- Build/vet green; full unit suite green.
