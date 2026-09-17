package worker

import (
	"context"

	"github.com/riverqueue/river"

	"github.com/rag-platform/ragctl/internal/cp/jobs"
)

// ReconcileJobsArgs triggers one mirror-reconciliation pass. It carries no data:
// the reconciler operates over the whole control-plane jobs table.
type ReconcileJobsArgs struct{}

func (ReconcileJobsArgs) Kind() string { return "reconcile_jobs" }

// reconcileWorker runs the control-plane jobs reconciler (SPEC-08 §3). Registered
// on the maintenance queue so it never competes with ingest/sync work.
type reconcileWorker struct {
	river.WorkerDefaults[ReconcileJobsArgs]
	recon jobs.Reconciler
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileJobsArgs]) error {
	_, err := w.recon.Reconcile(ctx)
	return err
}
