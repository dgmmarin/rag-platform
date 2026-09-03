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

// Storage is the object-storage seam for the raw upload bytes (EPIC-06). It is
// nil until STORY-06.x wires MinIO/S3; while nil, Ingest fails closed with the
// ErrStorageUnavailable seam. Adding the real backend requires no change outside
// its own package (NFR-MNT-01/02).
type Storage interface {
	// Put stores the raw bytes under key with the given content type.
	Put(ctx context.Context, key, contentType string, r io.Reader) error
}

// ControlJobs implements JobEnqueuer over the control-plane pgx pool.
type ControlJobs struct{ pool *pgxpool.Pool }

// JobsFromPool wraps a control-plane pool as a JobEnqueuer.
func JobsFromPool(pool *pgxpool.Pool) ControlJobs { return ControlJobs{pool: pool} }

// EnqueueIngest inserts a queued ingest_document job (SPEC-08 §1). source_id is
// nullable; the FK is to control-plane sources(id).
func (c ControlJobs) EnqueueIngest(ctx context.Context, nj NewIngestJob) (Job, error) {
	var j Job
	var sid *string
	payload := nj.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	err := c.pool.QueryRow(ctx, `
		insert into jobs (tenant_id, source_id, kind, status, payload)
		values ($1, $2, 'ingest_document', 'queued', $3)
		returning id::text, tenant_id::text, source_id::text, kind::text, status::text, payload, queued_at`,
		nj.TenantID, nj.SourceID, []byte(payload)).
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
