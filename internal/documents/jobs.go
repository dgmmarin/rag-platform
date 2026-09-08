package documents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job mirrors the control-plane jobs row shape the API returns (SPEC-07 §2,
// SPEC-08 §1). The ingest_document job is the mirror/history row the EPIC-09
// worker consumes; this package only enqueues it.
type Job struct {
	ID       string          `json:"id"`
	TenantID string          `json:"tenant_id"`
	SourceID *string         `json:"source_id,omitempty"`
	Kind     string          `json:"kind"`
	Status   string          `json:"status"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	QueuedAt time.Time       `json:"queued_at"`
}

// NewIngestJob is the input to JobEnqueuer.EnqueueIngest. SourceID is the tenant's
// upload source (nullable in jobs; resolving the implicit upload source is the
// upload connector's job, EPIC-06).
type NewIngestJob struct {
	TenantID string
	SourceID *string
	Payload  json.RawMessage
}

// JobEnqueuer is the control-plane queue port for the ingest_document job. It
// touches ONLY the control-plane jobs table (queue state is control-plane, C-3) —
// never a tenant database. ControlJobs implements it over the control-plane pool;
// unit tests use an in-memory fake.
type JobEnqueuer interface {
	// EnqueueIngest writes a queued ingest_document job and returns it.
	EnqueueIngest(ctx context.Context, nj NewIngestJob) (Job, error)
	// FindActiveIngest returns an active ingest_document job for the tenant whose
	// payload idempotency_key equals key (empty key => not found), for idempotent
	// replay of POST /v1/documents (SPEC-07 §1).
	FindActiveIngest(ctx context.Context, tenantID, idempotencyKey string) (Job, bool, error)
}

// Storage is the object-storage seam for the raw upload bytes. STORY-06.3 wires
// a MinIO/S3 backend (internal/objectstore) behind it; the ingest_document worker
// reads the bytes back through the wider objectstore.Fetcher. Adding/replacing the
// backend requires no change outside its own package (NFR-MNT-01/02).
type Storage interface {
	// Put stores the raw bytes under key with the given content type.
	Put(ctx context.Context, key, contentType string, r io.Reader) error
}

// UploadSource resolves (lazily creating on first use) the tenant's implicit
// "upload" source so an uploaded document has a source_id (SPEC-04 §5). Sources
// are control-plane registry data (C-3), so the implementation runs on the
// control-plane pool — never a tenant database.
type UploadSource interface {
	Resolve(ctx context.Context, tenantID string) (sourceID string, err error)
}

// UploadLimits reads a tenant's configured upload ceiling in bytes
// (settings.limits.max_upload_mb, SPEC-02 §5) so POST /v1/documents enforces the
// per-tenant size limit (FR-SRC-02). Settings are control-plane data (C-3).
type UploadLimits interface {
	MaxUploadBytes(ctx context.Context, tenantID string) (int64, error)
}

// IngestQueue enqueues the durable River ingest_document job in the caller's
// transaction and reports the River job id (to link the mirror row) plus whether
// River collapsed it onto an already-active job (STORY-09.2, ADR-0005, ADR-0060). It
// is implemented in internal/cli over a River insert client; a nil IngestQueue keeps
// the pre-09.2 path — a jobs row with no River job — which hermetic unit tests use to
// assert enqueue without a running queue.
type IngestQueue interface {
	EnqueueIngestTx(ctx context.Context, tx pgx.Tx, nj NewIngestJob) (riverJobID int64, duplicate bool, err error)
}

// querier is the read/write surface shared by *pgxpool.Pool and pgx.Tx, so the row
// insert runs on the pool (legacy path) or inside a transaction (River path).
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ControlJobs implements JobEnqueuer over the control-plane pgx pool. With a River
// IngestQueue it enqueues the queue job and its mirror row atomically; without one it
// writes the mirror row alone.
type ControlJobs struct {
	pool  *pgxpool.Pool
	queue IngestQueue
}

// JobsFromPool wraps a control-plane pool as a JobEnqueuer (jobs-row-only until a
// queue is attached).
func JobsFromPool(pool *pgxpool.Pool) ControlJobs { return ControlJobs{pool: pool} }

// WithQueue returns a copy that enqueues the River ingest_document job transactionally
// with the mirror row (ADR-0005). The composition root attaches it in `ragctl serve`.
func (c ControlJobs) WithQueue(q IngestQueue) ControlJobs { c.queue = q; return c }

// EnqueueIngest inserts a queued ingest_document job (SPEC-08 §1). source_id is
// nullable; the FK is to control-plane sources(id). With a queue attached it enqueues
// the River job first (same tx) and links the row by river_job_id; if River treated
// the insert as a duplicate of an active job, the existing mirror row is returned
// (idempotent) rather than a second row for the same River job.
func (c ControlJobs) EnqueueIngest(ctx context.Context, nj NewIngestJob) (Job, error) {
	if c.queue == nil {
		return insertIngestRow(ctx, c.pool, nj, nil)
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	riverID, dup, err := c.queue.EnqueueIngestTx(ctx, tx, nj)
	if err != nil {
		return Job{}, err
	}
	var j Job
	if dup {
		j, err = findIngestByRiverID(ctx, tx, riverID)
	} else {
		j, err = insertIngestRow(ctx, tx, nj, &riverID)
	}
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, err
	}
	return j, nil
}

// insertIngestRow writes the queued ingest_document mirror row, optionally linked to
// its River job (riverID nil = legacy jobs-row-only path).
func insertIngestRow(ctx context.Context, q querier, nj NewIngestJob, riverID *int64) (Job, error) {
	var j Job
	var sid *string
	payload := nj.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	err := q.QueryRow(ctx, `
		insert into jobs (tenant_id, source_id, kind, status, payload, river_job_id)
		values ($1, $2, 'ingest_document', 'queued', $3, $4)
		returning id::text, tenant_id::text, source_id::text, kind::text, status::text, payload, queued_at`,
		nj.TenantID, nj.SourceID, []byte(payload), riverID).
		Scan(&j.ID, &j.TenantID, &sid, &j.Kind, &j.Status, &j.Payload, &j.QueuedAt)
	if err != nil {
		return Job{}, err
	}
	j.SourceID = sid
	return j, nil
}

// findIngestByRiverID returns the mirror row already linked to a River job (used when
// River deduplicated the insert onto an active job).
func findIngestByRiverID(ctx context.Context, q querier, riverID int64) (Job, error) {
	var j Job
	var sid *string
	err := q.QueryRow(ctx, `
		select id::text, tenant_id::text, source_id::text, kind::text, status::text, payload, queued_at
		from jobs where river_job_id = $1`, riverID).
		Scan(&j.ID, &j.TenantID, &sid, &j.Kind, &j.Status, &j.Payload, &j.QueuedAt)
	if err != nil {
		return Job{}, err
	}
	j.SourceID = sid
	return j, nil
}

// FindActiveIngest returns a queued/running ingest_document job for the tenant
// whose payload idempotency_key matches, enabling idempotent replay.
func (c ControlJobs) FindActiveIngest(ctx context.Context, tenantID, key string) (Job, bool, error) {
	if key == "" {
		return Job{}, false, nil
	}
	var j Job
	var sid *string
	err := c.pool.QueryRow(ctx, `
		select id::text, tenant_id::text, source_id::text, kind::text, status::text, payload, queued_at
		from jobs
		where tenant_id = $1 and kind = 'ingest_document'
		  and status in ('queued', 'running') and payload->>'idempotency_key' = $2
		order by queued_at desc
		limit 1`,
		tenantID, key).
		Scan(&j.ID, &j.TenantID, &sid, &j.Kind, &j.Status, &j.Payload, &j.QueuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	j.SourceID = sid
	return j, true, nil
}
