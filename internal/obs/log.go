// Package obs provides the platform's observability scaffolding (STORY-01.6,
// FR-OBS-01/02/03, SPEC-10): structured JSON logging, Prometheus metrics, an
// OpenTelemetry tracer configured by env, HTTP middleware that ties them
// together, and health/readiness handlers.
//
// The package is deliberately a thin, reusable seam. Later stories layer real
// behaviour on top: STORY-04.1 the full router/middleware chain, STORY-10.1 the
// metric catalogue, STORY-10.3 span instrumentation of request paths, and
// STORY-02 tenant resolution (which fills the tenant_id/tenant label this layer
// leaves empty).
//
// Content fields (question text, document bodies) are never logged here; per
// SPEC-10 §1 they must not appear at info level.
package obs

import (
	"context"
	"io"
	"log/slog"
)

// Logger returns a JSON slog.Logger (the production/machine format, SPEC-10 §1).
// It is the default and what tests and log-scraping tooling expect; NewLogger
// selects a human-readable console format when asked.
func Logger(service string, level slog.Level, w io.Writer) *slog.Logger {
	return NewLogger(service, level, "json", w)
}

// NewLogger returns a slog.Logger writing to w at the given level, tagged with the
// mandatory `service` base field (SPEC-10 §1). format selects the handler:
//
//   - "text" / "console" — one line of key=value pairs per record, for a human
//     reading the local stack in mprocs; much easier to scan than JSON.
//   - anything else (incl. "json" and "") — structured JSON, the machine format.
//
// It uses slog's built-in handlers only: the go.mod go-1.22 pin (ADR-0014) rules
// out a third-party pretty-logger such as zerolog, which requires a newer Go and a
// larger dependency set. The remaining mandatory fields (request_id, tenant_id,
// duration_ms, err, …) are attached per-record by callers.
func NewLogger(service string, level slog.Level, format string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch format {
	case "text", "console":
		handler = slog.NewTextHandler(w, opts)
	default:
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler).With(slog.String("service", service))
}

// ParseLevel maps a textual level (debug, info, warn, error) to slog.Level,
// defaulting to info for the empty or unrecognised value so a misconfigured
// LOG_LEVEL never silences logging.
func ParseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ctxKey is an unexported context key type so obs's context values cannot
// collide with any other package's keys.
type ctxKey int

const (
	requestIDKey ctxKey = iota
	tenantIDKey
	tenantHolderKey
)

// tenantHolder is a request-scoped, mutable cell holding the resolved tenant
// identifier. The obs middleware runs OUTERMOST and reads the label AFTER the
// handler chain returns, but context values a later layer sets on its own child
// context are invisible to that outer snapshot. A pointer in the context lets a
// later layer (tenant resolution) write the tenant back where the outer
// middleware can read it, without a client-supplied tenant ever leaking in.
type tenantHolder struct{ id string }

// SetRequestTenant records the resolved tenant identifier for the current
// request so the obs middleware labels the request histogram and log line with
// it (SPEC-10 §2, FR-OBS-02). It is a no-op outside an obs-instrumented request
// (no holder in context), so callers need no guard.
func SetRequestTenant(ctx context.Context, id string) {
	if h, ok := ctx.Value(tenantHolderKey).(*tenantHolder); ok {
		h.id = id
	}
}

// ContextWithRequestID returns a child context carrying the request id. Context
// carries request/tenant *identity* only — never a DB handle or pool (ADR-0003).
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request id, or "" if none is set.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// ContextWithTenantID returns a child context carrying the resolved tenant slug
// (the low-cardinality identifier used as the metric/log tenant field).
func ContextWithTenantID(ctx context.Context, slug string) context.Context {
	return context.WithValue(ctx, tenantIDKey, slug)
}

// TenantIDFromContext returns the tenant slug, or "" if none is set.
func TenantIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantIDKey).(string); ok {
		return v
	}
	return ""
}

// With returns a logger enriched with the known identity fields carried by ctx:
// request_id and tenant_id are attached only when present, so log lines stay
// clean before tenant resolution (STORY-02) sets them.
func With(ctx context.Context, log *slog.Logger) *slog.Logger {
	var attrs []any
	if id := RequestIDFromContext(ctx); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	if slug := TenantIDFromContext(ctx); slug != "" {
		attrs = append(attrs, slog.String("tenant_id", slug))
	}
	if len(attrs) == 0 {
		return log
	}
	return log.With(attrs...)
}
