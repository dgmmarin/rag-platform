package documents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Service is the documents domain logic. It owns the resolver (the ONLY source of
// a tenant.DB, ADR-0003), the tenant-content Store, the control-plane JobEnqueuer
// for the ingest job, and the optional object-Storage seam (nil => EPIC-06
// not-wired). It is stateless and safe for concurrent use.
type Service struct {
	Resolver tenant.Resolver
	Store    Store
	Jobs     JobEnqueuer
	Storage  Storage // nil until EPIC-06 wires object storage; Ingest fails closed
	MaxBytes int64
	now      func() time.Time
}

// NewService builds a documents service. Storage is left nil (object storage is
// EPIC-06); set it to enable POST /v1/documents. MaxBytes defaults to the
// FR-SRC-02 50 MB ceiling when non-positive.
func NewService(resolver tenant.Resolver, store Store, jobs JobEnqueuer) *Service {
	return &Service{Resolver: resolver, Store: store, Jobs: jobs, now: time.Now}
}

// maxBytes returns the configured upload ceiling or the FR-SRC-02 default.
func (s *Service) maxBytes() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return defaultMaxUploadBytes
}

// open resolves the tenant to its *tenant.DB, mapping the resolver's lifecycle
// outcomes to the package sentinels (ADR-0003 — this is the only place a handle
// is obtained). A not-ready/unknown/schema-behind tenant becomes
// ErrTenantUnavailable so the caller never leaks internal detail.
func (s *Service) open(ctx context.Context, tid tenant.ID) (*tenant.DB, error) {
	db, err := s.Resolver.Open(ctx, tid)
	if err != nil {
		switch {
		case errors.Is(err, tenant.ErrTenantUnavailable),
			errors.Is(err, tenant.ErrTenantNotFound),
			errors.Is(err, tenant.ErrSchemaOutdated):
			return nil, ErrTenantUnavailable
		default:
			return nil, fmt.Errorf("documents: open tenant: %w", err)
		}
	}
	return db, nil
}

// List returns a page of the tenant's documents with the SPEC-07 §2 filters.
func (s *Service) List(ctx context.Context, tid tenant.ID, f ListFilter, limit int, cursor string) (Page, error) {
	if f.Status != "" && f.Status != "active" && f.Status != "deleted" {
		return Page{}, invalid("status must be one of active, deleted")
	}
	if f.SourceID != "" && !validUUID(f.SourceID) {
		return Page{}, invalid("source must be a UUID")
	}
	var cur *Cursor
	if cursor != "" {
		c, err := decodeCursor(cursor)
		if err != nil {
			return Page{}, invalid("invalid cursor")
		}
		cur = c
	}
	limit = clampLimit(limit)

	db, err := s.open(ctx, tid)
	if err != nil {
		return Page{}, err
	}
	rows, err := s.Store.List(ctx, db, f, limit+1, cur)
	if err != nil {
		return Page{}, fmt.Errorf("documents: list: %w", err)
	}
	page := Page{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(Cursor{FirstSeenAt: last.FirstSeenAt, ID: last.ID})
	}
	if page.Items == nil {
		page.Items = []Document{}
	}
	return page, nil
}

// Get returns one document plus its current version metadata (SPEC-07 §2).
func (s *Service) Get(ctx context.Context, tid tenant.ID, id string, withContent bool) (DocumentDetail, error) {
	db, err := s.open(ctx, tid)
	if err != nil {
		return DocumentDetail{}, err
	}
	return s.Store.Get(ctx, db, id, withContent)
}

// Chunks returns a page of a document's current-version chunks (FR-ADM-03).
func (s *Service) Chunks(ctx context.Context, tid tenant.ID, docID string, limit int, cursor string) (ChunkPage, error) {
	var cur *ChunkCursor
	if cursor != "" {
		c, err := decodeChunkCursor(cursor)
		if err != nil {
			return ChunkPage{}, invalid("invalid cursor")
		}
		cur = c
	}
	limit = clampLimit(limit)

	db, err := s.open(ctx, tid)
	if err != nil {
		return ChunkPage{}, err
	}
	rows, err := s.Store.Chunks(ctx, db, docID, limit+1, cur)
	if err != nil {
		return ChunkPage{}, err
	}
	page := ChunkPage{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		page.NextCursor = encodeChunkCursor(ChunkCursor{Position: page.Items[len(page.Items)-1].Position})
	}
	if page.Items == nil {
		page.Items = []Chunk{}
	}
	return page, nil
}

