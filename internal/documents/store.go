package documents

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Store is the tenant-content persistence port for documents/versions/chunks. It
// is reached ONLY through a *tenant.DB (ADR-0003, C-3): every method takes the
// resolved handle, so there is no way to read another tenant's content and no
// tenant_id filter (the database boundary is the tenant boundary, C-1).
// TenantStore implements it over a live tenant database; unit tests exercise the
// SQL via the e2e suite against a real tenant DB (a *tenant.DB is unforgeable by
// design, so it is not mockable — that is the point).
type Store interface {
	List(ctx context.Context, db *tenant.DB, f ListFilter, limit int, cur *Cursor) ([]Document, error)
	Get(ctx context.Context, db *tenant.DB, id string, withContent bool) (DocumentDetail, error)
	Chunks(ctx context.Context, db *tenant.DB, docID string, limit int, cur *ChunkCursor) ([]Chunk, error)
	// SoftDelete flips status to 'deleted' and stamps deleted_at. existed is false
	// when there is no such document; it is idempotent (a re-delete still reports
	// existed=true).
	SoftDelete(ctx context.Context, db *tenant.DB, id string) (existed bool, err error)
	// Put persists a fully-built, embedded document version and atomically flips
	// current_version (ADR-0008, SPEC-05 §5); an unchanged content hash only
	// touches last_seen_at. See put.go.
	Put(ctx context.Context, db *tenant.DB, in PutInput) (PutResult, error)
	// TouchIfUnchanged is the SPEC-05 §1 hash short-circuit: touch last_seen_at and
	// return unchanged=true when the current version's hash matches, so the sink
	// skips chunk+embed. See sync.go.
	TouchIfUnchanged(ctx context.Context, db *tenant.DB, sourceID, externalID string, hash []byte) (bool, error)
	// SoftDeleteUnseen marks documents of a source not re-seen since startedAt
	// 'deleted' (full-sync Sink.Complete, SPEC-05 §5). See sync.go.
	SoftDeleteUnseen(ctx context.Context, db *tenant.DB, sourceID string, startedAt time.Time) (int, error)
}

// TenantStore is the production Store over a tenant database.
type TenantStore struct{}

// NewTenantStore builds the tenant-schema store.
func NewTenantStore() TenantStore { return TenantStore{} }

// docColumns is the shared documents projection.
const docColumns = `d.id::text, d.source_id::text, d.external_id, d.title, d.uri, d.mime_type,
	d.status::text, d.current_version::text, d.metadata, d.first_seen_at, d.last_seen_at, d.deleted_at`

func scanDocument(row pgx.Row) (Document, error) {
	var d Document
	var meta []byte
	if err := row.Scan(&d.ID, &d.SourceID, &d.ExternalID, &d.Title, &d.URI, &d.MimeType,
		&d.Status, &d.CurrentVersion, &meta, &d.FirstSeenAt, &d.LastSeenAt, &d.DeletedAt); err != nil {
		return Document{}, err
	}
	d.Metadata = json.RawMessage(meta)
	return d, nil
}

