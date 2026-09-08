package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// deleteSourceWorker runs one delete_source job (SPEC-08 §1, FR-SRC-12, STORY-09.6). It
// opens the tenant.DB per job (ADR-0003) and removes the source's documents, versions,
// chunks and crawl state, reporting the removed counts to the jobs mirror (stats).
type deleteSourceWorker struct {
	river.WorkerDefaults[DeleteSourceArgs]
	resolver tenant.Resolver
	store    documents.TenantStore
	log      *slog.Logger
}

// Work resolves the tenant DB and deletes the source's content.
func (w *deleteSourceWorker) Work(ctx context.Context, job *river.Job[DeleteSourceArgs]) error {
	id, err := uuid.Parse(job.Args.TenantID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("delete_source: bad tenant id %q: %w", job.Args.TenantID, err))
	}
	if _, err := uuid.Parse(job.Args.SourceID); err != nil {
		return river.JobCancel(fmt.Errorf("delete_source: bad source id %q: %w", job.Args.SourceID, err))
	}
	db, err := w.resolver.Open(ctx, tenant.ID(id))
	if err != nil {
		return fmt.Errorf("delete_source: open tenant: %w", err)
	}
	stats, err := w.store.DeleteSource(ctx, db, job.Args.SourceID)
	if err != nil {
		return err
	}
	if b, mErr := json.Marshal(stats); mErr == nil {
		Stats(ctx).Set(b)
	}
	w.log.Info("delete_source done",
		"tenant_id", job.Args.TenantID, "source_id", job.Args.SourceID,
		"documents", stats.Documents, "chunks", stats.Chunks, "crawl_pages", stats.CrawlPages)
	return nil
}
