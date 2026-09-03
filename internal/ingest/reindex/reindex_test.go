package reindex

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// fakeStore is an in-memory Store: it holds a set of live versions (each with its
// stored chunks) and records what the reindexer writes into "chunks_new". It never
// touches a real database — the *tenant.DB is a nil pass-through (unforgeable by
// design, ADR-0003), ignored here.
type fakeStore struct {
	versions   []documents.ReindexVersion
	verChunks  map[string][]documents.ReindexChunk // versionID -> stored chunks
	newTable   map[string][]documents.ReindexChunk // versionID -> chunks_new rows
	prepared   int
	inserts    int
	swapped    bool
	dropped    bool
	swapCov    documents.ReindexCoverage // returned by VerifyCoverage("chunks_new")
	liveCov    documents.ReindexCoverage // returned by VerifyCoverage("chunks")
	prepareErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{verChunks: map[string][]documents.ReindexChunk{}, newTable: map[string][]documents.ReindexChunk{}}
}

func (f *fakeStore) CreateChunksNew(_ context.Context, _ *tenant.DB, _ int) error {
	f.prepared++
	return f.prepareErr
}

func (f *fakeStore) LiveVersionsAfter(_ context.Context, _ *tenant.DB, after string, limit int) ([]documents.ReindexVersion, error) {
	var out []documents.ReindexVersion
	for _, v := range f.versions {
		if after == "" || v.DocumentID > after {
			out = append(out, v)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeStore) VersionChunks(_ context.Context, _ *tenant.DB, versionID string) ([]documents.ReindexChunk, error) {
	return f.verChunks[versionID], nil
}

func (f *fakeStore) InsertChunksNew(_ context.Context, _ *tenant.DB, versionID string, rows []documents.ReindexChunk) error {
	f.inserts++
	// mimic the idempotent per-version replace
	cp := make([]documents.ReindexChunk, len(rows))
	copy(cp, rows)
	f.newTable[versionID] = cp
	return nil
}

func (f *fakeStore) VerifyCoverage(_ context.Context, _ *tenant.DB, table string) (documents.ReindexCoverage, error) {
	if table == "chunks" {
		return f.liveCov, nil
	}
	return f.swapCov, nil
}

func (f *fakeStore) SwapChunks(_ context.Context, _ *tenant.DB) error { f.swapped = true; return nil }
func (f *fakeStore) DropChunksOld(_ context.Context, _ *tenant.DB) error {
	f.dropped = true
	return nil
}

// fakeEmbedder returns one dim-length vector per input text plus a fixed token
// count, and records the exact texts it was asked to embed (so a test can assert
// the reconstructed context line).
type fakeEmbedder struct {
	dim   int
	seen  []string
	fail  error
	calls int
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) (embed.Result, error) {
	e.calls++
	if e.fail != nil {
		return embed.Result{}, e.fail
	}
	e.seen = append(e.seen, texts...)
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		v := make([]float32, e.dim)
		vecs[i] = v
	}
	return embed.Result{Vectors: vecs, Tokens: len(texts) * 3}, nil
}

func title(s string) *string { return &s }

func reuseVersion(docID, verID, src, ttl string, chunks ...documents.ReindexChunk) (documents.ReindexVersion, []documents.ReindexChunk) {
	for i := range chunks {
		chunks[i].DocumentID = docID
		chunks[i].VersionID = verID
		chunks[i].SourceID = src
		chunks[i].Position = i
	}
	return documents.ReindexVersion{DocumentID: docID, VersionID: verID, SourceID: src, Title: title(ttl)}, chunks
}

func seed(f *fakeStore, v documents.ReindexVersion, chunks []documents.ReindexChunk) {
	f.versions = append(f.versions, v)
	f.verChunks[v.VersionID] = chunks
}

const src = "11111111-1111-1111-1111-111111111111"

// TestReindexReuseRoundTrip is the headline hermetic test: Prepare, Run to
// completion re-embedding every live version's existing chunks with the new model,
// and Verify/Swap/DropOld gated on coverage.
func TestReindexReuseRoundTrip(t *testing.T) {
	f := newFakeStore()
	// Two documents (doc ids ordered a < b), one/two chunks each.
	va, ca := reuseVersion("a", "va", src, "Alpha",
		documents.ReindexChunk{HeadingPath: []string{"Intro"}, Content: "alpha one", TokenCount: 2})
	vb, cb := reuseVersion("b", "vb", src, "Bravo",
		documents.ReindexChunk{HeadingPath: []string{"Setup"}, Content: "bravo one", TokenCount: 2},
		documents.ReindexChunk{HeadingPath: []string{"Setup"}, Content: "bravo two", TokenCount: 2})
	seed(f, va, ca)
	seed(f, vb, cb)
	f.swapCov = documents.ReindexCoverage{LiveVersions: 2, CoveredVersions: 2}
	f.liveCov = documents.ReindexCoverage{LiveVersions: 2, CoveredVersions: 2}

	emb := &fakeEmbedder{dim: 256}
	r := New(Config{Store: f, Embedder: emb, NewModel: "new-model", NewDim: 256})

	if err := r.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if f.prepared != 1 {
		t.Fatalf("CreateChunksNew called %d times, want 1", f.prepared)
	}

	prog, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !prog.Done || prog.DocsIndexed != 2 || prog.ChunksWritten != 3 {
		t.Fatalf("progress = %+v, want Done, 2 docs, 3 chunks", prog)
	}
	if prog.EmbedTokens != 3*3 { // 3 chunks * 3 tokens each
		t.Fatalf("EmbedTokens = %d, want 9", prog.EmbedTokens)
	}
	// Every new row carries the new model and a dim-length vector.
	for ver, rows := range f.newTable {
		for _, rw := range rows {
			if rw.EmbeddingModel != "new-model" {
				t.Fatalf("version %s chunk model = %q, want new-model", ver, rw.EmbeddingModel)
			}
			if len(rw.Embedding) != 256 {
				t.Fatalf("version %s chunk vector len = %d, want 256", ver, len(rw.Embedding))
			}
		}
	}
	// Embed text is the reconstructed context line "{title} > {heading}\n{content}".
	joined := strings.Join(emb.seen, "\n---\n")
	if !strings.Contains(joined, "Alpha > Intro\nalpha one") {
		t.Fatalf("reuse embed text missing reconstructed context line; got:\n%s", joined)
	}

	// Verify + Swap + DropOld, all gated on coverage.
	if err := r.Swap(context.Background()); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if !f.swapped {
		t.Fatal("SwapChunks not called")
	}
	if err := r.DropOld(context.Background()); err != nil {
		t.Fatalf("DropOld: %v", err)
	}
	if !f.dropped {
		t.Fatal("DropChunksOld not called")
	}
}

// TestReindexResumesFromCursor proves Step resumes rather than restarts: a first
// Step with batch=1 indexes only doc "a", and a second Step from the returned
// cursor indexes only "b" — "a" is not re-embedded.
func TestReindexResumesFromCursor(t *testing.T) {
	f := newFakeStore()
	va, ca := reuseVersion("a", "va", src, "Alpha", documents.ReindexChunk{Content: "a", TokenCount: 1})
	vb, cb := reuseVersion("b", "vb", src, "Bravo", documents.ReindexChunk{Content: "b", TokenCount: 1})
	seed(f, va, ca)
	seed(f, vb, cb)
	emb := &fakeEmbedder{dim: 8}
	r := New(Config{Store: f, Embedder: emb, NewModel: "m", NewDim: 8, BatchDocs: 1})

	p1, err := r.Step(context.Background(), "")
	if err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	if p1.Done || p1.Cursor != "a" || p1.DocsIndexed != 1 {
		t.Fatalf("Step 1 progress = %+v, want cursor=a, 1 doc, not done", p1)
	}

	p2, err := r.Step(context.Background(), p1.Cursor)
	if err != nil {
		t.Fatalf("Step 2: %v", err)
	}
	if p2.Cursor != "b" || p2.DocsIndexed != 1 {
		t.Fatalf("Step 2 progress = %+v, want cursor=b, 1 doc", p2)
	}
	// A third Step from "b" finds nothing left and reports Done — no re-embed of a/b.
	p3, err := r.Step(context.Background(), p2.Cursor)
	if err != nil {
		t.Fatalf("Step 3: %v", err)
	}
	if !p3.Done || p3.DocsIndexed != 0 {
		t.Fatalf("Step 3 progress = %+v, want done, 0 docs", p3)
	}
	// Exactly two inserts happened (one per version), never re-doing "a".
	if f.inserts != 2 {
		t.Fatalf("InsertChunksNew calls = %d, want 2 (no restart)", f.inserts)
	}
}

// TestSwapRefusesWhenNotCovered proves the verify-before-swap gate: an incomplete
// chunks_new blocks the swap with ErrNotVerified and never calls SwapChunks.
func TestSwapRefusesWhenNotCovered(t *testing.T) {
	f := newFakeStore()
	f.swapCov = documents.ReindexCoverage{LiveVersions: 3, CoveredVersions: 2} // one missing
	r := New(Config{Store: f, Embedder: &fakeEmbedder{dim: 4}, NewModel: "m", NewDim: 4})

	err := r.Swap(context.Background())
	if !errors.Is(err, ErrNotVerified) {
		t.Fatalf("Swap error = %v, want ErrNotVerified", err)
	}
	if f.swapped {
		t.Fatal("SwapChunks called despite incomplete coverage")
	}
}

// TestDropRefusesWhenNotCovered proves the verify-before-drop gate on the post-swap
// live table.
func TestDropRefusesWhenNotCovered(t *testing.T) {
	f := newFakeStore()
	f.liveCov = documents.ReindexCoverage{LiveVersions: 2, CoveredVersions: 1}
	r := New(Config{Store: f, Embedder: &fakeEmbedder{dim: 4}, NewModel: "m", NewDim: 4})

	err := r.DropOld(context.Background())
	if !errors.Is(err, ErrNotVerified) {
		t.Fatalf("DropOld error = %v, want ErrNotVerified", err)
	}
	if f.dropped {
		t.Fatal("DropChunksOld called despite incomplete coverage")
	}
}

// fakeParser is a LocalParser that returns a fixed Normalised for the re-chunk path.
type fakeParser struct{ n parse.Normalised }

func (p fakeParser) Parse(_ string, _ []byte) (parse.Normalised, error) { return p.n, nil }

// TestReindexRechunkPath proves the re-chunk branch re-splits the stored content
// with the chunker rather than reusing stored chunks.
func TestReindexRechunkPath(t *testing.T) {
	f := newFakeStore()
	// A version whose stored content should be re-chunked; VersionChunks is NOT used.
	v := documents.ReindexVersion{DocumentID: "a", VersionID: "va", SourceID: src, Title: title("Doc"), Content: "# Doc\n\npara one\n"}
	f.versions = []documents.ReindexVersion{v}
	// If reuse were taken, VersionChunks would return this sentinel — assert it is NOT written.
	f.verChunks["va"] = []documents.ReindexChunk{{Content: "STORED-SHOULD-NOT-APPEAR", TokenCount: 1}}

	norm := parse.Normalised{Title: "Doc", Blocks: []parse.Block{{Type: parse.Paragraph, Text: "para one"}}}
	emb := &fakeEmbedder{dim: 16}
	r := New(Config{
		Store: f, Embedder: emb, NewModel: "m", NewDim: 16, Rechunk: true,
		Local: fakeParser{n: norm},
	})

	prog, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prog.ChunksWritten == 0 {
		t.Fatal("re-chunk wrote no chunks")
	}
	for _, rows := range f.newTable {
		for _, rw := range rows {
			if strings.Contains(rw.Content, "STORED-SHOULD-NOT-APPEAR") {
				t.Fatal("re-chunk path reused stored chunks instead of re-splitting content")
			}
			if rw.Content != "para one" {
				t.Fatalf("re-chunk content = %q, want the re-split paragraph", rw.Content)
			}
		}
	}
}

// TestPrepareValidates guards the dimension/model preconditions.
func TestPrepareValidates(t *testing.T) {
	f := newFakeStore()
	if err := (New(Config{Store: f, NewModel: "m", NewDim: 0})).Prepare(context.Background()); err == nil {
		t.Fatal("Prepare accepted a non-positive dimension")
	}
	if err := (New(Config{Store: f, NewModel: "", NewDim: 8})).Prepare(context.Background()); err == nil {
		t.Fatal("Prepare accepted an empty model")
	}
}
