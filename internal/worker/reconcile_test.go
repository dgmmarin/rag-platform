package worker

import (
	"context"
	"testing"

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
