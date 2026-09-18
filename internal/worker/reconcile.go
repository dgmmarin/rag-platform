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

// reconcileInsertOpts are the InsertOpts for the reconcile_jobs periodic job
// (SPEC-08 §3). River v0.15's PeriodicJobEnqueuer rejects a custom unique ByState
// that omits the required non-terminal states (pending, scheduled, available,
// running) — it logs "UniqueOpts.ByState must contain all required states" and
// never enqueues. syncActiveStates is exactly that non-terminal set, so reuse it:
// a new reconcile is skipped while a prior one is still un-finished (no pile-up),
// which is the intended dedup.
func reconcileInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{
		Queue:      QueueMaintenance,
		UniqueOpts: river.UniqueOpts{ByState: syncActiveStates},
	}
}

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
