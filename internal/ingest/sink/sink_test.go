package sink

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/ingest/sidecar"
	"github.com/rag-platform/ragctl/internal/tenant"
)

const testSourceID = "11111111-1111-1111-1111-111111111111"

// --- fakes ---------------------------------------------------------------

// fakeStore records the sink's store calls. It ignores the *tenant.DB (nil in
// unit tests): the handle is an opaque pass-through here (a real *tenant.DB is
// unforgeable by design, exercised in the e2e suite).
type fakeStore struct {
	unchanged   bool
	putErr      error
	deleteCount int
	deleteErr   error

	puts         []documents.PutInput
	touchCalls   int
	deleteCalls  int
	deleteSince  time.Time
	deleteSource string
}

func (f *fakeStore) TouchIfUnchanged(_ context.Context, _ *tenant.DB, _, _ string, _ []byte) (bool, error) {
	f.touchCalls++
	return f.unchanged, nil
}

func (f *fakeStore) Put(_ context.Context, _ *tenant.DB, in documents.PutInput) (documents.PutResult, error) {
	f.puts = append(f.puts, in)
	if f.putErr != nil {
		return documents.PutResult{}, f.putErr
	}
	return documents.PutResult{DocumentID: "doc", VersionID: "ver", Changed: true}, nil
}

func (f *fakeStore) SoftDeleteUnseen(_ context.Context, _ *tenant.DB, sourceID string, startedAt time.Time) (int, error) {
	f.deleteCalls++
	f.deleteSource = sourceID
	f.deleteSince = startedAt
	return f.deleteCount, f.deleteErr
}

// fakeEmbedder returns a dim-length vector per text and a fixed token count, or a
// preset error. It counts calls so a test can prove embed was skipped.
type fakeEmbedder struct {
	dim    int
	tokens int
	err    error
	calls  int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) (embed.Result, error) {
	f.calls++
	if f.err != nil {
		return embed.Result{}, f.err
	}
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		vecs[i] = make([]float32, f.dim)
	}
	return embed.Result{Vectors: vecs, Tokens: f.tokens}, nil
}

// fakeSidecar stands in for the Python sidecar parse client.
type fakeSidecar struct {
	norm  parse.Normalised
	err   error
	calls int
}

func (f *fakeSidecar) Parse(_ context.Context, _, _ string, _ []byte) (parse.Normalised, error) {
	f.calls++
	return f.norm, f.err
}

// baseConfig wires a sink with the real Go parser registry and injected fakes.
func baseConfig(store *fakeStore, emb *fakeEmbedder, sc *fakeSidecar) Config {
	return Config{
		Store:    store,
		Local:    parse.Default(),
		Sidecar:  sc,
		Embedder: emb,
		SourceID: testSourceID,
		Model:    "text-embedding-3-small",
		Now:      time.Now,
	}
}

func mdDoc() Document {
	return Document{
		ExternalID: "handbook.md",
		Filename:   "handbook.md",
		MimeType:   "text/markdown",
		Data:       []byte("# Handbook\n\nHello world, this is the body.\n"),
	}
}

// --- tests ---------------------------------------------------------------

