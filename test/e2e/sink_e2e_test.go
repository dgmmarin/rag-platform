//go:build e2e

// STORY-05.6 golden path: the ingestion Sink (internal/ingest/sink) driving the
// real embed→store→DB commit path against a REAL enrolled tenant database (up via
// `mise run up`), reached ONLY through a resolver + *tenant.DB (ADR-0003, C-3).
// The embedding PROVIDER is an external service, so it is stubbed with a
// deterministic local Embedder (embed.Embedder is the seam) — what is exercised
// for real is the sink orchestration and the SPEC-05 §5 commit semantics:
//   - a full sync commits each document in its own transaction; current_version is
//     visible in live_chunks the instant it commits,
//   - a mid-transaction failure (a chunk that violates the vector dimension) rolls
//     the whole version back: NO partial document is left — the NFR-REL-02 teeth,
//   - crash-and-retry: a document already committed is skipped by hash on the
//     retry (unchanged), never duplicated (SPEC-05 §8),
//   - Sink.Complete on a FULL sync soft-deletes documents not re-seen this run,
//     while an incremental Complete deletes nothing.
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
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/ingest/sink"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// stubEmbedder is a deterministic, network-free embed.Embedder: it returns one
// dim-length vector per text plus a fixed per-text token count, so the e2e drives
// the real store/DB path without reaching an external provider.
type stubEmbedder struct {
	dim           int
	tokensPerText int
}

func (e stubEmbedder) Embed(_ context.Context, texts []string) (embed.Result, error) {
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		v := make([]float32, e.dim)
		for j := range v {
			v[j] = float32(i+1) * 1e-3
		}
		vecs[i] = v
	}
	return embed.Result{Vectors: vecs, Tokens: len(texts) * e.tokensPerText}, nil
}

func mdBytes(title, body string) []byte {
	return []byte("# " + title + "\n\n" + body + "\n")
}

