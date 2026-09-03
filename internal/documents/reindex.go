package documents

import (
	"context"
	"fmt"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// This file is the tenant-side SQL for the reindex table swap (STORY-05.8,
// FR-ING-09, SPEC-05 §7, SPEC-03 §5). It lives beside Put/TouchIfUnchanged because
// it is the SAME tenant content (chunks) on the SAME tables, reached ONLY through a
// *tenant.DB (ADR-0003, C-1, C-3) — the database boundary is the tenant boundary,
// so there is no tenant_id and no cross-tenant path. The orchestration (which
// iterates live versions, re-embeds with the new model and drives these steps in
// order) is internal/ingest/reindex; this store owns the DDL/DML only.
//
// The physical vector(N) column moves to the new dimension purely here: chunks_new
// is built at the new dimension and the swap renames it to chunks. The control-plane
// settings.embedding_dim mirror (ADR-0022) is moved by the driving worker as the
// finalize step of the same job (the sanctioned dimension-change path); it is not a
// tenant-DB object and so is out of this store's reach (ADR-0039).

// ReindexVersion is one live version to re-embed: the current version of an active
// document, in document_id order (the resume cursor). Content is the normalised
// markdown (document_versions.content) used only when re-chunking.
type ReindexVersion struct {
	DocumentID string
	VersionID  string
	SourceID   string
	Title      *string
	Content    string
}

// ReindexChunk is one row of the side table chunks_new. VersionChunks returns it
// with Embedding empty (the stored chunk to re-embed); InsertChunksNew consumes it
// with Embedding/EmbeddingModel filled by the caller (the new model's vectors).
type ReindexChunk struct {
	DocumentID     string
	VersionID      string
	SourceID       string
	Position       int
	HeadingPath    []string
	Content        string
	TokenCount     int
	Embedding      []float32
	EmbeddingModel string
}

// ReindexCoverage is the verification flag SPEC-05 §7 requires before dropping the
// old table: every live version (active document current_version) must have chunks
// in the target table. Covered() is the go/no-go for the swap and the drop.
type ReindexCoverage struct {
	LiveVersions    int // active documents with a non-null current_version
	CoveredVersions int // of those, how many have >=1 row in the target table
	TargetRows      int // rows in the target table (chunks_new or the swapped chunks)
	LiveChunks      int // rows visible through live_chunks (informational)
}

// Covered reports whether every live version is represented in the target table —
// the verification flag. An empty tenant (0 live versions) is trivially covered.
func (c ReindexCoverage) Covered() bool { return c.LiveVersions == c.CoveredVersions }

// reindexTables is the closed set of table names VerifyCoverage may target. The
// value is a hardcoded constant chosen by the reindexer (never client input), but
// it is checked against this allowlist so the string interpolation the DDL needs
// (a table identifier cannot be a bind parameter) can never carry anything else.
var reindexTables = map[string]bool{"chunks": true, "chunks_new": true}

// CreateChunksNew builds the side table chunks_new at the NEW embedding dimension
// (SPEC-03 §5 step 1), mirroring the chunks shape but with vector(dim). It is
// idempotent/resumable: if chunks_new already exists (a crashed prior attempt) it
// is left in place so the build resumes rather than restarting. Queries keep
// serving from chunks throughout — this only adds a new, empty table.
//
// The dimension cannot be a bind parameter (it is part of the column TYPE), so it
// is interpolated as a validated integer, exactly as the tenant-migration runner
// substitutes EMBEDDING_DIM (ADR-0015).
func (TenantStore) CreateChunksNew(ctx context.Context, db *tenant.DB, dim int) error {
	if dim <= 0 {
		return invalid("reindex: embedding dimension must be positive, got %d", dim)
	}
	var existing *string
	if err := db.QueryRow(ctx, `select to_regclass('chunks_new')::text`).Scan(&existing); err != nil {
		return err
	}
	if existing != nil {
		return nil // already prepared; resume into it
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// LIKE copies the columns, NOT NULLs, defaults and the generated tsv column; the
	// dimension is then moved on the still-index-free (empty) column, and the
	// primary key, unique, foreign keys and indexes are added explicitly. Explicit
	// chunks_new_* names avoid colliding with chunks_old's names during the swap.
	//
	// ponytail: this DDL mirrors the chunks table in schemas/tenant.sql / the tenant
	// migration; if that shape changes, update here too. Ceiling: a *repeat* reindex
	// would find these chunks_new_* index/constraint names already on the live
	// (previously swapped) chunks table and collide in CreateChunksNew. Upgrade path:
	// rename them back to the canonical chunks_* names inside DropChunksOld (where
	// chunks_old has freed those names). A single reindex — the story's scope — is
	// unaffected.
	stmts := []string{
		`create table chunks_new (like chunks including defaults including generated)`,
		fmt.Sprintf(`alter table chunks_new alter column embedding type vector(%d)`, dim),
		`alter table chunks_new add constraint chunks_new_pkey primary key (id)`,
		`alter table chunks_new add constraint chunks_new_version_position_key unique (version_id, position)`,
		`alter table chunks_new add constraint chunks_new_document_fk foreign key (document_id) references documents(id) on delete cascade`,
		`alter table chunks_new add constraint chunks_new_version_fk foreign key (version_id) references document_versions(id) on delete cascade`,
		`create index chunks_new_document_idx on chunks_new (document_id)`,
		`create index chunks_new_source_idx on chunks_new (source_id)`,
		`create index chunks_new_tsv_idx on chunks_new using gin (tsv)`,
		`create index chunks_new_embedding_idx on chunks_new using hnsw (embedding vector_cosine_ops) with (m = 16, ef_construction = 64)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("reindex: create chunks_new: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// LiveVersionsAfter returns the current versions of active documents with
// document_id > afterDocID, in document_id order, up to limit (SPEC-05 §7 iterates
// live versions; the cursor is the last document_id processed). afterDocID is empty
// to start from the beginning.
func (TenantStore) LiveVersionsAfter(ctx context.Context, db *tenant.DB, afterDocID string, limit int) ([]ReindexVersion, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := db.Query(ctx, `
		select d.id::text, d.current_version::text, d.source_id::text, d.title, v.content
		from documents d
		join document_versions v on v.id = d.current_version
		where d.status = 'active' and d.current_version is not null
		  and ($1 = '' or d.id > $1::uuid)
		order by d.id
		limit $2`, afterDocID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ReindexVersion
	for rows.Next() {
		var v ReindexVersion
		if err := rows.Scan(&v.DocumentID, &v.VersionID, &v.SourceID, &v.Title, &v.Content); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VersionChunks returns a version's stored chunks (position order) for the reuse
// path — when chunking settings are unchanged the reindex re-embeds the existing
// chunk text rather than re-chunking. Embedding is left empty; the caller fills it
// from the new model.
func (TenantStore) VersionChunks(ctx context.Context, db *tenant.DB, versionID string) ([]ReindexChunk, error) {
	if !validUUID(versionID) {
		return nil, ErrNotFound
	}
	rows, err := db.Query(ctx, `
		select c.document_id::text, c.version_id::text, c.source_id::text,
		       c.position, c.heading_path, c.content, c.token_count
		from chunks c
		where c.version_id = $1::uuid
		order by c.position`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ReindexChunk
	for rows.Next() {
		var c ReindexChunk
		if err := rows.Scan(&c.DocumentID, &c.VersionID, &c.SourceID,
			&c.Position, &c.HeadingPath, &c.Content, &c.TokenCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertChunksNew writes one version's re-embedded chunks into chunks_new. It is
// atomic and idempotent per version: it deletes any existing chunks_new rows for
// versionID first, so a resume after a crash mid-version re-does that version
// cleanly rather than colliding on unique (version_id, position). Each row carries
// its new-model embedding; the vector is written via the pgvector text literal +
// $n::vector, the same codec-free path as Put (ADR-0033).
func (TenantStore) InsertChunksNew(ctx context.Context, db *tenant.DB, versionID string, rows []ReindexChunk) error {
	if !validUUID(versionID) {
		return invalid("reindex: version_id must be a UUID")
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `delete from chunks_new where version_id = $1::uuid`, versionID); err != nil {
		return err
	}
	for _, c := range rows {
		if len(c.Embedding) == 0 {
			return invalid("reindex: chunk position %d has no embedding", c.Position)
		}
		if c.EmbeddingModel == "" {
			return invalid("reindex: chunk position %d has no embedding_model", c.Position)
		}
		hp := c.HeadingPath
		if hp == nil {
			hp = []string{}
		}
		if _, err := tx.Exec(ctx, `
			insert into chunks_new
				(document_id, version_id, source_id, position, heading_path,
				 content, token_count, embedding, embedding_model)
			values ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8::vector, $9)`,
			c.DocumentID, c.VersionID, c.SourceID, c.Position, hp,
			c.Content, c.TokenCount, vectorLiteral(c.Embedding), c.EmbeddingModel); err != nil {
			return err
		}
		// ponytail: one Exec per chunk (matches Put's noted O(n) ceiling); upgrade to
		// a multi-row insert or CopyFrom if a version ever produces thousands of chunks.
	}
	return tx.Commit(ctx)
}

// VerifyCoverage computes the SPEC-05 §7 verification flag for a target table:
// how many live versions exist and how many are represented in it. table is a
// hardcoded constant from the reindexer, checked against reindexTables before it is
// interpolated (a table name cannot be a bind parameter).
func (TenantStore) VerifyCoverage(ctx context.Context, db *tenant.DB, table string) (ReindexCoverage, error) {
	if !reindexTables[table] {
		return ReindexCoverage{}, invalid("reindex: unknown target table %q", table)
	}
	var c ReindexCoverage
	err := db.QueryRow(ctx, fmt.Sprintf(`
		select
		  (select count(*) from documents d
		     where d.status = 'active' and d.current_version is not null),
		  (select count(*) from documents d
		     where d.status = 'active' and d.current_version is not null
		       and exists (select 1 from %[1]s t where t.version_id = d.current_version)),
		  (select count(*) from %[1]s),
		  (select count(*) from live_chunks)`, table)).
		Scan(&c.LiveVersions, &c.CoveredVersions, &c.TargetRows, &c.LiveChunks)
	if err != nil {
		return ReindexCoverage{}, err
	}
	return c, nil
}

// SwapChunks performs the SPEC-03 §5 atomic swap in ONE transaction: drop the
// live_chunks view, rename chunks -> chunks_old and chunks_new -> chunks, and
// recreate live_chunks over the new chunks. Because it is one transaction, no query
// ever sees a half-swapped state — a reader blocks on the brief rename lock and then
// sees the new table. The view is dropped and recreated (rather than left) because a
// view binds to the table by OID: without recreating it, live_chunks would keep
// pointing at the renamed old table.
func (TenantStore) SwapChunks(ctx context.Context, db *tenant.DB) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []string{
		`drop view live_chunks`,
		`alter table chunks rename to chunks_old`,
		`alter table chunks_new rename to chunks`,
		`create view live_chunks as
			select c.*
			from chunks c
			join documents d on d.id = c.document_id
			where d.status = 'active' and d.current_version = c.version_id`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("reindex: swap: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// DropChunksOld drops the retired chunks_old table (SPEC-03 §5 step 4). The
// reindexer only calls it after VerifyCoverage on the now-live chunks table passes,
// so the old table is never dropped before the new one is verified complete.
func (TenantStore) DropChunksOld(ctx context.Context, db *tenant.DB) error {
	if _, err := db.Exec(ctx, `drop table if exists chunks_old`); err != nil {
		return err
	}
	return nil
}
