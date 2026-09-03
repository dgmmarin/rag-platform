// Package sink is the per-document ingestion orchestrator (SPEC-05 §1/§5,
// FR-ING-07, NFR-REL-02). A connector (EPIC-06/07) pushes each fetched document
// at Sink.Put, which composes the already-built stages — parse (Go registry or
// Python sidecar), normalise + hash, the SPEC-05 §1 hash short-circuit, chunk,
// embed, and the ADR-0008 single-transaction store — then Sink.Complete performs
// the full-sync soft-delete of documents not seen this run. It owns no SQL of its
// own: every tenant write goes through documents.Store and thus a *tenant.DB from
// the resolver (ADR-0003, C-3).
//
// Crash safety (the NFR-REL-02 teeth): each changed document is committed by its
// own store.Put transaction (SPEC-05 §5), so a worker crash mid-sync leaves no
// partial document — completed documents are whole and unseen ones are skipped by
// hash on the retry (TouchIfUnchanged). The sink accumulates a forward-compatible
// Stats the worker (STORY-05.7) will persist into jobs.stats/usage_daily; this
// package fills the struct but writes neither.
package sink

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/chunk"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// Mode selects whether Complete sweeps unseen documents. A full sync has seen the
// source's entire corpus, so absence means deletion; an incremental sync has not,
// so it never deletes (SPEC-05 §1/§5).
type Mode int

const (
	// Incremental is a partial sync (e.g. sitemap lastmod, API cursor). Complete is
	// a no-op: unseen documents are NOT deleted.
	Incremental Mode = iota
	// Full is a complete re-listing of the source; Complete soft-deletes documents
	// whose last_seen_at predates the run start.
	Full
)

// Document is one unit a connector pushes at the sink: the raw bytes plus the
// identity and metadata the store records. It is the connector-facing input; the
// sink parses/chunks/embeds it into a documents.PutInput.
type Document struct {
	// ExternalID is the source-stable id (documents.external_id); (SourceID,
	// ExternalID) is the document identity.
	ExternalID string
	// Filename is advisory, forwarded to the sidecar as the multipart filename.
	Filename string
	// MimeType is the canonical MIME the parse registry / sidecar dispatch on.
	MimeType string
	// Data is the raw document bytes (bounded by the connector; a document is small
	// enough to hold in memory, matching the parser/sidecar contract).
	Data []byte

	// Optional metadata copied onto the stored document/version.
	Title    *string         // connector-supplied title; falls back to the parsed title
	URI      *string         // canonical URI, if any
	Metadata json.RawMessage // document-level metadata; nil => '{}'
	RawRef   *string         // object-storage key of the original bytes, if kept
	RawJSON  json.RawMessage // original API payload, if any

	// BytesFetched is folded into stats.bytes_fetched; 0 falls back to len(Data).
	BytesFetched int
}

// DocError is one per-document failure recorded in stats (SPEC-05 §6). The error
// message is client-safe metadata; it never carries document content.
type DocError struct {
	ExternalID string `json:"external_id"`
	Msg        string `json:"msg"`
}

// maxDocErrors caps stats.errors per SPEC-05 §6: keep at most the first 100
// {external_id, msg} entries. docs_failed keeps counting past the cap (see
// recordFailure), so the count stays honest even when the list is truncated.
const maxDocErrors = 100

// Stats accumulates the SPEC-05 §6 job statistics as the sink runs. The JSON tags
// match the jobs.stats shape so the worker (STORY-09.1) can marshal it directly;
// this package only fills the struct. Errors is capped at maxDocErrors (SPEC-05
// §6); DocsFailed keeps counting past the cap, so the count is honest even when
// the list is truncated. Stats() guarantees Errors marshals as a list ([], never
// null) so the persisted value always matches the SPEC-05 §6 shape.
type Stats struct {
	DocsSeen      int        `json:"docs_seen"`
	DocsChanged   int        `json:"docs_changed"`
	DocsUnchanged int        `json:"docs_unchanged"`
	DocsDeleted   int        `json:"docs_deleted"`
	DocsFailed    int        `json:"docs_failed"`
	ChunksWritten int        `json:"chunks_written"`
	EmbedTokens   int        `json:"embed_tokens"`
	BytesFetched  int64      `json:"bytes_fetched"`
	DurationMS    int64      `json:"duration_ms"`
	Errors        []DocError `json:"errors"`
}

// SnoozeError signals the caller (the worker) to SNOOZE the job — pause and retry
// later — rather than fail it, per SPEC-05 §8 ("provider quota exhausted → job
// paused, not failed"). It is returned when the embedding circuit breaker is open
// (embed.ErrCircuitOpen). Nothing was committed, so the retry re-processes the
// document from scratch (unchanged ones skipped by hash).
type SnoozeError struct{ Err error }

func (e *SnoozeError) Error() string { return "ingest: snooze job: " + e.Err.Error() }
func (e *SnoozeError) Unwrap() error { return e.Err }