func TestPutChangedDocumentEmbedsAndStores(t *testing.T) {
	store := &fakeStore{unchanged: false}
	emb := &fakeEmbedder{dim: 4, tokens: 17}
	s := New(baseConfig(store, emb, &fakeSidecar{}))

	if err := s.Put(context.Background(), mdDoc()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if store.touchCalls != 1 {
		t.Fatalf("TouchIfUnchanged calls = %d, want 1", store.touchCalls)
	}
	if emb.calls != 1 {
		t.Fatalf("embed calls = %d, want 1", emb.calls)
	}
	if len(store.puts) != 1 {
		t.Fatalf("Put store calls = %d, want 1", len(store.puts))
	}
	in := store.puts[0]
	if in.ExternalID != "handbook.md" || in.SourceID != testSourceID {
		t.Fatalf("PutInput identity = %+v", in)
	}
	if len(in.ContentHash) == 0 || in.Content == "" {
		t.Fatalf("PutInput missing content/hash: %+v", in)
	}
	if len(in.Chunks) == 0 {
		t.Fatalf("PutInput has no chunks")
	}
	for i, c := range in.Chunks {
		if c.EmbeddingModel != "text-embedding-3-small" {
			t.Fatalf("chunk %d model = %q, want configured model", i, c.EmbeddingModel)
		}
		if len(c.Embedding) != 4 {
			t.Fatalf("chunk %d embedding dim = %d, want 4", i, len(c.Embedding))
		}
	}
	st := s.Stats()
	if st.DocsSeen != 1 || st.DocsChanged != 1 || st.DocsUnchanged != 0 || st.DocsFailed != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if st.ChunksWritten != len(in.Chunks) {
		t.Fatalf("ChunksWritten = %d, want %d", st.ChunksWritten, len(in.Chunks))
	}
	if st.EmbedTokens != 17 {
		t.Fatalf("EmbedTokens = %d, want 17", st.EmbedTokens)
	}
}

func TestPutUnchangedShortCircuitsEmbed(t *testing.T) {
	store := &fakeStore{unchanged: true}
	emb := &fakeEmbedder{dim: 4, tokens: 5}
	s := New(baseConfig(store, emb, &fakeSidecar{}))

	if err := s.Put(context.Background(), mdDoc()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if emb.calls != 0 {
		t.Fatalf("embed calls = %d, want 0 (unchanged doc must cost no embedding)", emb.calls)
	}
	if len(store.puts) != 0 {
		t.Fatalf("Put store calls = %d, want 0 (unchanged)", len(store.puts))
	}
	st := s.Stats()
	if st.DocsUnchanged != 1 || st.DocsChanged != 0 {
		t.Fatalf("stats = %+v, want DocsUnchanged=1", st)
	}
}

func TestPutRoutesUnsupportedMIMEToSidecar(t *testing.T) {
	store := &fakeStore{}
	emb := &fakeEmbedder{dim: 4, tokens: 3}
	sc := &fakeSidecar{norm: parse.Normalised{
		Title:  "Report",
		Blocks: []parse.Block{{Type: parse.Paragraph, Text: "Body from the sidecar."}},
	}}
	s := New(baseConfig(store, emb, sc))

	doc := Document{ExternalID: "report.pdf", Filename: "report.pdf", MimeType: "application/pdf", Data: []byte("%PDF-1.7")}
	if err := s.Put(context.Background(), doc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if sc.calls != 1 {
		t.Fatalf("sidecar calls = %d, want 1 (pdf routed to sidecar)", sc.calls)
	}
	if len(store.puts) != 1 {
		t.Fatalf("Put store calls = %d, want 1", len(store.puts))
	}
	if s.Stats().DocsChanged != 1 {
		t.Fatalf("DocsChanged = %d, want 1", s.Stats().DocsChanged)
	}
}

func TestPutParseFailureRecordedAndContinues(t *testing.T) {
	store := &fakeStore{}
	emb := &fakeEmbedder{dim: 4}
	sc := &fakeSidecar{err: sidecar.ErrParseFailed}
	s := New(baseConfig(store, emb, sc))

	doc := Document{ExternalID: "broken.pdf", MimeType: "application/pdf", Data: []byte("garbage")}
	if err := s.Put(context.Background(), doc); err != nil {
		t.Fatalf("Put returned %v, want nil (single-doc parse error is recorded, sync continues)", err)
	}
	if emb.calls != 0 {
		t.Fatalf("embed calls = %d, want 0 (parse failed)", emb.calls)
	}
	if len(store.puts) != 0 {
		t.Fatalf("Put store calls = %d, want 0", len(store.puts))
	}
	st := s.Stats()
	if st.DocsFailed != 1 || len(st.Errors) != 1 {
		t.Fatalf("stats = %+v, want DocsFailed=1 and one error", st)
	}
	if st.Errors[0].ExternalID != "broken.pdf" {
		t.Fatalf("error external id = %q", st.Errors[0].ExternalID)
	}
}

func TestPutCircuitOpenSnoozes(t *testing.T) {
	store := &fakeStore{}
	emb := &fakeEmbedder{dim: 4, err: embed.ErrCircuitOpen}
	s := New(baseConfig(store, emb, &fakeSidecar{}))

	err := s.Put(context.Background(), mdDoc())
	var se *SnoozeError
	if !errors.As(err, &se) {
		t.Fatalf("Put err = %v, want *SnoozeError", err)
	}
	if !errors.Is(err, embed.ErrCircuitOpen) {
		t.Fatalf("SnoozeError must wrap ErrCircuitOpen, got %v", err)
	}
	if len(store.puts) != 0 {
		t.Fatalf("Put store calls = %d, want 0 (snoozed before commit)", len(store.puts))
	}
	st := s.Stats()
	if st.DocsFailed != 0 {
		t.Fatalf("DocsFailed = %d, want 0 (snooze is not a per-doc failure)", st.DocsFailed)
	}
	if st.DocsSeen != 1 {
		t.Fatalf("DocsSeen = %d, want 1", st.DocsSeen)
	}
}

func TestPutEmbedErrorRecordedPerDoc(t *testing.T) {
	store := &fakeStore{}
	emb := &fakeEmbedder{dim: 4, err: errors.New("provider 400")}
	s := New(baseConfig(store, emb, &fakeSidecar{}))

	if err := s.Put(context.Background(), mdDoc()); err != nil {
		t.Fatalf("Put returned %v, want nil (non-circuit embed error recorded per-doc)", err)
	}
	if len(store.puts) != 0 {
		t.Fatalf("Put store calls = %d, want 0 (no transaction opened without embeddings)", len(store.puts))
	}
	st := s.Stats()
	if st.DocsFailed != 1 || len(st.Errors) != 1 {
		t.Fatalf("stats = %+v, want DocsFailed=1", st)
	}
}

func TestPutStoreErrorPropagates(t *testing.T) {
	store := &fakeStore{putErr: errors.New("db down")}
	emb := &fakeEmbedder{dim: 4, tokens: 1}
	s := New(baseConfig(store, emb, &fakeSidecar{}))

	err := s.Put(context.Background(), mdDoc())
	if err == nil {
		t.Fatalf("Put err = nil, want the store error propagated (fail the job)")
	}
	var se *SnoozeError
	if errors.As(err, &se) {
		t.Fatalf("store error must not be a snooze")
	}
	if s.Stats().DocsChanged != 0 {
		t.Fatalf("DocsChanged = %d, want 0 (commit failed)", s.Stats().DocsChanged)
	}
}

func TestCompleteFullSyncSoftDeletesUnseen(t *testing.T) {
	store := &fakeStore{deleteCount: 3}
	s := New(Config{
		Store: store, Local: parse.Default(), Embedder: &fakeEmbedder{dim: 4},
		SourceID: testSourceID, Mode: Full, Model: "m", Now: time.Now,
	})
	if err := s.Complete(context.Background()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if store.deleteCalls != 1 {
		t.Fatalf("SoftDeleteUnseen calls = %d, want 1 (full sync)", store.deleteCalls)
	}
	if store.deleteSource != testSourceID {
		t.Fatalf("delete source = %q", store.deleteSource)
	}
	if store.deleteSince.IsZero() {
		t.Fatalf("delete startedAt must be the run start, got zero")
	}
	if s.Stats().DocsDeleted != 3 {
		t.Fatalf("DocsDeleted = %d, want 3", s.Stats().DocsDeleted)
	}
}

func TestCompleteIncrementalDoesNotDelete(t *testing.T) {
	store := &fakeStore{deleteCount: 9}
	s := New(Config{
		Store: store, Local: parse.Default(), Embedder: &fakeEmbedder{dim: 4},
		SourceID: testSourceID, Mode: Incremental, Model: "m", Now: time.Now,
	})
	if err := s.Complete(context.Background()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if store.deleteCalls != 0 {
		t.Fatalf("SoftDeleteUnseen calls = %d, want 0 (incremental never soft-deletes)", store.deleteCalls)
	}
	if s.Stats().DocsDeleted != 0 {
		t.Fatalf("DocsDeleted = %d, want 0", s.Stats().DocsDeleted)
	}
}

func TestStatsAccumulateAcrossDocsAndDuration(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	store := &fakeStore{}
	emb := &fakeEmbedder{dim: 4, tokens: 10}
	cfg := baseConfig(store, emb, &fakeSidecar{})
	cfg.Now = clock
	s := New(cfg)

	d1 := mdDoc()
	d1.BytesFetched = 100
	d2 := mdDoc()
	d2.ExternalID = "other.md"
	d2.BytesFetched = 250

	if err := s.Put(context.Background(), d1); err != nil {
		t.Fatalf("Put d1: %v", err)
	}
	if err := s.Put(context.Background(), d2); err != nil {
		t.Fatalf("Put d2: %v", err)
	}
	now = now.Add(2 * time.Second) // advance the clock for duration
	st := s.Stats()
	if st.DocsSeen != 2 {
		t.Fatalf("DocsSeen = %d, want 2", st.DocsSeen)
	}
	if st.BytesFetched != 350 {
		t.Fatalf("BytesFetched = %d, want 350", st.BytesFetched)
	}
	if st.EmbedTokens != 20 {
		t.Fatalf("EmbedTokens = %d, want 20", st.EmbedTokens)
	}
	if st.DurationMS != 2000 {
		t.Fatalf("DurationMS = %d, want 2000", st.DurationMS)
	}
}

func TestPutBytesFetchedDefaultsToDataLength(t *testing.T) {
	store := &fakeStore{}
	emb := &fakeEmbedder{dim: 4}
	s := New(baseConfig(store, emb, &fakeSidecar{}))
	doc := mdDoc() // BytesFetched unset
	if err := s.Put(context.Background(), doc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, want := s.Stats().BytesFetched, int64(len(doc.Data)); got != want {
		t.Fatalf("BytesFetched = %d, want len(Data)=%d", got, want)
	}
}
