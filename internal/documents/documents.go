// Package documents is the tenant-content document API (SPEC-07 §2, FR-SRC-02,
// FR-ADM-03). Documents, their versions and their chunks are TENANT content
// (schemas/tenant.sql), never control-plane data (C-3): every read and the soft
// delete reach the tenant database ONLY through a *tenant.DB obtained from the
// Resolver (ADR-0003) — there is no tenant_id column and no control-plane path
// to this data. The tenant is always the one resolved from the authenticated
// principal (FR-ACC-03), never a request parameter.
//
// The ingest job it enqueues (ingest_document) is control-plane queue state, so
// that one write goes to the control-plane jobs table via a JobEnqueuer; the raw
// upload bytes go to object storage via the Storage seam (EPIC-06).
//
// The WRITE side of this same tenant content — building a version and its chunks
// and flipping documents.current_version atomically — is TenantStore.Put (put.go,
// STORY-05.1, ADR-0008/ADR-0033): given a fully parsed, chunked and embedded
// version, it persists it in one transaction so a query never sees a half-built
// document. The upstream pipeline stages that produce a PutInput (parser, chunker,
// embedder) and the sink orchestration/worker are EPIC-05.2+ and EPIC-09; the
// upload connector is EPIC-06.
package documents

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

const (
	defaultListLimit = 50
	maxListLimit     = 200
	// defaultMaxUploadBytes is the FR-SRC-02 default upload ceiling (50 MB). It is
	// configurable (config.MaxUploadBytes); this is the fallback floor.
	defaultMaxUploadBytes int64 = 50 << 20
)

// Document is the API view of a tenant document (schemas/tenant.sql documents).
// It is the list-row projection; Get returns a DocumentDetail that adds the
// current version metadata.
type Document struct {
	ID             string          `json:"id"`
	SourceID       string          `json:"source_id"`
	ExternalID     string          `json:"external_id"`
	Title          *string         `json:"title,omitempty"`
	URI            *string         `json:"uri,omitempty"`
	MimeType       *string         `json:"mime_type,omitempty"`
	Status         string          `json:"status"`
	CurrentVersion *string         `json:"current_version,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	FirstSeenAt    time.Time       `json:"first_seen_at"`
	LastSeenAt     time.Time       `json:"last_seen_at"`
	DeletedAt      *time.Time      `json:"deleted_at,omitempty"`
}

// VersionMeta is the current-version metadata returned with a document. The full
// normalised text (document_versions.content) is included only when the caller
// asks (?content=true), keeping the default Get response small.
type VersionMeta struct {
	ID          string    `json:"id"`
	ContentHash string    `json:"content_hash"` // hex-encoded sha256
	CharCount   int       `json:"char_count"`
	Parser      *string   `json:"parser,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Content     *string   `json:"content,omitempty"` // only when ?content=true
}

// DocumentDetail is GET /v1/documents/{id}: the document plus its current version
// metadata (SPEC-07 §2). CurrentVersion is nil when the document has no built
// version yet (should not happen for an active document — SPEC-03 §2 invariant 1).
type DocumentDetail struct {
	Document
	CurrentVersion *VersionMeta `json:"current_version_meta,omitempty"`
}

// Chunk is the debugging view of a chunk (FR-ADM-03). The embedding vector is
// deliberately never returned: it is large, opaque, and not useful to a human
// debugging retrieval.
type Chunk struct {
	ID             string          `json:"id"`
	Position       int             `json:"position"`
	HeadingPath    []string        `json:"heading_path"`
	Content        string          `json:"content"`
	TokenCount     int             `json:"token_count"`
	EmbeddingModel string          `json:"embedding_model"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Page is the SPEC-07 §1 pagination envelope for documents.
type Page struct {
	Items      []Document `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// ChunkPage is the SPEC-07 §1 pagination envelope for a document's chunks.
type ChunkPage struct {
	Items      []Chunk `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// ListFilter selects documents by source, status and a free-text query (SPEC-07
// §2: "filter by source, status, q"). An empty field is "no filter".
type ListFilter struct {
	SourceID string
	Status   string
	Q        string
}

// Cursor is the opaque keyset position for document List pagination: the
// (first_seen_at, id) of the last returned row.
type Cursor struct {
	FirstSeenAt time.Time `json:"f"`
	ID          string    `json:"i"`
}

// ChunkCursor is the keyset position for chunk pagination: the position of the
// last returned chunk (chunks are ordered by position within the current version).
type ChunkCursor struct {
	Position int `json:"p"`
}

// encodeCursor serialises a Cursor to an opaque base64url token.
func encodeCursor(c Cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a base64url document cursor token.
func decodeCursor(s string) (*Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// encodeChunkCursor / decodeChunkCursor are the chunk-page equivalents.
func encodeChunkCursor(c ChunkCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeChunkCursor(s string) (*ChunkCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c ChunkCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// clampLimit applies the default and maximum page sizes.
func clampLimit(n int) int {
	if n <= 0 {
		return defaultListLimit
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// validUUID is a cheap guard so a non-UUID path segment scans as ErrNotFound
// rather than raising a Postgres type error (mirrors internal/cp/sources).
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// invalid builds a ValidationError with a formatted message.
func invalid(format string, args ...any) *ValidationError {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}
