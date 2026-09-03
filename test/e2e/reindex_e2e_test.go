//go:build e2e

// STORY-05.8 golden path: the resumable reindex operation (internal/ingest/reindex)
// migrating a tenant's live chunks to a NEW embedding model AND dimension while
// retrieval keeps serving from the current chunks table — against a REAL enrolled
// tenant database, reached ONLY through a resolver + *tenant.DB (ADR-0003, C-3).
// The embedding PROVIDER is an external service, so it is stubbed with the same
// deterministic local Embedder the sink e2e uses (embed.Embedder is the seam); what
// is exercised for real is the SPEC-05 §7 / SPEC-03 §5 table-swap mechanics:
//   - a side table chunks_new is built at the new dimension; queries keep reading
//     the OLD chunks table (live_chunks unchanged, still the old dimension) while it
//     is populated,
//   - the operation is resumable: a partial Step advances a document_id cursor and a
//     resume from that cursor continues (indexes only the remaining docs) rather than
//     restarting,
//   - the swap is refused until chunks_new covers every live version (verify-before-
//     swap), then performed atomically so live_chunks flips to the new dimension in
//     one transaction,
//   - chunks_old is dropped only after the now-live table is verified complete.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/ingest/reindex"
	"github.com/rag-platform/ragctl/internal/ingest/sink"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// embeddingType reads the SQL type (e.g. "vector(768)") of a table's embedding
// column — the physical dimension the reindex moves.
func embeddingType(ctx context.Context, t *testing.T, db *tenant.DB, table string) string {
	t.Helper()
	return tenantScalarDB(ctx, t, db, `
		select format_type(atttypid, atttypmod)
		from pg_attribute
		where attrelid = $1::regclass and attname = 'embedding' and not attisdropped`, table)
}

