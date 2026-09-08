package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// apiTracer names the API entry-point spans. A no-op tracer is returned until
// SetupTracing installs a provider, so this is safe when tracing is disabled.
var apiTracer = otel.Tracer("ragctl-api")

// tenantLabelUnset is the placeholder tenant label/field used until STORY-02
// resolves the tenant from the authenticated principal (FR-ACC-03). Keeping the
// seam here means the tenant never comes from a client-supplied parameter.
const tenantLabelUnset = "-"

// statusRecorder wraps http.ResponseWriter to capture the status code, defaulting
// to 200 for handlers that write a body without an explicit WriteHeader.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.status = http.StatusOK
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

// Middleware returns net/http middleware that, for each request:
//   - adopts an inbound request id (X-Request-Id, else the W3C traceparent's
//     trace-id) or mints a fresh one, and stashes it in the request context so
//     handlers and downstream loggers can carry it;
//   - echoes the request id on the response (X-Request-Id);
//   - captures the response status, times the request, logs exactly one
//     structured line with duration_ms, and observes the request histogram.
//
// The tenant label/field is left as "-" here: tenant identity is resolved from
// the authenticated principal by a later layer (STORY-02, FR-ACC-03), never from
// this middleware. This keeps the seam without leaking a client-supplied tenant.
func Middleware(log *slog.Logger, m *Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := inboundRequestID(r)
			// Adopt any inbound W3C trace context so a caller's trace continues through
			// the platform, then open the API server span. Its context flows into the
			// handler, so retrieval and provider spans downstream are children — one
			// trace covers API → retrieval → provider (FR-OBS-03, SPEC-10). The span is a
			// no-op until SetupTracing installs a provider, and the sampler ratio
			// (TracingConfig.SamplerRatio) controls capture.
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx = ContextWithRequestID(ctx, reqID)
			// Install a mutable tenant cell a later layer (tenant resolution) writes
			// via SetRequestTenant; read back here for the label/log after the chain
			// returns (see tenantHolder). The tenant never comes from this middleware.
			holder := &tenantHolder{}
			ctx = context.WithValue(ctx, tenantHolderKey, holder)
			// ponytail: the span name is the raw path (matches the metrics route). Ceiling:
			// high-cardinality span names for id-bearing paths. Upgrade path: a routed
			// pattern (chi RoutePattern) once the router exposes it.
			ctx, span := apiTracer.Start(ctx, r.Method+" "+r.URL.Path, trace.WithSpanKind(trace.SpanKindServer))
			defer span.End()
			r = r.WithContext(ctx)
			w.Header().Set("X-Request-Id", reqID)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r)
			elapsed := time.Since(start)

			route := r.URL.Path
			tenant := holder.id
			if tenant == "" {
				tenant = tenantLabelUnset
			}
			// Surface the resolved tenant on the log line too (With reads tenantIDKey).
			ctx = ContextWithTenantID(ctx, tenant)

			span.SetAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.route", route),
				attribute.Int("http.status_code", rec.status),
				attribute.String("tenant", tenant),
			)
			if rec.status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}

			m.ObserveRequest(route, rec.status, tenant, elapsed.Seconds())

			With(ctx, log).LogAttrs(ctx, slog.LevelInfo, "http_request",
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.Int("status", rec.status),
				slog.Int64("duration_ms", elapsed.Milliseconds()),
			)
		})
	}
}

// inboundRequestID reuses a caller-supplied X-Request-Id, else the trace-id from
// a W3C traceparent header (trace continuity across hops), else mints a random
// id. It never trusts an empty/malformed value.
func inboundRequestID(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get("X-Request-Id")); id != "" {
		return id
	}
	if tid := traceIDFromTraceparent(r.Header.Get("traceparent")); tid != "" {
		return tid
	}
	return newRequestID()
}

// traceIDFromTraceparent extracts the 32-hex trace-id from a W3C traceparent
// (version "-" traceid "-" spanid "-" flags), or "" if absent/malformed.
func traceIDFromTraceparent(h string) string {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) < 4 {
		return ""
	}
	traceID := parts[1]
	if len(traceID) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(traceID); err != nil {
		return ""
	}
	return traceID
}

// newRequestID returns a random 128-bit hex id.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is fatal to the process elsewhere; degrade to a
		// timestamp-derived id rather than panic in a request path.
		return "req-" + time.Now().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}
