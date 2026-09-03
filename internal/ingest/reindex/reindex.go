// Package reindex is the resumable reindex operation (STORY-05.8, FR-ING-09,
// SPEC-05 §7, SPEC-03 §5): it migrates a tenant's live chunks to a NEW embedding
// model and/or dimension while retrieval keeps serving from the current chunks
// table. It builds a side table chunks_new at the new dimension, re-embeds every
// live version into it with the new model, then performs the SPEC-03 §5 atomic
// swap and drops the old table only after a coverage verification.
//
// It owns no SQL of its own: every tenant write goes through documents.Store and
// thus a *tenant.DB from the resolver (ADR-0003, C-1, C-3). It is deliberately
// NOT a River worker — the driving worker (STORY-09.1) calls Prepare once, then
// Step in a loop persisting the returned cursor between calls (so a crash or retry
// resumes from the last committed version rather than restarting), then Verify,
// Swap and DropOld. Because the cursor is the caller's durable state and each
// version's write is atomic/idempotent (documents.InsertChunksNew), the operation
// is crash-safe end to end.
//
// Dimension immutability (ADR-0022): a tenant's settings.embedding_dim is immutable
// to ordinary PATCHes precisely so a dimension change goes through THIS sanctioned
// path. The physical vector(N) column moves to the new dimension via the swap
// (chunks_new is built at NewDim and renamed to chunks). Moving the control-plane
// settings.embedding_dim mirror and the configured model to the new values is the
// driving worker's finalize step (it holds the control pool; this operation holds
// only a tenant handle) — see ADR-0039.
package reindex

