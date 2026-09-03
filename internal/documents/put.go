package documents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// PutInput is a fully-parsed, normalised, chunked AND embedded document version
// ready to persist (SPEC-05 §1 flow, up to the commit step). The parser, chunker
// and embedder (STORY-05.2..05.5) produce it; this store only writes it. Per
// SPEC-05 §5 the commit transaction is not opened until embeddings exist, so
// every chunk carries its embedding here — there is no unembedded staging.
type PutInput struct {
	// Identity (documents): (SourceID, ExternalID) is the unique key. SourceID is
	// an informational copy of a control-plane sources.id (Invariant 4, C-4) — no
	// cross-database FK.
	SourceID   string
	ExternalID string
	Title      *string
	URI        *string
	MimeType   *string
	Metadata   json.RawMessage // document-level metadata; nil => '{}'

	// Version (document_versions): immutable normalised content keyed by hash.
	ContentHash []byte // sha256 of the normalised text (ADR-0008)
	Content     string // normalised markdown/text
	CharCount   int
	Parser      *string
	RawRef      *string         // object-storage key of the original bytes, if kept
	RawJSON     json.RawMessage // original API payload, if any; nil => NULL

	Chunks []ChunkInput
}

// ChunkInput is one structure-aware chunk with its embedding (SPEC-05 §3/§4).
type ChunkInput struct {
	Position       int
	HeadingPath    []string
	Content        string
	TokenCount     int
	Embedding      []float32
	EmbeddingModel string
	Metadata       json.RawMessage // nil => '{}'
}

// PutResult reports what Put did. Changed is false when the content hash matched
// the current version and only last_seen_at was touched (SPEC-05 §5). VersionID
// is the current version after the call (the new one on a change, the existing
// one when unchanged).
type PutResult struct {
	DocumentID string
	VersionID  string
	Changed    bool
}