func TestReindexTableSwap(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "reindex-" + suffix
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		dbName := tryScalar(slug, "d.database_name")
		role := tryScalar(slug, "d.username")
		if dbName != "" {
			_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		}
		if role != "" {
			_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
		}
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE slug = '%s'", slug))
	})

	const oldDim = 768
	const newDim = 384
	if out, exit := runEnroll(t, ageKey, blob, slug, "Reindex "+suffix, oldDim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `select id::text from tenants where slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}
	var sourceID string
	if err := pool.QueryRow(ctx,
		`insert into sources (tenant_id, kind, name, status) values ($1, 'upload', 'uploads', 'active') returning id::text`,
		tenantID).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher, CacheTTL: 50 * time.Millisecond})
	db, err := resolver.Open(ctx, tenant.ID(uuid.MustParse(tenantID)))
	if err != nil {
		t.Fatalf("resolver.Open: %v", err)
	}
	store := documents.NewTenantStore()

	// --- Seed: ingest three documents at the OLD dimension via the sink. ---
	s := sink.New(sink.Config{
		DB: db, Store: store, Local: parse.Default(), Embedder: stubEmbedder{dim: oldDim, tokensPerText: 5},
		SourceID: sourceID, Mode: sink.Full, Model: "old-model", Now: time.Now,
	})
	docs := []sink.Document{
		{ExternalID: "a.md", MimeType: "text/markdown", Data: mdBytes("Alpha", "Alpha body one.\n\nAlpha body two.")},
		{ExternalID: "b.md", MimeType: "text/markdown", Data: mdBytes("Bravo", "Bravo body one.")},
		{ExternalID: "c.md", MimeType: "text/markdown", Data: mdBytes("Charlie", "Charlie body one.")},
	}
	for _, d := range docs {
		if err := s.Put(ctx, d); err != nil {
			t.Fatalf("seed Put %s: %v", d.ExternalID, err)
		}
	}
	liveBefore := tenantScalarDB(ctx, t, db, `select count(*) from live_chunks`)
	if liveBefore == "0" {
		t.Fatal("no live chunks after seeding")
	}
	if got := embeddingType(ctx, t, db, "chunks"); got != fmt.Sprintf("vector(%d)", oldDim) {
		t.Fatalf("seeded chunks embedding type = %s, want vector(%d)", got, oldDim)
	}

	// --- Reindex to a NEW model + dimension. ---
	r := reindex.New(reindex.Config{
		DB: db, Store: store, Embedder: stubEmbedder{dim: newDim, tokensPerText: 4},
		NewModel: "new-model", NewDim: newDim, BatchDocs: 1, // batch=1 so we can checkpoint mid-run
	})
	if err := r.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// chunks_new exists at the NEW dimension; queries still serve from the OLD chunks.
	if got := embeddingType(ctx, t, db, "chunks_new"); got != fmt.Sprintf("vector(%d)", newDim) {
		t.Fatalf("chunks_new embedding type = %s, want vector(%d)", got, newDim)
	}
	if got := embeddingType(ctx, t, db, "chunks"); got != fmt.Sprintf("vector(%d)", oldDim) {
		t.Fatalf("live chunks embedding type changed during build = %s, want vector(%d)", got, oldDim)
	}

	// One Step (batch=1) indexes only the first document; live_chunks is untouched.
	p1, err := r.Step(ctx, "")
	if err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	if p1.Done || p1.DocsIndexed != 1 || p1.Cursor == "" {
		t.Fatalf("Step 1 progress = %+v, want 1 doc, not done, cursor set", p1)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from live_chunks`); got != liveBefore {
		t.Fatalf("live_chunks changed during partial build = %s, want %s (queries must keep serving the old table)", got, liveBefore)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(distinct version_id) from chunks_new`); got != "1" {
		t.Fatalf("chunks_new has %s versions after Step 1, want 1", got)
	}

	// Verify-before-swap: with only 1 of 3 live versions covered, Swap must refuse
	// (the real coverage query says so) and change nothing.
	if err := r.Swap(ctx); err == nil {
		t.Fatal("Swap succeeded on an incomplete chunks_new; expected a verify refusal")
	}
	if got := embeddingType(ctx, t, db, "chunks"); got != fmt.Sprintf("vector(%d)", oldDim) {
		t.Fatalf("chunks swapped despite incomplete coverage (type=%s)", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select (to_regclass('chunks_old') is not null)::text`); got != "false" {
		t.Fatal("chunks_old exists after a refused swap")
	}

	// Resume from the checkpointed cursor: it continues with the remaining docs only.
	p2, err := r.Run(ctx, p1.Cursor)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if !p2.Done || p2.DocsIndexed != 2 {
		t.Fatalf("resume progress = %+v, want done and exactly 2 docs (resume, not restart)", p2)
	}

	// Verify: chunks_new now covers every live version, with the same row count as the
	// old live chunks (the reuse path preserves chunk boundaries).
	cov, err := r.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !cov.Covered() || cov.LiveVersions != 3 {
		t.Fatalf("coverage = %+v, want 3/3 covered", cov)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from chunks_new`); got != liveBefore {
		t.Fatalf("chunks_new row count = %s, want %s (same chunks re-embedded)", got, liveBefore)
	}

	// --- Atomic swap: live_chunks flips to the new dimension in one transaction. ---
	if err := r.Swap(ctx); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := embeddingType(ctx, t, db, "chunks"); got != fmt.Sprintf("vector(%d)", newDim) {
		t.Fatalf("after swap chunks embedding type = %s, want vector(%d)", got, newDim)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from live_chunks`); got != liveBefore {
		t.Fatalf("live_chunks count after swap = %s, want %s (content preserved)", got, liveBefore)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(distinct embedding_model) from live_chunks where embedding_model <> 'new-model'`); got != "0" {
		t.Fatal("live_chunks still carry the old embedding model after swap")
	}
	// The retired table is present at the old dimension, awaiting a verified drop.
	if got := embeddingType(ctx, t, db, "chunks_old"); got != fmt.Sprintf("vector(%d)", oldDim) {
		t.Fatalf("chunks_old embedding type = %s, want vector(%d)", got, oldDim)
	}

	// --- Verify-then-drop: chunks_old is dropped only after the live table verifies. ---
	if err := r.DropOld(ctx); err != nil {
		t.Fatalf("DropOld: %v", err)
	}
	if got := tenantScalarDB(ctx, t, db, `select (to_regclass('chunks_old') is null)::text`); got != "true" {
		t.Fatal("chunks_old still exists after DropOld")
	}
	// A retrieval query still works against the swapped, new-dimension table.
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from live_chunks lc join documents d on d.id = lc.document_id where d.external_id = 'a.md'`); got == "0" {
		t.Fatal("a.md has no live_chunks after the reindex swap")
	}
}