// List returns a tenant's documents newest-first (first_seen_at, id) with keyset
// pagination and the SPEC-07 §2 filters (source, status, q). q matches the title
// or external_id case-insensitively.
func (TenantStore) List(ctx context.Context, db *tenant.DB, f ListFilter, limit int, cur *Cursor) ([]Document, error) {
	var curAt, curID any
	if cur != nil {
		curAt = cur.FirstSeenAt
		curID = cur.ID
	}
	rows, err := db.Query(ctx, `
		select `+docColumns+`
		from documents d
		where ($1 = '' or d.source_id = $1::uuid)
		  and ($2 = '' or d.status = $2::document_status)
		  and ($3 = '' or d.title ilike '%' || $3 || '%' or d.external_id ilike '%' || $3 || '%')
		  and ($4::timestamptz is null
		       or d.first_seen_at < $4::timestamptz
		       or (d.first_seen_at = $4::timestamptz and d.id < $5::uuid))
		order by d.first_seen_at desc, d.id desc
		limit $6`,
		f.SourceID, f.Status, f.Q, curAt, curID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Get returns one document plus its current-version metadata, or ErrNotFound. The
// full version content is included only when withContent is true (?content=true).
func (TenantStore) Get(ctx context.Context, db *tenant.DB, id string, withContent bool) (DocumentDetail, error) {
	if !validUUID(id) {
		return DocumentDetail{}, ErrNotFound
	}
	contentExpr := "null::text"
	if withContent {
		contentExpr = "v.content"
	}
	var (
		d       Document
		meta    []byte
		vID     *string
		vHash   []byte
		vChars  *int
		vParser *string
		vCreat  *time.Time
		vContnt *string
	)
	err := db.QueryRow(ctx, `
		select `+docColumns+`,
		       v.id::text, v.content_hash, v.char_count, v.parser, v.created_at, `+contentExpr+`
		from documents d
		left join document_versions v on v.id = d.current_version
		where d.id = $1`, id).
		Scan(&d.ID, &d.SourceID, &d.ExternalID, &d.Title, &d.URI, &d.MimeType,
			&d.Status, &d.CurrentVersion, &meta, &d.FirstSeenAt, &d.LastSeenAt, &d.DeletedAt,
			&vID, &vHash, &vChars, &vParser, &vCreat, &vContnt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DocumentDetail{}, ErrNotFound
	}
	if err != nil {
		return DocumentDetail{}, err
	}
	d.Metadata = json.RawMessage(meta)
	detail := DocumentDetail{Document: d}
	if vID != nil {
		vm := &VersionMeta{ID: *vID, ContentHash: hex.EncodeToString(vHash)}
		if vCreat != nil {
			vm.CreatedAt = *vCreat
		}
		if vChars != nil {
			vm.CharCount = *vChars
		}
		vm.Parser = vParser
		if withContent {
			vm.Content = vContnt
		}
		detail.CurrentVersion = vm
	}
	return detail, nil
}

// Chunks returns the current-version chunks of a document ordered by position,
// with keyset pagination on position (FR-ADM-03 debugging). It returns the chunks
// of documents.current_version even for a soft-deleted document, so an admin can
// still inspect what was indexed. The embedding vector is never selected.
func (TenantStore) Chunks(ctx context.Context, db *tenant.DB, docID string, limit int, cur *ChunkCursor) ([]Chunk, error) {
	if !validUUID(docID) {
		return nil, ErrNotFound
	}
	// Confirm the document exists so an unknown id is 404 rather than an empty page.
	var exists bool
	if err := db.QueryRow(ctx, `select exists(select 1 from documents where id = $1)`, docID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	after := -1
	if cur != nil {
		after = cur.Position
	}
	rows, err := db.Query(ctx, `
		select c.id::text, c.position, c.heading_path, c.content, c.token_count,
		       c.embedding_model, c.metadata, c.created_at
		from chunks c
		join documents d on d.id = c.document_id
		where c.document_id = $1 and c.version_id = d.current_version and c.position > $2
		order by c.position
		limit $3`,
		docID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Chunk
	for rows.Next() {
		var ch Chunk
		var meta []byte
		if err := rows.Scan(&ch.ID, &ch.Position, &ch.HeadingPath, &ch.Content, &ch.TokenCount,
			&ch.EmbeddingModel, &meta, &ch.CreatedAt); err != nil {
			return nil, err
		}
		ch.Metadata = json.RawMessage(meta)
		out = append(out, ch)
	}
	return out, rows.Err()
}

// SoftDelete marks a document 'deleted' (FR-SRC-02 delete is a soft delete: the
// row and its chunks are retained; retrieval's live_chunks view already excludes
// non-active documents). It is idempotent and reports whether the document exists.
func (TenantStore) SoftDelete(ctx context.Context, db *tenant.DB, id string) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	tag, err := db.Exec(ctx,
		`update documents set status = 'deleted', deleted_at = now()
		 where id = $1 and status <> 'deleted'`, id)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var exists bool
	if err := db.QueryRow(ctx, `select exists(select 1 from documents where id = $1)`, id).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}
