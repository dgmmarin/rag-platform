package worker

import (
	"context"
	"testing"

	"github.com/riverqueue/river/rivertype"

	"github.com/rag-platform/ragctl/internal/cp/jobs"
)

// fakeReconcileStore is a jobs.Store stub that records whether Reconcile ran and
// returns zero healed rows, no error.
type fakeReconcileStore struct {
	jobs.Store
	calls int
}

func (f *fakeReconcileStore) Reconcile(ctx context.Context) ([]jobs.Reconciled, error) {
	f.calls++
	return nil, nil
}

// TestReconcileWorkerRunsReconciler: reconcileWorker.Work calls jobs.Reconciler.
// Reconcile and returns its error (nil on an empty reconcile).
func TestReconcileWorkerRunsReconciler(t *testing.T) {
	store := &fakeReconcileStore{}
	w := &reconcileWorker{recon: jobs.Reconciler{Store: store}}

	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() = %v, want nil", err)
	}
	if store.calls != 1 {
		t.Fatalf("Store.Reconcile called %d times, want 1", store.calls)
	}
}

// TestReconcileInsertOptsHasRequiredUniqueStates guards the ISSUE-0081 fix: River's
// PeriodicJobEnqueuer rejects a custom unique ByState missing any required
// non-terminal state (pending, scheduled, available, running) and then never
// enqueues reconcile_jobs. This asserts the wired opts include all four, so the
// periodic reconcile keeps running.
func TestReconcileInsertOptsHasRequiredUniqueStates(t *testing.T) {
	got := map[rivertype.JobState]bool{}
	for _, s := range reconcileInsertOpts().UniqueOpts.ByState {
		got[s] = true
	}
	for _, want := range []rivertype.JobState{
		rivertype.JobStatePending,
		rivertype.JobStateScheduled,
		rivertype.JobStateAvailable,
		rivertype.JobStateRunning,
	} {
		if !got[want] {
			t.Fatalf("reconcile unique ByState missing required state %q; River will refuse to enqueue", want)
		}
	}
}