import (
	"context"
	"errors"
	"fmt"

	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/chunk"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// defaultBatchDocs is how many live versions Step processes per call when Config
// leaves BatchDocs unset. The batch bounds how much work a single Step commits
// before the caller can persist the advanced cursor.
const defaultBatchDocs = 50

// ErrNotVerified is returned by Swap and DropOld when the target table does not yet
// cover every live version, so neither the swap nor the drop of the old table is
// allowed to proceed (SPEC-05 §7 verify-before-drop).
var ErrNotVerified = errors.New("reindex: chunks not verified complete")

// LocalParser re-parses a stored version's normalised markdown back into blocks for
// the re-chunk path (parse.Default()); only needed when Config.Rechunk is set.
type LocalParser interface {
	Parse(contentType string, data []byte) (parse.Normalised, error)
}

// Store is the tenant-content persistence the reindex needs — the subset of
// documents.Store for the table swap, satisfied by documents.TenantStore. Every
// method takes the resolved *tenant.DB (ADR-0003, C-3).
type Store interface {
	CreateChunksNew(ctx context.Context, db *tenant.DB, dim int) error
	LiveVersionsAfter(ctx context.Context, db *tenant.DB, afterDocID string, limit int) ([]documents.ReindexVersion, error)
	VersionChunks(ctx context.Context, db *tenant.DB, versionID string) ([]documents.ReindexChunk, error)
	InsertChunksNew(ctx context.Context, db *tenant.DB, versionID string, rows []documents.ReindexChunk) error
	VerifyCoverage(ctx context.Context, db *tenant.DB, table string) (documents.ReindexCoverage, error)
	SwapChunks(ctx context.Context, db *tenant.DB) error
	DropChunksOld(ctx context.Context, db *tenant.DB) error
}

// Config assembles one reindex run. The worker builds it per job: the resolved
// tenant DB, the store, an Embedder already selected for the NEW provider/model
// from settings.providers_allowed (ADR-0037), and the target model/dimension.
type Config struct {
	DB       *tenant.DB     // resolved tenant handle (ADR-0003); nil only in unit tests with a fake Store
	Store    Store          // documents.TenantStore in production
	Embedder embed.Embedder // produces NewDim-length vectors with NewModel
	Local    LocalParser    // parse.Default(); only required when Rechunk is true

	NewModel string // embedding model stamped on every new chunk
	NewDim   int    // new embedding dimension (the vector(N) of chunks_new)

	// Rechunk re-splits each version's stored content with Chunk before embedding
	// (SPEC-05 §7 "re-chunks if chunking settings changed"). When false (the common
	// model/dimension-only reindex) the existing chunk text is re-embedded verbatim,
	// which is exact and cheaper — no re-parse.
	Rechunk bool
	Chunk   chunk.Config // chunker settings for the re-chunk path

	// BatchDocs bounds how many live versions one Step processes; 0 uses the default.
	BatchDocs int
}

// Progress is the resumable cursor the caller persists between Step calls. Cursor
// is the last document_id fully indexed into chunks_new; passing it back to Step
// continues after it. Done is true once every live version has been indexed. The
// counters are this Step's contribution (the caller accumulates across Steps).
type Progress struct {
	Cursor        string `json:"cursor"`
	Done          bool   `json:"done"`
	DocsIndexed   int    `json:"docs_indexed"`
	ChunksWritten int    `json:"chunks_written"`
	EmbedTokens   int    `json:"embed_tokens"`
}

// Reindexer runs one reindex operation. It holds no mutable run state beyond its
// config; all progress is the cursor the caller passes to Step, so the operation is
// safe to resume in a fresh process.
type Reindexer struct {
	cfg Config
}

// New builds a Reindexer, defaulting the batch size.
func New(cfg Config) *Reindexer {
	if cfg.BatchDocs <= 0 {
		cfg.BatchDocs = defaultBatchDocs
	}
	return &Reindexer{cfg: cfg}
}

// Prepare creates the empty chunks_new side table at the new dimension (SPEC-03 §5
// step 1). It is idempotent and safe to call again on a resume: an existing
// chunks_new is left in place. Queries continue on the current chunks table.
func (r *Reindexer) Prepare(ctx context.Context) error {
	if r.cfg.NewDim <= 0 {
		return fmt.Errorf("reindex: new dimension must be positive, got %d", r.cfg.NewDim)
	}
	if r.cfg.NewModel == "" {
		return errors.New("reindex: new model is required")
	}
	return r.cfg.Store.CreateChunksNew(ctx, r.cfg.DB, r.cfg.NewDim)
}

// Step indexes up to BatchDocs live versions after cursor (document_id order) into
// chunks_new, re-embedding with the new model, and returns the advanced cursor.
// Pass the returned Cursor back to continue; Done is true when no live version
// remains. Each version is written atomically and idempotently, so re-running a
// Step from a stale cursor after a crash re-does at most the in-flight version
// rather than duplicating or restarting.
func (r *Reindexer) Step(ctx context.Context, cursor string) (Progress, error) {
	p := Progress{Cursor: cursor}
	versions, err := r.cfg.Store.LiveVersionsAfter(ctx, r.cfg.DB, cursor, r.cfg.BatchDocs)
	if err != nil {
		return p, err
	}
	if len(versions) == 0 {
		p.Done = true
		return p, nil
	}

	for _, v := range versions {
		rows, texts, err := r.rowsFor(ctx, v)
		if err != nil {
			return p, err
		}
		res, err := r.cfg.Embedder.Embed(ctx, texts)
		if err != nil {
			return p, err // circuit-open/quota is the worker's to classify (snooze); here we surface it
		}
		if len(res.Vectors) != len(rows) {
			return p, fmt.Errorf("reindex: embedder returned %d vectors for %d chunks (version %s)",
				len(res.Vectors), len(rows), v.VersionID)
		}
		for i := range rows {
			rows[i].Embedding = res.Vectors[i]
			rows[i].EmbeddingModel = r.cfg.NewModel
		}
		if err := r.cfg.Store.InsertChunksNew(ctx, r.cfg.DB, v.VersionID, rows); err != nil {
			return p, err
		}
		p.Cursor = v.DocumentID
		p.DocsIndexed++
		p.ChunksWritten += len(rows)
		p.EmbedTokens += res.Tokens
	}

	// Fewer than a full batch means this source is exhausted; a full batch means a
	// further Step is needed (it will return Done on the next empty page).
	p.Done = len(versions) < r.cfg.BatchDocs
	return p, nil
}

// Run drives Step to completion from the given cursor, accumulating progress. It is
// a convenience for a caller that does not need to persist intermediate cursors
// (e.g. the e2e); the worker calls Step directly so it can checkpoint the cursor.
func (r *Reindexer) Run(ctx context.Context, cursor string) (Progress, error) {
	total := Progress{Cursor: cursor}
	for {
		p, err := r.Step(ctx, total.Cursor)
		total.Cursor = p.Cursor
		total.DocsIndexed += p.DocsIndexed
		total.ChunksWritten += p.ChunksWritten
		total.EmbedTokens += p.EmbedTokens
		if err != nil {
			return total, err
		}
		if p.Done {
			total.Done = true
			return total, nil
		}
	}
}

// Verify reports whether chunks_new covers every live version — the SPEC-05 §7
// go/no-go before the swap.
func (r *Reindexer) Verify(ctx context.Context) (documents.ReindexCoverage, error) {
	return r.cfg.Store.VerifyCoverage(ctx, r.cfg.DB, "chunks_new")
}

// Swap performs the SPEC-03 §5 atomic table swap, but only once chunks_new is
// verified to cover every live version; otherwise it returns ErrNotVerified and
// changes nothing. After a successful swap the new chunks table is live (at the new
// dimension) and the old one is chunks_old, awaiting DropOld.
func (r *Reindexer) Swap(ctx context.Context) error {
	cov, err := r.Verify(ctx)
	if err != nil {
		return err
	}
	if !cov.Covered() {
		return fmt.Errorf("%w: chunks_new covers %d of %d live versions",
			ErrNotVerified, cov.CoveredVersions, cov.LiveVersions)
	}
	return r.cfg.Store.SwapChunks(ctx, r.cfg.DB)
}

// DropOld drops the retired chunks_old table, but only after re-verifying that the
// now-live chunks table (post-swap) covers every live version (SPEC-05 §7
// verify-before-drop). The re-check reads durable DB state, so it holds even across
// a crash between Swap and DropOld. On an incomplete table it returns ErrNotVerified
// and leaves chunks_old in place.
func (r *Reindexer) DropOld(ctx context.Context) error {
	cov, err := r.cfg.Store.VerifyCoverage(ctx, r.cfg.DB, "chunks")
	if err != nil {
		return err
	}
	if !cov.Covered() {
		return fmt.Errorf("%w: refusing to drop chunks_old, live chunks cover %d of %d live versions",
			ErrNotVerified, cov.CoveredVersions, cov.LiveVersions)
	}
	return r.cfg.Store.DropChunksOld(ctx, r.cfg.DB)
}

// rowsFor produces one version's chunks_new rows (without embeddings) plus the
// aligned texts to embed. In the reuse path it re-embeds the stored chunk text with
// its reconstructed context line; in the re-chunk path it re-splits the stored
// content with the configured chunker.
func (r *Reindexer) rowsFor(ctx context.Context, v documents.ReindexVersion) ([]documents.ReindexChunk, []string, error) {
	if r.cfg.Rechunk {
		return r.rechunk(v)
	}
	return r.reuse(ctx, v)
}

// reuse re-embeds a version's existing chunks verbatim (chunking settings
// unchanged): same position/heading_path/content/token_count, embed text
// reconstructed from title+heading_path so the new vectors use the same SPEC-05 §3
// context line the sink embedded originally.
func (r *Reindexer) reuse(ctx context.Context, v documents.ReindexVersion) ([]documents.ReindexChunk, []string, error) {
	old, err := r.cfg.Store.VersionChunks(ctx, r.cfg.DB, v.VersionID)
	if err != nil {
		return nil, nil, err
	}
	texts := make([]string, len(old))
	for i, c := range old {
		texts[i] = chunk.WithContext(deref(v.Title), c.HeadingPath, c.Content)
	}
	return old, texts, nil
}

// rechunk re-splits the version's stored normalised markdown with the configured
// chunker (SPEC-05 §7 "re-chunks if chunking settings changed"), producing fresh
// chunk boundaries and the chunker's own EmbedText.
//
// ponytail: it re-parses the STORED markdown (document_versions.content), not the
// original bytes, so a version whose original was PDF/DOCX will re-chunk from its
// normalised markdown rather than the source structure. Ceiling: chunk boundaries
// may differ slightly from a fresh ingest of the original. Upgrade path: re-fetch
// raw_ref bytes and re-run the full parse when byte-faithful re-chunking is needed.
func (r *Reindexer) rechunk(v documents.ReindexVersion) ([]documents.ReindexChunk, []string, error) {
	if r.cfg.Local == nil {
		return nil, nil, errors.New("reindex: Rechunk requires a LocalParser")
	}
	norm, err := r.cfg.Local.Parse("text/markdown", []byte(v.Content))
	if err != nil {
		return nil, nil, fmt.Errorf("reindex: re-parse version %s: %w", v.VersionID, err)
	}
	chunks := chunk.Document(norm, r.cfg.Chunk)
	rows := make([]documents.ReindexChunk, len(chunks))
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		rows[i] = documents.ReindexChunk{
			DocumentID:  v.DocumentID,
			VersionID:   v.VersionID,
			SourceID:    v.SourceID,
			Position:    c.Position,
			HeadingPath: c.HeadingPath,
			Content:     c.Content,
			TokenCount:  c.TokenCount,
		}
		texts[i] = c.EmbedText
	}
	return rows, texts, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
