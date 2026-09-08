package worker

import (
	"context"
	"encoding/json"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// jobTracer names the worker job spans. It is a no-op until SetupTracing installs a
// provider, so it is safe when tracing is disabled.
var jobTracer = otel.Tracer("ragctl-worker")

// traceMiddleware is a river.WorkerMiddleware that opens one span per job carrying
// tenant_id, job_id and source_id (SPEC-08 §5, FR-OBS-03). Its context flows into the
// handler, so the sidecar/provider spans a job makes downstream are children — one
// trace covers a worker job → sidecar. It records the handler's error on the span but
// never alters it (the queue stays authoritative).
type traceMiddleware struct {
	river.WorkerMiddlewareDefaults
}

// Work wraps job execution in a span.
func (traceMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	tenantID, sourceID := jobIdentity(job.EncodedArgs)
	ctx, span := jobTracer.Start(ctx, "job:"+job.Kind, trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()
	span.SetAttributes(
		attribute.String("job.kind", job.Kind),
		attribute.String("job.id", strconv.FormatInt(job.ID, 10)),
		attribute.Int("job.attempt", job.Attempt),
		attribute.String("tenant.id", tenantID),
	)
	if sourceID != "" {
		span.SetAttributes(attribute.String("source.id", sourceID))
	}
	err := doInner(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// jobIdentity pulls tenant_id and (when present) source_id from a job's encoded args.
func jobIdentity(encoded []byte) (tenantID, sourceID string) {
	var a struct {
		TenantID string `json:"tenant_id"`
		SourceID string `json:"source_id"`
	}
	_ = json.Unmarshal(encoded, &a)
	return a.TenantID, a.SourceID
}
