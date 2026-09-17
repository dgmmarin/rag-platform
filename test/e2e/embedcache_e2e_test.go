//go:build e2e

// Chunk-level drift reuse golden path: the ingestion Sink
// (internal/ingest/sink) wired to a REAL embedcache.NewPgCache() over a REAL
// enrolled tenant database (up via `mise run up`), reached ONLY through a
// resolver + *tenant.DB (ADR-0003, C-3). It proves the SPEC-05 §1 chunk-level
// drift contract: a changed document whose chunks are mostly byte-identical to
// the previous version's embed-text embeds only the CHANGED chunks and reuses
// the rest, and that every committed chunk carries the sha256(embed-text)
// content_hash the cache keys on.
package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/chunk"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/embedcache"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/ingest/sink"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// countingEmbedder is a deterministic, network-free embed.Embedder that stamps
// every vector's components with its call number, so a test can prove WHICH
// call produced a stored embedding: a chunk reused from the cache keeps an
// earlier call's stamp instead of picking up the latest one.
type countingEmbedder struct {
	dim       int
	tokens    int
	calls     int
	lastCount int
}

func (e *countingEmbedder) Embed(_ context.Context, texts []string) (embed.Result, error) {
	e.calls++
	e.lastCount = len(texts)
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		v := make([]float32, e.dim)
		for j := range v {
			v[j] = float32(e.calls)
		}
		vecs[i] = v
	}
	return embed.Result{Vectors: vecs, Tokens: len(texts) * e.tokens}, nil
}

// threeSectionMarkdown builds a 3-heading-section document; chunk.Document
// (default config) splits it into exactly 3 chunks, one per section. bodyTwo
// varies Section Two's content between ingests so exactly one chunk's
// embed-text changes while the other two stay byte-identical.
func threeSectionMarkdown(bodyTwo string) []byte {
	return []byte("# Handbook\n\n" +
		"## Section One\n\nContent one.\n\n" +
		"## Section Two\n\n" + bodyTwo + "\n\n" +
		"## Section Three\n\nContent three.\n")
}

