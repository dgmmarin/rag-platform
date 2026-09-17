package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// logMiddleware logs every job's start and finish (kind, id, attempt, duration,
// outcome). It is the operator's "the workers are alive" signal: a "job started"
// with no matching "job finished" is a job still running (or, after a crash, one
// that was left stuck — recovered by RescueStuckJobsAfter). It is registered just
// inside the tenant limiter, so "job started" marks work actually beginning, not a
// job snoozing behind the per-tenant concurrency gate.
type logMiddleware struct {
	river.WorkerMiddlewareDefaults
	log *slog.Logger
}

// Work implements rivertype.WorkerMiddleware.
func (m logMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	tenantID, sourceID := jobIdentity(job.EncodedArgs)
	base := []any{
		"kind", job.Kind, "river_job_id", job.ID, "attempt", job.Attempt,
		"max_attempts", job.MaxAttempts, "tenant_id", tenantID, "source_id", sourceID,
	}
	m.log.Info("job started", append([]any{"event", "started", "status", "running"}, base...)...)

	start := time.Now()
	err := doInner(ctx)
	durMs := time.Since(start).Milliseconds()
	fields := append([]any{"duration_ms", durMs}, base...)

	switch {
	case err == nil:
		m.log.Info("job finished", append([]any{"event", "finished", "status", "succeeded"}, fields...)...)
	case errors.Is(err, context.Canceled):
		m.log.Info("job cancelled", append([]any{"event", "cancelled", "status", "cancelled"}, fields...)...)
	default:
		m.log.Warn("job failed", append([]any{"event", "failed", "status", "failed", "err", err.Error()}, fields...)...)
	}
	return err
}