// Put upserts one document version (ADR-0008, SPEC-05 §5, FR-ING-02). Given an
// already-built version and its embedded chunks:
//
//   - if the document exists and the content hash equals the current version's,
//     it only touches documents.last_seen_at and returns Changed=false — no new
//     version, no chunk churn, no embedding cost;
//   - otherwise it inserts the new immutable document_versions row, writes its
//     chunks, and flips documents.current_version to it in ONE transaction, so a
//     query reading the live_chunks view never sees a half-built version and the
//     swap is instant at commit (Invariants 1 and 2).
//
// A content hash that matches a PRIOR (non-current) version — an A→B→A rollback —
// reuses that version and its existing chunks (immutable, Invariant 2) and only
// re-points current_version.
//
// It is reached only through a *tenant.DB from the resolver (ADR-0003, C-3): the
// database boundary is the tenant boundary, so there is no tenant_id and no way
// to write another tenant's content.
func (TenantStore) Put(ctx context.Context, db *tenant.DB, in PutInput) (PutResult, error) {
	if err := validatePut(in); err != nil {
		return PutResult{}, err
	}

	// Look up the current identity + current-version hash for the change test.
	var (
		docID   string
		curVer  *string
		curHash []byte
	)
	err := db.QueryRow(ctx, `
		select d.id::text, d.current_version::text, v.content_hash
		from documents d
		left join document_versions v on v.id = d.current_version
		where d.source_id = $1::uuid and d.external_id = $2`,
		in.SourceID, in.ExternalID).Scan(&docID, &curVer, &curHash)
	found := true
	if errors.Is(err, pgx.ErrNoRows) {
		found = false
	} else if err != nil {
		return PutResult{}, err
	}

	// Unchanged content: touch last_seen_at only (no version, no chunk churn).
	if found && curVer != nil && bytes.Equal(curHash, in.ContentHash) {
		if _, err := db.Exec(ctx,
			`update documents set last_seen_at = now() where id = $1::uuid`, docID); err != nil {
			return PutResult{}, err
		}
		return PutResult{DocumentID: docID, VersionID: *curVer, Changed: false}, nil
	}

	// New or changed: build the version + chunks and flip current_version in ONE
	// transaction so the swap is atomic for readers of live_chunks (ADR-0008).
	tx, err := db.Begin(ctx)
	if err != nil {
		return PutResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	// Upsert the identity; a changed doc is (re)activated and touched (SPEC-05 §5).
	if err := tx.QueryRow(ctx, `
		insert into documents (source_id, external_id, title, uri, mime_type, metadata, status, last_seen_at)
		values ($1::uuid, $2, $3, $4, $5, coalesce($6::jsonb, '{}'::jsonb), 'active', now())
		on conflict (source_id, external_id) do update set
			title = excluded.title,
			uri = excluded.uri,
			mime_type = excluded.mime_type,
			metadata = excluded.metadata,
			status = 'active',
			last_seen_at = now()
		returning id::text`,
		in.SourceID, in.ExternalID, in.Title, in.URI, in.MimeType, nullableJSON(in.Metadata)).
		Scan(&docID); err != nil {
		return PutResult{}, err
	}

	// Reuse a prior version with this exact hash (rollback) or insert a new one.
	var versionID string
	newVersion := false
	err = tx.QueryRow(ctx,
		`select id::text from document_versions where document_id = $1::uuid and content_hash = $2`,
		docID, in.ContentHash).Scan(&versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		newVersion = true
		err = tx.QueryRow(ctx, `
			insert into document_versions
				(document_id, content_hash, content, char_count, parser, raw_ref, raw_json)
			values ($1::uuid, $2, $3, $4, $5, $6, $7::jsonb)
			returning id::text`,
			docID, in.ContentHash, in.Content, in.CharCount, in.Parser, in.RawRef, nullableJSON(in.RawJSON)).
			Scan(&versionID)
	}
	if err != nil {
		return PutResult{}, err
	}

	// A new version gets new chunks; a reused version keeps its own (Invariant 2).
	if newVersion {
		for _, c := range in.Chunks {
			hp := c.HeadingPath
			if hp == nil {
				hp = []string{}
			}
			if _, err := tx.Exec(ctx, `
				insert into chunks
					(document_id, version_id, source_id, position, heading_path,
					 content, token_count, embedding, embedding_model, metadata)
				values ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8::vector, $9,
				        coalesce($10::jsonb, '{}'::jsonb))`,
				docID, versionID, in.SourceID, c.Position, hp,
				c.Content, c.TokenCount, vectorLiteral(c.Embedding), c.EmbeddingModel,
				nullableJSON(c.Metadata)); err != nil {
				return PutResult{}, err
			}
			// ponytail: one Exec per chunk (O(n) round trips inside the tx). Chunk
			// counts per document are small (tens); upgrade to a single multi-row
			// insert or CopyFrom if a document ever produces thousands of chunks.
		}
	}

	// The atomic swap: point current_version at the fully-built version, in the
	// same tx as the chunk writes. Readers on the live_chunks view see the old
	// version until this commits, then the new one instantly (Invariant 1).
	if _, err := tx.Exec(ctx, `
		update documents set current_version = $1::uuid, last_seen_at = now(), status = 'active'
		where id = $2::uuid`, versionID, docID); err != nil {
		return PutResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return PutResult{}, err
	}
	return PutResult{DocumentID: docID, VersionID: versionID, Changed: true}, nil
}

// validatePut checks the invariants Put relies on before touching the database.
// Embeddings are required because SPEC-05 §5 opens the commit transaction only
// once the version is fully embedded (there is no unembedded chunk staging).
func validatePut(in PutInput) error {
	if in.ExternalID == "" {
		return invalid("external_id is required")
	}
	if !validUUID(in.SourceID) {
		return invalid("source_id must be a UUID")
	}
	if len(in.ContentHash) == 0 {
		return invalid("content_hash is required")
	}
	for i, c := range in.Chunks {
		if len(c.Embedding) == 0 {
			return invalid("chunk %d: embedding is required at commit (SPEC-05 §5)", i)
		}
		if c.EmbeddingModel == "" {
			return invalid("chunk %d: embedding_model is required", i)
		}
	}
	return nil
}

// vectorLiteral renders a []float32 as a pgvector text literal ("[a,b,c]") so a
// chunk's embedding can be written with $n::vector — avoids a pgvector codec
// dependency for one column. The dimension is validated by the column type.
func vectorLiteral(v []float32) string {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// nullableJSON returns a value pgx sends as jsonb: a string (cast ::jsonb) for
// non-empty JSON, or nil for SQL NULL. Passing a []byte would encode as bytea,
// which cannot cast to jsonb.
func nullableJSON(m json.RawMessage) any {
	if len(m) == 0 {
		return nil
	}
	return string(m)
}
