package obs

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
)

// TestSetupTracingDisabledByDefault proves that with no OTLP endpoint configured
// tracing is a no-op: SetupTracing succeeds and returns a shutdown hook, so local
// dev needs nothing (SPEC-10 §3).
func TestSetupTracingDisabledByDefault(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), TracingConfig{Service: "ragctl"})
	if err != nil {
		t.Fatalf("SetupTracing (disabled): %v", err)
	}
	if shutdown == nil {
		t.Fatal("SetupTracing must return a non-nil shutdown hook even when disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown (disabled): %v", err)
	}
}

// TestSetupTracingInstallsW3CPropagator proves the global propagator is the W3C
// tracecontext (+ baggage) so trace context flows over standard headers, which is
// required even in the disabled/local case for request-id continuity.
func TestSetupTracingInstallsW3CPropagator(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), TracingConfig{Service: "ragctl"})
	if err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	prop := otel.GetTextMapPropagator()
	fields := prop.Fields()
	hasTraceparent := false
	for _, f := range fields {
		if f == "traceparent" {
			hasTraceparent = true
		}
	}
	if !hasTraceparent {
		t.Fatalf("global propagator missing W3C traceparent field, got %v", fields)
	}
	// It must be a composite that also carries baggage.
	hasBaggage := false
	for _, f := range fields {
		if f == "baggage" {
			hasBaggage = true
		}
	}
	if !hasBaggage {
		t.Fatalf("global propagator missing baggage field, got %v", fields)
	}
}

// TestSetupTracingEnabledWithEndpoint proves that configuring an OTLP endpoint
// wires an exporter without error (no collector needs to be running; export is
// lazy/batched).
func TestSetupTracingEnabledWithEndpoint(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), TracingConfig{
		Service:      "ragctl",
		OTLPEndpoint: "localhost:4317",
		SamplerRatio: 0.5,
	})
	if err != nil {
		t.Fatalf("SetupTracing (enabled): %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown (enabled): %v", err)
	}
}
