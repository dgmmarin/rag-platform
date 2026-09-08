package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// gcWorker runs one gc_tenant job (SPEC-08 §1, STORY-05.9). It opens a fresh
// tenant.DB per job (ADR-0003) and runs the SPEC-03 §4 retention sweep. A default
// policy is used (SPEC-03 §4 retention windows; crawl-page sweep skipped unless a
// stale window is set); the scheduler (STORY-09.3) enqueues this daily.
type gcWorker struct {
	river.WorkerDefaults[GCTenantArgs]
	resolver tenant.Resolver
	store    documents.TenantStore
	log      *slog.Logger
}

// Work resolves the tenant DB and collects garbage.
func (w *gcWorker) Work(ctx context.Context, job *river.Job[GCTenantArgs]) error {
	id, err := uuid.Parse(job.Args.TenantID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("gc_tenant: bad tenant id %q: %w", job.Args.TenantID, err))
	}
	db, err := w.resolver.Open(ctx, tenant.ID(id))
	if err != nil {
		return fmt.Errorf("gc_tenant: open tenant: %w", err)
	}
	m, err := w.store.CollectGarbage(ctx, db, documents.GCPolicy{}, time.Now())
	if err != nil {
		return err
	}
	w.log.Info("gc_tenant done", "tenant_id", job.Args.TenantID, "rows_removed", m.Total())
	return nil
}

// todoWorker registers a queue's job kind whose handler is not built in this story
// so the queue structure is complete and an accidentally-enqueued job of that kind
// fails LOUDLY and permanently (JobCancel, no retry-loop) rather than silently
// vanishing. Each names the story that will implement it.
type todoWorker[T river.JobArgs] struct {
	river.WorkerDefaults[T]
	story string
	log   *slog.Logger
}

// Work logs and cancels: the kind is wired into the queue but has no handler yet.
func (w *todoWorker[T]) Work(_ context.Context, job *river.Job[T]) error {
	w.log.Warn("job kind not yet wired", "kind", job.Args.Kind(), "story", w.story)
	return river.JobCancel(fmt.Errorf("worker: %q not yet wired (%s)", job.Args.Kind(), w.story))
}