// Delete soft-deletes a document (SPEC-07 §2 DELETE, FR-SRC-02). It is idempotent
// and returns ErrNotFound for an unknown document.
func (s *Service) Delete(ctx context.Context, tid tenant.ID, id string) error {
	db, err := s.open(ctx, tid)
	if err != nil {
		return err
	}
	existed, err := s.Store.SoftDelete(ctx, db, id)
	if err != nil {
		if errors.Is(err, tenant.ErrReadOnly) {
			return ErrTenantUnavailable // suspended tenant: writes refused
		}
		return fmt.Errorf("documents: delete: %w", err)
	}
	if !existed {
		return ErrNotFound
	}
	return nil
}

// IngestParams is the input to Ingest: the validated upload plus its metadata. The
// handler has already enforced the type allowlist and the size ceiling before the
// bytes are read.
type IngestParams struct {
	TenantID       string
	SourceID       *string // the tenant's upload source (EPIC-06 resolves the implicit one)
	Filename       string
	ContentType    string
	Size           int64
	Reader         io.Reader
	IdempotencyKey string
}

// Ingest stores the raw upload and enqueues an ingest_document job for the EPIC-09
// worker to build the document version and chunks (ADR-0008). Object storage is
// the EPIC-06 seam: when Storage is nil, Ingest fails closed with
// ErrStorageUnavailable (the handler returns the not_found seam envelope) rather
// than silently dropping the bytes. The enqueue itself is real (control-plane
// jobs table). An Idempotency-Key replays the active ingest job (SPEC-07 §1).
//
// The document row is NOT created here: an active document must have a non-null
// current_version (SPEC-03 §2 invariant 1) and there is no pending status, so the
// row is created by the ingest worker/document store (STORY-05.1) in one
// transaction with its first version. Ingest therefore returns the queued job.
func (s *Service) Ingest(ctx context.Context, p IngestParams) (Job, error) {
	if p.TenantID == "" {
		return Job{}, fmt.Errorf("documents: tenant is required")
	}
	if p.Filename == "" {
		return Job{}, invalid("a file is required")
	}
	if s.Storage == nil {
		return Job{}, ErrStorageUnavailable
	}
	if p.IdempotencyKey != "" {
		if existing, ok, err := s.Jobs.FindActiveIngest(ctx, p.TenantID, p.IdempotencyKey); err != nil {
			return Job{}, fmt.Errorf("documents: idempotency lookup: %w", err)
		} else if ok {
			return existing, nil // idempotent replay
		}
	}

	// Object key namespaced by tenant; the worker reads it back to fetch the bytes.
	objectKey := fmt.Sprintf("uploads/%s/%s-%s", p.TenantID, uuid.NewString(), p.Filename)
	if err := s.Storage.Put(ctx, objectKey, p.ContentType, p.Reader); err != nil {
		return Job{}, fmt.Errorf("documents: store upload: %w", err)
	}

	payload := map[string]any{
		"external_id":  p.Filename,
		"filename":     p.Filename,
		"mime_type":    p.ContentType,
		"size":         p.Size,
		"object_key":   objectKey,
		"uploaded_via": "api",
	}
	if p.SourceID != nil {
		payload["source_id"] = *p.SourceID
	}
	if p.IdempotencyKey != "" {
		payload["idempotency_key"] = p.IdempotencyKey
	}
	body, _ := json.Marshal(payload)

	job, err := s.Jobs.EnqueueIngest(ctx, NewIngestJob{TenantID: p.TenantID, SourceID: p.SourceID, Payload: body})
	if err != nil {
		return Job{}, fmt.Errorf("documents: enqueue ingest: %w", err)
	}
	return job, nil
}