// expectedChunkHashes runs the same parse+chunk pipeline the sink runs and
// returns sha256(EmbedText) per chunk, in position order — the independent
// oracle for the content_hash column.
func expectedChunkHashes(t *testing.T, data []byte) [][]byte {
	t.Helper()
	norm, err := parse.Default().Parse("text/markdown", data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	chunks := chunk.Document(norm, chunk.Config{})
	hashes := make([][]byte, len(chunks))
	for i, c := range chunks {
		sum := sha256.Sum256([]byte(c.EmbedText))
		hashes[i] = sum[:]
	}
	return hashes
}

// liveChunkRow is one live_chunks row's drift-relevant columns.
type liveChunkRow struct {
	contentHash []byte
	embedding   string // vector text form, e.g. "[1,1,...]"
}

// liveChunkRows reads live_chunks for a document, ordered by position — what
// the current version actually committed.
func liveChunkRows(ctx context.Context, t *testing.T, db *tenant.DB, docID string) []liveChunkRow {
	t.Helper()
	rows, err := db.Query(ctx,
		`select content_hash, embedding::text from live_chunks where document_id = $1::uuid order by position`, docID)
	if err != nil {
		t.Fatalf("query live_chunks: %v", err)
	}
	defer rows.Close()
	var out []liveChunkRow
	for rows.Next() {
		var r liveChunkRow
		if err := rows.Scan(&r.contentHash, &r.embedding); err != nil {
			t.Fatalf("scan live_chunks: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("live_chunks rows: %v", err)
	}
	return out
}

// embeddingCallStamp reads the call-number stamp a countingEmbedder wrote into
// every component of a vector text literal ("[N,N,...,N]" -> N).
func embeddingCallStamp(t *testing.T, vecText string) string {
	t.Helper()
	s := strings.TrimSuffix(strings.TrimPrefix(vecText, "["), "]")
	first, _, _ := strings.Cut(s, ",")
	return first
}

func TestEmbedCacheReuseOnReingest(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "embedcache-" + suffix
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
	if out, exit := runEnroll(t, ageKey, blob, slug, "EmbedCache "+suffix, dim); exit != 0 {
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
	cache := embedcache.NewPgCache()
	emb := &countingEmbedder{dim: dim, tokens: 5}
	const model = "text-embedding-3-small"
	const externalID = "handbook.md"
	newSink := func() *sink.Sink {
		return sink.New(sink.Config{
			DB: db, Store: store, Local: parse.Default(), Embedder: emb, Cache: cache,
			SourceID: sourceID, Mode: sink.Incremental, Model: model, Now: time.Now,
		})
	}

	// --- Run 1: a fresh document has no cached hashes, so all 3 chunks embed. ---
	v1Data := threeSectionMarkdown("Content two.")
	s1 := newSink()
	if err := s1.Put(ctx, sink.Document{ExternalID: externalID, MimeType: "text/markdown", Data: v1Data}); err != nil {
		t.Fatalf("run1 Put: %v", err)
	}
	st1 := s1.Stats()
	if st1.ChunksWritten != 3 || st1.ChunksEmbedded != 3 || st1.ChunksReused != 0 {
		t.Fatalf("run1 stats = %+v, want written=3 embedded=3 reused=0", st1)
	}
	if emb.calls != 1 || emb.lastCount != 3 {
		t.Fatalf("run1 embedder calls=%d lastCount=%d, want calls=1 lastCount=3", emb.calls, emb.lastCount)
	}

	var docID string
	if err := db.QueryRow(ctx, `select id::text from documents where source_id = $1::uuid and external_id = $2`,
		sourceID, externalID).Scan(&docID); err != nil {
		t.Fatalf("read document id: %v", err)
	}

	wantHashes1 := expectedChunkHashes(t, v1Data)
	rows1 := liveChunkRows(ctx, t, db, docID)
	if len(rows1) != 3 {
		t.Fatalf("live_chunks after run1 = %d rows, want 3", len(rows1))
	}
	for i, r := range rows1 {
		if string(r.contentHash) != string(wantHashes1[i]) {
			t.Fatalf("run1 chunk %d content_hash mismatch", i)
		}
		if stamp := embeddingCallStamp(t, r.embedding); stamp != "1" {
			t.Fatalf("run1 chunk %d embedding stamp = %q, want \"1\" (call 1)", i, stamp)
		}
	}

	// --- Run 2: only Section Two's text changed, so its hash is the lone miss;
	// Sections One and Three are byte-identical and must be reused, not re-embedded. ---
	v2Data := threeSectionMarkdown("Content two REVISED.")
	s2 := newSink()
	if err := s2.Put(ctx, sink.Document{ExternalID: externalID, MimeType: "text/markdown", Data: v2Data}); err != nil {
		t.Fatalf("run2 Put: %v", err)
	}
	st2 := s2.Stats()
	if st2.ChunksWritten != 3 || st2.ChunksEmbedded != 1 || st2.ChunksReused != 2 {
		t.Fatalf("run2 stats = %+v, want written=3 embedded=1 reused=2", st2)
	}
	if emb.calls != 2 || emb.lastCount != 1 {
		t.Fatalf("run2 embedder calls=%d lastCount=%d, want calls=2 lastCount=1 (only the changed chunk)", emb.calls, emb.lastCount)
	}

	wantHashes2 := expectedChunkHashes(t, v2Data)
	rows2 := liveChunkRows(ctx, t, db, docID)
	if len(rows2) != 3 {
		t.Fatalf("live_chunks after run2 = %d rows, want 3", len(rows2))
	}
	for i, r := range rows2 {
		if string(r.contentHash) != string(wantHashes2[i]) {
			t.Fatalf("run2 chunk %d content_hash mismatch", i)
		}
	}
	// Section Two's hash changed and its stored embedding carries the run2 stamp
	// (freshly embedded); Sections One and Three's hashes are unchanged from run1
	// and their embeddings still carry the run1 stamp (reused verbatim, never
	// re-embedded, proving actual embedding reuse and not just a matching count).
	if string(wantHashes2[1]) == string(wantHashes1[1]) {
		t.Fatalf("fixture bug: Section Two's hash did not change between runs")
	}
	for _, i := range []int{0, 2} {
		if string(wantHashes2[i]) != string(wantHashes1[i]) {
			t.Fatalf("fixture bug: Section %d's hash changed between runs, want unchanged", i)
		}
		if stamp := embeddingCallStamp(t, rows2[i].embedding); stamp != "1" {
			t.Fatalf("run2 chunk %d embedding stamp = %q, want \"1\" (reused from run 1, not re-embedded)", i, stamp)
		}
	}
	if stamp := embeddingCallStamp(t, rows2[1].embedding); stamp != "2" {
		t.Fatalf("run2 chunk 1 embedding stamp = %q, want \"2\" (freshly embedded)", stamp)
	}
}
