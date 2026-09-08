package worker

import (
	"context"
	"errors"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/riverqueue/river/rivertype"
)

// recordSpans installs a recording tracer provider and points jobTracer at it for the
// duration of a test, returning the recorder.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := jobTracer
	jobTracer = tp.Tracer("worker-test")
	t.Cleanup(func() { jobTracer = prev })
	return sr
}

// The job span carries tenant_id and source_id (SPEC-08 §5) and names the kind.
func TestJobTraceMiddlewareRecordsSpanWithIdentity(t *testing.T) {
	sr := recordSpans(t)
	job := &rivertype.JobRow{ID: 7, Kind: "sync_source", Attempt: 1,
		EncodedArgs: []byte(`{"tenant_id":"t-1","source_id":"s-1"}`)}
	ran := false
	if err := (traceMiddleware{}).Work(context.Background(), job, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("Work err=%v ran=%v", err, ran)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if spans[0].Name() != "job:sync_source" {
		t.Fatalf("span name = %q, want job:sync_source", spans[0].Name())
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["tenant.id"] != "t-1" || attrs["source.id"] != "s-1" || attrs["job.kind"] != "sync_source" {
		t.Fatalf("attrs = %v, want tenant.id=t-1 source.id=s-1 job.kind=sync_source", attrs)
	}
}

// A failing job records the error on its span (and passes the error through unchanged).
func TestJobTraceMiddlewareRecordsError(t *testing.T) {
	sr := recordSpans(t)
	job := &rivertype.JobRow{ID: 1, Kind: "gc_tenant", EncodedArgs: []byte(`{"tenant_id":"t"}`)}
	want := errors.New("boom")
	if err := (traceMiddleware{}).Work(context.Background(), job, func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("Work returned %v, want the handler error unchanged", err)
	}
	spans := sr.Ended()
	if len(spans) != 1 || spans[0].Status().Code.String() != "Error" {
		t.Fatalf("span status = %v, want Error", spans)
	}
}
