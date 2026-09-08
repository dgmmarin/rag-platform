package worker

import (
	"context"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/rag-platform/ragctl/internal/obs"
)

// metricsMiddleware is a river.WorkerMiddleware that records jobs_duration_seconds
// per kind for every worked job and jobs_failed_total per kind for a job that
// ends in a terminal handler error (SPEC-10 §2/§5). It never alters the job's
// outcome (the queue stays authoritative) and is a no-op when metrics are
// disabled (nil m).
type metricsMiddleware struct {
	river.WorkerMiddlewareDefaults
	m *obs.Metrics
}

// Work times the inner handler and records the outcome under the job kind.
func (mw metricsMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	start := time.Now()
	err := doInner(ctx)
	mw.m.ObserveJob(job.Kind, time.Since(start).Seconds())
	if err != nil {
		mw.m.IncJobFailed(job.Kind)
	}
	return err
}
