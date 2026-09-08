package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/rag-platform/ragctl/internal/ingest/ingestdoc"
	"github.com/rag-platform/ragctl/internal/ingest/sink"
)

// snoozeDuration is how long an ingest job waits before retry when the embedding
// circuit breaker is open (SPEC-05 §8: "provider quota exhausted → job paused, not
// failed"). The job is re-queued, not failed, so the retry budget is untouched.
const snoozeDuration = 15 * time.Minute

// ingestWorker runs one ingest_document job (SPEC-08 §1). It opens a fresh
// tenant.DB per job via the ingestor (which resolves it from the job's tenant_id,
// ADR-0003) and drives the STORY-06.3 handler: fetch the raw bytes, parse → chunk →
// embed → commit through the ingestion sink. The handler already exists; this is the
// River seam it was designed for (see ingestdoc's package doc).
type ingestWorker struct {
	river.WorkerDefaults[IngestDocumentArgs]
	ingestor *ingestdoc.Ingestor
	log      *slog.Logger
}

// Work dispatches the job to the ingestdoc handler. A SnoozeError (circuit open)
// pauses the job rather than failing it (SPEC-05 §8); any other error fails the job
// for retry, and because each document commits in its own transaction (SPEC-05 §5)
// a retry re-processes cleanly (unchanged docs skipped by hash).
func (w *ingestWorker) Work(ctx context.Context, job *river.Job[IngestDocumentArgs]) error {
	// Reuse the handler's own fail-closed payload decoder (object_key/source_id
	// required, external_id falls back to filename) by round-tripping the typed args
	// through the same JSON shape documents.Service.Ingest writes — one source of
	// truth for the payload contract.
	payload, err := json.Marshal(job.Args)
	if err != nil {
		return err
	}
	j, err := ingestdoc.JobFromPayload(job.Args.TenantID, payload)
	if err != nil {
		// A malformed payload will never succeed on retry: cancel rather than burn the
		// retry budget on a permanent error.
		return river.JobCancel(err)
	}

	stats, err := w.ingestor.Dispatch(ctx, j)
	if err != nil {
		var sn *sink.SnoozeError
		if errors.As(err, &sn) {
			w.log.Warn("ingest_document snoozed (embedding circuit open)",
				"tenant_id", job.Args.TenantID, "external_id", j.ExternalID)
			return river.JobSnooze(snoozeDuration)
		}
		return err
	}
	// Report stats to the mirror so they land in jobs.stats on success (SPEC-08 §3);
	// the sink.Stats JSON tags already match the jobs.stats shape.
	if b, mErr := json.Marshal(stats); mErr == nil {
		Stats(ctx).Set(b)
	}
	w.log.Info("ingest_document done",
		"tenant_id", job.Args.TenantID, "external_id", j.ExternalID,
		"docs_changed", stats.DocsChanged, "chunks_written", stats.ChunksWritten)
	return nil
}