func TestSinkFullSyncCommitSemantics(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "sink-" + suffix
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
	const dim = 768
	if out, exit := runEnroll(t, ageKey, blob, slug, "Sink "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	// Read the tenant id over the direct control pool (not `docker compose exec`,
	// which is pathologically slow in some environments).
	var tenantID string
	if err := pool.QueryRow(ctx, `select id::text from tenants where slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}

	var sourceID string
	if err := pool.QueryRow(ctx,
		`insert into sources (tenant_id, kind, name, status) values ($1, 'upload', 'uploads', 'active') returning id::text`,
		tenantID).Scan(&sourceID); err != nil {
		t.Fatalf("seed upload source: %v", err)
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
	newSink := func(mode sink.Mode, emb embed.Embedder) *sink.Sink {
		return sink.New(sink.Config{
			DB: db, Store: store, Local: parse.Default(), Embedder: emb,
			SourceID: sourceID, Mode: mode, Model: "text-embedding-3-small", Now: time.Now,
		})
	}

	// --- Run 1: a FULL sync commits A, B, C. ---
	s1 := newSink(sink.Full, stubEmbedder{dim: dim, tokensPerText: 7})
	docs := []sink.Document{
		{ExternalID: "a.md", MimeType: "text/markdown", Data: mdBytes("Alpha", "Alpha body one.")},
		{ExternalID: "b.md", MimeType: "text/markdown", Data: mdBytes("Bravo", "Bravo body one.")},
		{ExternalID: "c.md", MimeType: "text/markdown", Data: mdBytes("Charlie", "Charlie body one.")},
	}
	for _, d := range docs {
		if err := s1.Put(ctx, d); err != nil {
			t.Fatalf("run1 Put %s: %v", d.ExternalID, err)
		}
	}
	if err := s1.Complete(ctx); err != nil {
		t.Fatalf("run1 Complete: %v", err)
	}
	st1 := s1.Stats()
	if st1.DocsSeen != 3 || st1.DocsChanged != 3 || st1.DocsUnchanged != 0 || st1.DocsDeleted != 0 || st1.DocsFailed != 0 {
		t.Fatalf("run1 stats = %+v, want 3 seen/changed", st1)
	}
	if st1.ChunksWritten < 3 || st1.EmbedTokens == 0 {
		t.Fatalf("run1 stats chunks/tokens = %+v", st1)
	}
	// Each committed document is whole and visible in live_chunks the instant it
	// committed (current_version flipped in the same tx).
	if got := tenantScalarDB(ctx, t, db, `select count(*) from documents where source_id = $1::uuid and status = 'active'`, sourceID); got != "3" {
		t.Fatalf("active documents after run1 = %s, want 3", got)
	}
	for _, ext := range []string{"a.md", "b.md", "c.md"} {
		if got := tenantScalarDB(ctx, t, db,
			`select count(*) from live_chunks lc join documents d on d.id = lc.document_id where d.external_id = $1`, ext); got == "0" {
			t.Fatalf("%s has no live_chunks after run1", ext)
		}
	}

	// --- No partial document on a mid-transaction failure (crash safety teeth). ---
	// A wrong-dimension embedder makes the chunk INSERT fail after the version row
	// was already inserted in the same tx; the store must roll the whole thing back,
	// leaving no document, version or chunk for d.md.
	sBad := newSink(sink.Incremental, stubEmbedder{dim: 5, tokensPerText: 1}) // 5 != 768
	badDoc := sink.Document{ExternalID: "d.md", MimeType: "text/markdown", Data: mdBytes("Delta", "Delta body.")}
	if err := sBad.Put(ctx, badDoc); err == nil {
		t.Fatalf("run-bad Put d.md: expected a store error from the dimension mismatch, got nil")
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from documents where source_id = $1::uuid and external_id = 'd.md'`, sourceID); got != "0" {
		t.Fatalf("partial document left after failed commit: d.md rows = %s, want 0", got)
	}
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from document_versions v join documents d on d.id = v.document_id where d.external_id = 'd.md'`); got != "0" {
		t.Fatalf("orphan version left after failed commit (count=%s)", got)
	}

	// --- Crash-and-retry: a committed doc is skipped by hash, never duplicated. ---
	// Simulate a crash after A committed by starting a fresh full run that re-sees A
	// unchanged, changes B, and does NOT see C.
	time.Sleep(15 * time.Millisecond) // cross-clock margin: DB now() must exceed the Go run start
	s2 := newSink(sink.Full, stubEmbedder{dim: dim, tokensPerText: 7})
	// A: identical content -> unchanged, no new version, no re-embed.
	if err := s2.Put(ctx, docs[0]); err != nil {
		t.Fatalf("run2 Put a.md: %v", err)
	}
	// B: changed content -> new version, live_chunks reflects it.
	bChanged := sink.Document{ExternalID: "b.md", MimeType: "text/markdown", Data: mdBytes("Bravo", "Bravo body TWO, revised.")}
	if err := s2.Put(ctx, bChanged); err != nil {
		t.Fatalf("run2 Put b.md: %v", err)
	}
	// C is not seen this run.
	if err := s2.Complete(ctx); err != nil {
		t.Fatalf("run2 Complete: %v", err)
	}
	st2 := s2.Stats()
	if st2.DocsUnchanged != 1 || st2.DocsChanged != 1 || st2.DocsDeleted != 1 {
		t.Fatalf("run2 stats = %+v, want unchanged=1 changed=1 deleted=1", st2)
	}
	// A was not re-versioned (skipped by hash).
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from document_versions v join documents d on d.id = v.document_id where d.external_id = 'a.md'`); got != "1" {
		t.Fatalf("a.md version count after run2 = %s, want 1 (unchanged skips by hash)", got)
	}
	// B has a second version and live_chunks shows the revised content.
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from document_versions v join documents d on d.id = v.document_id where d.external_id = 'b.md'`); got != "2" {
		t.Fatalf("b.md version count after run2 = %s, want 2 (changed)", got)
	}
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from live_chunks lc join documents d on d.id = lc.document_id where d.external_id = 'b.md' and lc.content ilike '%revised%'`); got == "0" {
		t.Fatalf("b.md live_chunks did not reflect the revised content")
	}
	// C was soft-deleted by the full-sync Complete and is gone from live_chunks.
	if got := tenantScalarDB(ctx, t, db, `select status::text from documents where source_id = $1::uuid and external_id = 'c.md'`, sourceID); got != "deleted" {
		t.Fatalf("c.md status after run2 = %s, want deleted", got)
	}
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from live_chunks lc join documents d on d.id = lc.document_id where d.external_id = 'c.md'`); got != "0" {
		t.Fatalf("c.md still visible in live_chunks after soft delete (count=%s)", got)
	}
	// A and B remain active.
	if got := tenantScalarDB(ctx, t, db, `select count(*) from documents where source_id = $1::uuid and status = 'active'`, sourceID); got != "2" {
		t.Fatalf("active documents after run2 = %s, want 2 (a,b)", got)
	}

	// --- Incremental Complete never deletes: re-seeing only A leaves B active. ---
	time.Sleep(15 * time.Millisecond)
	s3 := newSink(sink.Incremental, stubEmbedder{dim: dim, tokensPerText: 7})
	if err := s3.Put(ctx, docs[0]); err != nil { // only A
		t.Fatalf("run3 Put a.md: %v", err)
	}
	if err := s3.Complete(ctx); err != nil {
		t.Fatalf("run3 Complete: %v", err)
	}
	if s3.Stats().DocsDeleted != 0 {
		t.Fatalf("run3 (incremental) DocsDeleted = %d, want 0", s3.Stats().DocsDeleted)
	}
	if got := tenantScalarDB(ctx, t, db, `select status::text from documents where source_id = $1::uuid and external_id = 'b.md'`, sourceID); got != "active" {
		t.Fatalf("b.md status after incremental run = %s, want active (incremental never soft-deletes)", got)
	}
}
