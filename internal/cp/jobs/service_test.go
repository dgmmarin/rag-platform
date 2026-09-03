package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeStore is an in-memory Store for the service tests. It mimics the
// control-plane jobs semantics the PoolDB enforces in SQL (tenant scoping, a
// queued-only guarded cancel) without a database.
type fakeStore struct {
	seq       int
	jobs      map[string]Job // id -> job
	failOn    string         // method name to force an error on
	cancelled []string       // ids passed to CancelQueued that changed
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: map[string]Job{}} }

func (f *fakeStore) add(j Job) Job {
	f.seq++
	if j.ID == "" {
		j.ID = "job-" + itoa(f.seq)
	}
	if j.QueuedAt.IsZero() {
		j.QueuedAt = time.Unix(1_700_000_000+int64(f.seq), 0).UTC()
	}
	f.jobs[j.ID] = j
	return j
}

func itoa(n int) string { return string(rune('0' + n%10)) }

func (f *fakeStore) List(_ context.Context, tenantID string, flt ListFilter, limit int, cur *Cursor) ([]Job, error) {
	if f.failOn == "List" {
		return nil, errors.New("boom")
	}
	var out []Job
	for _, j := range f.jobs {
		if j.TenantID != tenantID {
			continue
		}
		if flt.Status != "" && j.Status != flt.Status {
			continue
		}
		if flt.Kind != "" && j.Kind != flt.Kind {
			continue
		}
		if flt.SourceID != "" && (j.SourceID == nil || *j.SourceID != flt.SourceID) {
			continue
		}
		out = append(out, j)
	}
	sortJobsDesc(out)
	if cur != nil {
		var filtered []Job
		for _, j := range out {
			if j.QueuedAt.Before(cur.QueuedAt) || (j.QueuedAt.Equal(cur.QueuedAt) && j.ID < cur.ID) {
				filtered = append(filtered, j)
			}
		}
		out = filtered
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func sortJobsDesc(js []Job) {
	for i := 1; i < len(js); i++ {
		for k := i; k > 0; k-- {
			a, b := js[k-1], js[k]
			if a.QueuedAt.Before(b.QueuedAt) || (a.QueuedAt.Equal(b.QueuedAt) && a.ID < b.ID) {
				js[k-1], js[k] = js[k], js[k-1]
			}
		}
	}
}

func (f *fakeStore) Get(_ context.Context, tenantID, id string) (Job, error) {
	if f.failOn == "Get" {
		return Job{}, errors.New("boom")
	}
	j, ok := f.jobs[id]
	if !ok || j.TenantID != tenantID {
		return Job{}, ErrNotFound
	}
	return j, nil
}

func (f *fakeStore) CancelQueued(_ context.Context, tenantID, id string) (Job, bool, error) {
	if f.failOn == "CancelQueued" {
		return Job{}, false, errors.New("boom")
	}
	j, ok := f.jobs[id]
	if !ok || j.TenantID != tenantID || j.Status != StatusQueued {
		return Job{}, false, nil
	}
	j.Status = StatusCancelled
	now := time.Unix(1_700_100_000, 0).UTC()
	j.FinishedAt = &now
	f.jobs[id] = j
	f.cancelled = append(f.cancelled, id)
	return j, true, nil
}

// fakeCanceller records running-job cancellation signals (the EPIC-09 River seam).
type fakeCanceller struct {
	called []string
	err    error
}

func (c *fakeCanceller) Cancel(_ context.Context, _, jobID string) error {
	c.called = append(c.called, jobID)
	return c.err
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestListRequiresTenant(t *testing.T) {
	svc := NewService(newFakeStore())
	if _, err := svc.List(context.Background(), ListParams{}); err == nil {
		t.Fatal("List with no tenant should fail closed")
	}
}

func TestListPaginates(t *testing.T) {
	fs := newFakeStore()
	for i := 0; i < 3; i++ {
		fs.add(Job{TenantID: "t1", Kind: "sync_source", Status: StatusQueued})
	}
	svc := NewService(fs)
	page, err := svc.List(context.Background(), ListParams{TenantID: "t1", Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next_cursor when more rows remain")
	}
	// Second page.
	page2, err := svc.List(context.Background(), ListParams{TenantID: "t1", Limit: 2, Cursor: page.NextCursor})
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2.Items) != 1 {
		t.Fatalf("page2 items = %d, want 1", len(page2.Items))
	}
	if page2.NextCursor != "" {
		t.Fatal("expected no next_cursor on the last page")
	}
}

func TestListFiltersValidated(t *testing.T) {
	svc := NewService(newFakeStore())
	if _, err := svc.List(context.Background(), ListParams{TenantID: "t1", Filter: ListFilter{Status: "bogus"}}); err == nil {
		t.Fatal("invalid status filter should be a validation error")
	}
	if _, err := svc.List(context.Background(), ListParams{TenantID: "t1", Filter: ListFilter{Kind: "bogus"}}); err == nil {
		t.Fatal("invalid kind filter should be a validation error")
	}
	var ve *ValidationError
	_, err := svc.List(context.Background(), ListParams{TenantID: "t1", Filter: ListFilter{Status: "bogus"}})
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %T", err)
	}
}

func TestListTenantScoped(t *testing.T) {
	fs := newFakeStore()
	fs.add(Job{TenantID: "t1", Kind: "sync_source", Status: StatusQueued})
	fs.add(Job{TenantID: "t2", Kind: "sync_source", Status: StatusQueued})
	svc := NewService(fs)
	page, _ := svc.List(context.Background(), ListParams{TenantID: "t1"})
	if len(page.Items) != 1 || page.Items[0].TenantID != "t1" {
		t.Fatalf("List leaked across tenants: %+v", page.Items)
	}
}

func TestGetRequiresTenantAndScopes(t *testing.T) {
	fs := newFakeStore()
	j := fs.add(Job{TenantID: "t1", Kind: "sync_source", Status: StatusQueued})
	svc := NewService(fs)
	if _, err := svc.Get(context.Background(), "", j.ID); err == nil {
		t.Fatal("Get with no tenant should fail closed")
	}
	// Another tenant cannot read it.
	if _, err := svc.Get(context.Background(), "t2", j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Get = %v, want ErrNotFound", err)
	}
	got, err := svc.Get(context.Background(), "t1", j.ID)
	if err != nil || got.ID != j.ID {
		t.Fatalf("Get: %v, %+v", err, got)
	}
}

func TestCancelQueuedIsEffectiveNow(t *testing.T) {
	fs := newFakeStore()
	j := fs.add(Job{TenantID: "t1", Kind: "sync_source", Status: StatusQueued})
	svc := NewService(fs)
	got, err := svc.Cancel(context.Background(), "t1", j.ID)
	if err != nil {
		t.Fatalf("Cancel queued: %v", err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
	if len(fs.cancelled) != 1 {
		t.Fatalf("store did not cancel the queued row")
	}
}

func TestCancelRunningWithoutCancellerIsSeam(t *testing.T) {
	fs := newFakeStore()
	j := fs.add(Job{TenantID: "t1", Kind: "sync_source", Status: StatusRunning})
	svc := NewService(fs) // Canceller nil (EPIC-09 not wired)
	_, err := svc.Cancel(context.Background(), "t1", j.ID)
	if !errors.Is(err, ErrCancelUnavailable) {
		t.Fatalf("running cancel without worker = %v, want ErrCancelUnavailable", err)
	}
}

func TestCancelRunningWithCancellerSignals(t *testing.T) {
	fs := newFakeStore()
	j := fs.add(Job{TenantID: "t1", Kind: "sync_source", Status: StatusRunning})
	c := &fakeCanceller{}
	svc := NewService(fs)
	svc.Canceller = c
	got, err := svc.Cancel(context.Background(), "t1", j.ID)
	if err != nil {
		t.Fatalf("Cancel running: %v", err)
	}
	if len(c.called) != 1 || c.called[0] != j.ID {
		t.Fatalf("canceller not signalled: %+v", c.called)
	}
	// The mirror row transition is the worker's job (SPEC-08 §3): still running.
	if got.Status != StatusRunning {
		t.Fatalf("status = %q, want running (worker finalises)", got.Status)
	}
}

func TestCancelTerminalIsConflict(t *testing.T) {
	for _, st := range []string{StatusSucceeded, StatusFailed, StatusCancelled} {
		fs := newFakeStore()
		j := fs.add(Job{TenantID: "t1", Kind: "sync_source", Status: st})
		svc := NewService(fs)
		got, err := svc.Cancel(context.Background(), "t1", j.ID)
		if st == StatusCancelled {
			// Already cancelled is idempotent (no error).
			if err != nil {
				t.Fatalf("cancel already-cancelled = %v, want nil (idempotent)", err)
			}
			if got.Status != StatusCancelled {
				t.Fatalf("status = %q, want cancelled", got.Status)
			}
			continue
		}
		if !errors.Is(err, ErrNotCancellable) {
			t.Fatalf("cancel %s = %v, want ErrNotCancellable", st, err)
		}
	}
}

func TestCancelUnknownIsNotFound(t *testing.T) {
	svc := NewService(newFakeStore())
	if _, err := svc.Cancel(context.Background(), "t1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown = %v, want ErrNotFound", err)
	}
}

func TestListComputesDuration(t *testing.T) {
	fs := newFakeStore()
	start := time.Unix(1_700_000_000, 0).UTC()
	end := start.Add(3 * time.Second)
	fs.add(Job{TenantID: "t1", Kind: "sync_source", Status: StatusSucceeded, StartedAt: &start, FinishedAt: &end, Stats: mustJSON(map[string]int{"docs": 2})})
	svc := NewService(fs)
	page, _ := svc.List(context.Background(), ListParams{TenantID: "t1"})
	if len(page.Items) != 1 {
		t.Fatalf("items = %d", len(page.Items))
	}
	if page.Items[0].DurationMS == nil || *page.Items[0].DurationMS != 3000 {
		t.Fatalf("duration_ms = %v, want 3000", page.Items[0].DurationMS)
	}
}
