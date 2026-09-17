package worker

import (
	"context"
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
	m.log.Info("job started",
		"kind", job.Kind, "job_id", job.ID, "attempt", job.Attempt,
		"max_attempts", job.MaxAttempts, "tenant_id", tenantID, "source_id", sourceID)

	start := time.Now()
	err := doInner(ctx)
	durMs := time.Since(start).Milliseconds()

	if err != nil {
		m.log.Warn("job finished",
			"kind", job.Kind, "job_id", job.ID, "attempt", job.Attempt,
			"duration_ms", durMs, "err", err.Error())
	} else {
		m.log.Info("job finished",
			"kind", job.Kind, "job_id", job.ID, "attempt", job.Attempt, "duration_ms", durMs)
	}
	return err
}
