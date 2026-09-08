package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/rag-platform/ragctl/internal/cp/jobs"
	"github.com/rag-platform/ragctl/internal/cp/sources"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/worker"
)

// riverCanceller implements jobs.Canceller by cancelling the River job linked to a
// mirror row (STORY-09.4, SPEC-08 §4). River drops a QUEUED job immediately (the
// worker can never claim it) and signals a RUNNING one by cancelling its work context,
// so the handler stops between documents committing nothing partial (SPEC-05 §5) and
// the mirror middleware records the cancelled terminal.
type riverCanceller struct {
	pool   *pgxpool.Pool
	client *river.Client[pgx.Tx]
}

func (c riverCanceller) Cancel(ctx context.Context, tenantID, jobID string) error {
	var riverID *int64
	err := c.pool.QueryRow(ctx,
		`select river_job_id from jobs where tenant_id = $1::uuid and id = $2::uuid`, tenantID, jobID).Scan(&riverID)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.ErrNotFound
	}
	if err != nil {
		return err
	}
	if riverID == nil {
		return nil // no River job linked (a legacy jobs-row-only kind); nothing to drop
	}
	if _, err := c.client.JobCancel(ctx, *riverID); err != nil && !errors.Is(err, rivertype.ErrNotFound) {
		return err // a job River already finished/removed is not an error for cancel
	}
	return nil
}

// ingestQueue bridges the documents producer to the River queue (STORY-09.2,
// ADR-0005): it enqueues a worker.IngestDocumentArgs job in the producer's own
// transaction so the queue job and its jobs mirror row commit atomically. Its args
// are decoded from the payload documents.Service.Ingest already builds.
type ingestQueue struct{ client *river.Client[pgx.Tx] }

func (q ingestQueue) EnqueueIngestTx(ctx context.Context, tx pgx.Tx, nj documents.NewIngestJob) (int64, bool, error) {
	var p struct {
		ObjectKey  string `json:"object_key"`
		ExternalID string `json:"external_id"`
		Filename   string `json:"filename"`
		MimeType   string `json:"mime_type"`
		SourceID   string `json:"source_id"`
	}
	if len(nj.Payload) > 0 {
		if err := json.Unmarshal(nj.Payload, &p); err != nil {
			return 0, false, fmt.Errorf("ingest args: %w", err)
		}
	}
	sourceID := p.SourceID
	if nj.SourceID != nil {
		sourceID = *nj.SourceID
	}
	res, err := q.client.InsertTx(ctx, tx, worker.IngestDocumentArgs{
		TenantID:   nj.TenantID,
		SourceID:   sourceID,
		ObjectKey:  p.ObjectKey,
		ExternalID: p.ExternalID,
		Filename:   p.Filename,
		MimeType:   p.MimeType,
	}, nil)
	if err != nil {
		return 0, false, err
	}
	return res.Job.ID, res.UniqueSkippedAsDuplicate, nil
}

// syncQueue bridges the sources producer to the River queue (STORY-09.2, ADR-0005):
// it enqueues a worker.SyncSourceArgs job in the producer's transaction. The Full
// flag is decoded from the payload sources.Service.Sync builds.
type syncQueue struct{ client *river.Client[pgx.Tx] }

func (q syncQueue) EnqueueSyncTx(ctx context.Context, tx pgx.Tx, nj sources.NewJob) (int64, bool, error) {
	var p struct {
		Full bool `json:"full"`
	}
	if len(nj.Payload) > 0 {
		if err := json.Unmarshal(nj.Payload, &p); err != nil {
			return 0, false, fmt.Errorf("sync args: %w", err)
		}
	}
	res, err := q.client.InsertTx(ctx, tx, worker.SyncSourceArgs{
		TenantID: nj.TenantID,
		SourceID: nj.SourceID,
		Full:     p.Full,
	}, nil)
	if err != nil {
		return 0, false, err
	}
	return res.Job.ID, res.UniqueSkippedAsDuplicate, nil
}

// EnqueueDeleteSourceTx enqueues a worker.DeleteSourceArgs job (STORY-09.6) in the
// producer's transaction.
func (q syncQueue) EnqueueDeleteSourceTx(ctx context.Context, tx pgx.Tx, nj sources.NewJob) (int64, bool, error) {
	res, err := q.client.InsertTx(ctx, tx, worker.DeleteSourceArgs{
		TenantID: nj.TenantID,
		SourceID: nj.SourceID,
	}, nil)
	if err != nil {
		return 0, false, err
	}
	return res.Job.ID, res.UniqueSkippedAsDuplicate, nil
}