// LocalParser is the native-Go parse registry (parse.Default()). It returns
// parse.ErrUnsupportedMIME for the heavy formats, which the sink routes to the
// sidecar.
type LocalParser interface {
	Parse(contentType string, data []byte) (parse.Normalised, error)
}

// SidecarParser is the Python parsing sidecar client (sidecar.Client) for the
// formats the Go parsers cannot read (PDF/DOCX/PPTX/XLSX).
type SidecarParser interface {
	Parse(ctx context.Context, filename, mimeType string, data []byte) (parse.Normalised, error)
}

// Store is the tenant-content persistence the sink needs (a subset of
// documents.Store, satisfied by documents.TenantStore). Every method takes the
// resolved *tenant.DB — the only path to tenant data (ADR-0003, C-3).
type Store interface {
	TouchIfUnchanged(ctx context.Context, db *tenant.DB, sourceID, externalID string, hash []byte) (bool, error)
	Put(ctx context.Context, db *tenant.DB, in documents.PutInput) (documents.PutResult, error)
	SoftDeleteUnseen(ctx context.Context, db *tenant.DB, sourceID string, startedAt time.Time) (int, error)
}

// Config assembles a sink for one sync run of one source. The worker (STORY-09.1)
// builds it per job: the resolved tenant DB, the store, the parser pair, an
// Embedder already selected from settings.providers_allowed (ADR-0037), the
// source id, sync mode, chunker config and embedding model.
type Config struct {
	DB       *tenant.DB    // resolved tenant handle (ADR-0003); nil only in unit tests with a fake Store
	Store    Store         // documents.TenantStore in production
	Local    LocalParser   // parse.Default()
	Sidecar  SidecarParser // sidecar.Client; may be nil if no heavy formats are expected
	Embedder embed.Embedder
	SourceID string
	Mode     Mode
	Chunk    chunk.Config // target/overlap from tenant settings; zero values use SPEC-05 §3 defaults
	Model    string       // embedding model id stamped on every chunk (Invariant 3)

	// Now is the clock; nil uses time.Now. startedAt is captured at New.
	Now func() time.Time
}

// Sink orchestrates one sync run. It is not safe for concurrent Put calls (a
// connector drives one document at a time); Stats reads a consistent snapshot.
type Sink struct {
	cfg       Config
	now       func() time.Time
	startedAt time.Time
	stats     Stats
}

// New builds a sink and captures the run's start time (the boundary Complete
// compares last_seen_at against).
func New(cfg Config) *Sink {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Sink{cfg: cfg, now: now, startedAt: now()}
}

// Put ingests one document through the SPEC-05 §1 flow. It returns:
//   - nil on success (a changed document committed, or an unchanged one touched)
//     AND on a recorded per-document failure (parse or non-circuit embed error) —
//     a single bad document is recorded in Stats and the sync continues (§2/§8);
//   - a *SnoozeError when the embedding circuit is open, so the worker snoozes the
//     whole job rather than failing it (§8);
//   - the underlying error on an infrastructure failure (store/DB), so the worker
//     fails the job for retry — completed documents stay whole (per-doc tx, §5).
func (s *Sink) Put(ctx context.Context, doc Document) error {
	s.stats.DocsSeen++
	s.stats.BytesFetched += int64(bytesFetched(doc))

	// 1. Parse: Go registry, routing the heavy formats to the sidecar. A parse
	// failure is a single-document error — recorded and skipped (SPEC-05 §2/§8).
	norm, parser, err := s.parse(ctx, doc)
	if err != nil {
		s.recordFailure(doc.ExternalID, err)
		return nil
	}

	// 2. Normalise + hash (SPEC-05 §1).
	content := norm.Markdown()
	sum := sha256.Sum256([]byte(content))
	hash := sum[:]

	// 3. Compare to the current version hash BEFORE chunk/embed: an unchanged
	// document only touches last_seen_at, costing no embedding (SPEC-05 §1).
	unchanged, err := s.cfg.Store.TouchIfUnchanged(ctx, s.cfg.DB, s.cfg.SourceID, doc.ExternalID, hash)
	if err != nil {
		return err // infrastructure error: fail the job for retry
	}
	if unchanged {
		s.stats.DocsUnchanged++
		return nil
	}

	// 4. Chunk (structure-aware; target/overlap from settings).
	chunks := chunk.Document(norm, s.cfg.Chunk)

	// 5. Embed (batched, with the breaker/retry inside the Embedder). A circuit-open
	// snoozes the job; any other embed error is a recorded per-document failure —
	// the document keeps its previous version because the commit tx is never opened
	// (SPEC-05 §5). The breaker escalates a sustained provider outage into a snooze.
	res, err := s.cfg.Embedder.Embed(ctx, embedTexts(chunks))
	if err != nil {
		if errors.Is(err, embed.ErrCircuitOpen) {
			return &SnoozeError{Err: err}
		}
		s.recordFailure(doc.ExternalID, err)
		return nil
	}
	if len(res.Vectors) != len(chunks) {
		return fmt.Errorf("sink: embedder returned %d vectors for %d chunks", len(res.Vectors), len(chunks))
	}

	// 6. Commit: insert version + chunks and flip current_version in ONE
	// transaction (ADR-0008, SPEC-05 §5). A store error fails the job for retry;
	// nothing partial is left behind (the transaction rolls back).
	in := s.putInput(doc, norm, content, hash, parser, chunks, res.Vectors)
	if _, err := s.cfg.Store.Put(ctx, s.cfg.DB, in); err != nil {
		return err
	}

	s.stats.DocsChanged++
	s.stats.ChunksWritten += len(chunks)
	s.stats.EmbedTokens += res.Tokens
	return nil
}

