package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// TracingConfig configures the OpenTelemetry tracer from the environment
// (SPEC-10 §3). All exporter settings are optional so local dev needs nothing:
// with no OTLPEndpoint tracing is disabled (a no-op provider), and W3C trace
// context propagation is still installed for header continuity.
type TracingConfig struct {
	// Service names the tracer's resource (service.name).
	Service string
	// OTLPEndpoint is the OTLP/gRPC collector endpoint (host:port). Empty ⇒ tracing
	// disabled.
	OTLPEndpoint string
	// SamplerRatio is the parent-based ratio sampler fraction (0..1). Values <=0
	// with an endpoint set fall back to AlwaysSample so a configured exporter is
	// not silently muted; a ratio is only meaningful when explicitly chosen.
	SamplerRatio float64
	// Insecure sends OTLP over plaintext (typical for a local collector).
	Insecure bool
}

// ShutdownFunc flushes and stops tracing; callers defer it on graceful shutdown.
type ShutdownFunc func(context.Context) error

// SetupTracing installs the global W3C tracecontext+baggage propagator and, when
// an OTLP endpoint is configured, a batched OTLP/gRPC exporter with a
// parent-based ratio sampler. It returns a shutdown hook that is always non-nil,
// so callers can defer it unconditionally.
//
// When disabled (no endpoint) it leaves the global no-op tracer provider in
// place and returns a no-op shutdown — nothing is required for local dev.
func SetupTracing(ctx context.Context, cfg TracingConfig) (ShutdownFunc, error) {
	// Propagation is installed in every mode so request/trace context flows over
	// standard headers even when export is disabled.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.OTLPEndpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return func(context.Context) error { return nil }, err
	}

	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceName(cfg.Service),
		),
	)
	if err != nil {
		return func(context.Context) error { return nil }, err
	}

	sampler := sdktrace.AlwaysSample()
	if cfg.SamplerRatio > 0 {
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplerRatio))
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}