// Complete finishes the run. On a FULL sync it soft-deletes documents of the
// source not seen since startedAt (SPEC-05 §1/§5); on an incremental sync it is a
// no-op. It is the last call of the run, after every Put.
func (s *Sink) Complete(ctx context.Context) error {
	if s.cfg.Mode != Full {
		return nil
	}
	deleted, err := s.cfg.Store.SoftDeleteUnseen(ctx, s.cfg.DB, s.cfg.SourceID, s.startedAt)
	if err != nil {
		return err
	}
	s.stats.DocsDeleted += deleted
	return nil
}

// Stats returns a snapshot of the accumulated statistics, with duration_ms filled
// from the run start to now. Errors is normalised to a non-nil slice so it
// marshals as a list ([], never null) — the SPEC-05 §6 jobs.stats shape.
func (s *Sink) Stats() Stats {
	out := s.stats
	out.DurationMS = s.now().Sub(s.startedAt).Milliseconds()
	if out.Errors == nil {
		out.Errors = []DocError{}
	}
	return out
}

// parse dispatches to the Go registry and, on parse.ErrUnsupportedMIME, to the
// sidecar (SPEC-05 §1/§2). It returns the Normalised document and a parser label
// for the version record.
func (s *Sink) parse(ctx context.Context, doc Document) (parse.Normalised, string, error) {
	norm, err := s.cfg.Local.Parse(doc.MimeType, doc.Data)
	if err == nil {
		return norm, doc.MimeType, nil
	}
	if !errors.Is(err, parse.ErrUnsupportedMIME) {
		return parse.Normalised{}, "", err // a real Go parse error (malformed input)
	}
	if s.cfg.Sidecar == nil {
		return parse.Normalised{}, "", fmt.Errorf("sink: no sidecar configured for %q: %w", doc.MimeType, err)
	}
	norm, err = s.cfg.Sidecar.Parse(ctx, doc.Filename, doc.MimeType, doc.Data)
	if err != nil {
		return parse.Normalised{}, "", err
	}
	return norm, "sidecar", nil
}

// putInput assembles the store's PutInput from the parsed/chunked/embedded
// document. Each chunk is stamped with the configured embedding model (Invariant
// 3) and its aligned vector.
func (s *Sink) putInput(doc Document, norm parse.Normalised, content string, hash []byte, parser string, chunks []chunk.Chunk, vectors [][]float32) documents.PutInput {
	in := documents.PutInput{
		SourceID:    s.cfg.SourceID,
		ExternalID:  doc.ExternalID,
		Title:       docTitle(doc, norm),
		URI:         doc.URI,
		MimeType:    strPtr(doc.MimeType),
		Metadata:    doc.Metadata,
		ContentHash: hash,
		Content:     content,
		CharCount:   utf8.RuneCountInString(content),
		Parser:      strPtr(parser),
		RawRef:      doc.RawRef,
		RawJSON:     doc.RawJSON,
	}
	in.Chunks = make([]documents.ChunkInput, len(chunks))
	for i, c := range chunks {
		in.Chunks[i] = documents.ChunkInput{
			Position:       c.Position,
			HeadingPath:    c.HeadingPath,
			Content:        c.Content,
			TokenCount:     c.TokenCount,
			Embedding:      vectors[i],
			EmbeddingModel: s.cfg.Model,
		}
	}
	return in
}

// recordFailure counts a per-document failure and appends its {external_id, msg}
// to stats.errors, capped at maxDocErrors (SPEC-05 §6). docs_failed always
// increments — including past the cap — so the count is honest even when the
// error list is truncated. A doc counted failed here contributed no version and
// no chunks (SPEC-05 §5/§8).
func (s *Sink) recordFailure(externalID string, err error) {
	s.stats.DocsFailed++
	if len(s.stats.Errors) < maxDocErrors {
		s.stats.Errors = append(s.stats.Errors, DocError{ExternalID: externalID, Msg: err.Error()})
	}
}

func embedTexts(chunks []chunk.Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.EmbedText
	}
	return out
}

func bytesFetched(doc Document) int {
	if doc.BytesFetched > 0 {
		return doc.BytesFetched
	}
	return len(doc.Data)
}

// docTitle prefers the connector-supplied title, falling back to the parsed title
// when the connector supplied none and the body carried one.
func docTitle(doc Document, norm parse.Normalised) *string {
	if doc.Title != nil {
		return doc.Title
	}
	if norm.Title != "" {
		return &norm.Title
	}
	return nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
