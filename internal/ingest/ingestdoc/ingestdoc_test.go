package ingestdoc

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestParseSettings(t *testing.T) {
	raw := map[string]any{
		"embedding":         map[string]any{"provider": "voyage", "model": "voyage-3", "dim": float64(1024)},
		"chunking":          map[string]any{"target_tokens": float64(400), "overlap_tokens": float64(50)},
		"providers_allowed": []any{"voyage", "cohere"},
	}
	s := parseSettings(raw)
	if s.EmbeddingProvider != "voyage" || s.EmbeddingModel != "voyage-3" || s.EmbeddingDim != 1024 {
		t.Fatalf("embedding = %+v", s)
	}
	if s.ChunkTarget != 400 || s.ChunkOverlap != 50 {
		t.Fatalf("chunking = %+v", s)
	}
	if len(s.ProvidersAllowed) != 2 || s.ProvidersAllowed[0] != "voyage" {
		t.Fatalf("providers = %+v", s.ProvidersAllowed)
	}
}

func TestJobFromPayload(t *testing.T) {
	payload := json.RawMessage(`{
		"external_id":"notes.md","filename":"notes.md","mime_type":"text/markdown",
		"object_key":"uploads/t/abc-notes.md","source_id":"55555555-5555-5555-5555-555555555555"}`)
	job, err := JobFromPayload("11111111-1111-1111-1111-111111111111", payload)
	if err != nil {
		t.Fatalf("JobFromPayload: %v", err)
	}
	if job.TenantID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("tenant = %q", job.TenantID)
	}
	if job.ObjectKey != "uploads/t/abc-notes.md" || job.ExternalID != "notes.md" ||
		job.MimeType != "text/markdown" || job.SourceID != "55555555-5555-5555-5555-555555555555" {
		t.Fatalf("job = %+v", job)
	}
}

func TestJobFromPayloadRequiresObjectKeyAndSource(t *testing.T) {
	if _, err := JobFromPayload("t", json.RawMessage(`{"external_id":"x","source_id":"s"}`)); err == nil {
		t.Fatal("expected error for a payload with no object_key")
	}
	if _, err := JobFromPayload("t", json.RawMessage(`{"external_id":"x","object_key":"k"}`)); err == nil {
		t.Fatal("expected error for a payload with no source_id")
	}
}

// --- Run path with fakes (the *tenant.DB is nil; the fake store ignores it). ---

type fakeFetcher struct {
	data    []byte
	gotKey  string
	callErr error
}

func (f *fakeFetcher) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.gotKey = key
	if f.callErr != nil {
		return nil, f.callErr
	}
	return io.NopCloser(strings.NewReader(string(f.data))), nil
}

type fakeSettings struct{ doc map[string]any }

func (f fakeSettings) Get(_ context.Context, _ string) (map[string]any, error) { return f.doc, nil }

type stubEmbedder struct{ dim int }

func (e stubEmbedder) Embed(_ context.Context, texts []string) (embed.Result, error) {
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		vecs[i] = make([]float32, e.dim)
	}
	return embed.Result{Vectors: vecs, Tokens: len(texts) * 3}, nil
}

type stubFactory struct{ dim int }

func (f stubFactory) Embedder(_ context.Context, s Settings) (embed.Embedder, error) {
	d := f.dim
	if d == 0 {
		d = s.EmbeddingDim
	}
	return stubEmbedder{dim: d}, nil
}

type fakeStore struct {
	putIn     documents.PutInput
	putCalled int
	unchanged bool
}

func (f *fakeStore) TouchIfUnchanged(_ context.Context, _ *tenant.DB, _, _ string, _ []byte) (bool, error) {
	return f.unchanged, nil
}
func (f *fakeStore) Put(_ context.Context, _ *tenant.DB, in documents.PutInput) (documents.PutResult, error) {
	f.putCalled++
	f.putIn = in
	return documents.PutResult{DocumentID: "doc-1", VersionID: "ver-1", Changed: true}, nil
}
func (f *fakeStore) SoftDeleteUnseen(_ context.Context, _ *tenant.DB, _ string, _ time.Time) (int, error) {
	return 0, nil
}

func newIngestor(store *fakeStore, fetch *fakeFetcher) *Ingestor {
	return &Ingestor{
		Storage:  fetch,
		Settings: fakeSettings{doc: map[string]any{"embedding": map[string]any{"provider": "voyage", "model": "voyage-3", "dim": float64(8)}, "chunking": map[string]any{"target_tokens": float64(512), "overlap_tokens": float64(64)}, "providers_allowed": []any{"voyage"}}},
		Embedder: stubFactory{},
		Store:    store,
		Local:    parse.Default(),
		Now:      time.Now,
	}
}

func TestRunCreatesVersionForUploadedDocument(t *testing.T) {
	store := &fakeStore{}
	fetch := &fakeFetcher{data: []byte("# Title\n\nsome body text here.\n")}
	in := newIngestor(store, fetch)
	job := Job{
		TenantID: "t1", SourceID: "55555555-5555-5555-5555-555555555555",
		ObjectKey: "uploads/t1/abc-notes.md", ExternalID: "notes.md",
		Filename: "notes.md", MimeType: "text/markdown",
	}
	stats, err := in.Run(context.Background(), nil, job)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fetch.gotKey != job.ObjectKey {
		t.Fatalf("fetched key = %q, want %q", fetch.gotKey, job.ObjectKey)
	}
	if store.putCalled != 1 {
		t.Fatalf("store.Put calls = %d, want 1", store.putCalled)
	}
	if store.putIn.SourceID != job.SourceID || store.putIn.ExternalID != job.ExternalID {
		t.Fatalf("put identity = (%q,%q), want (%q,%q)", store.putIn.SourceID, store.putIn.ExternalID, job.SourceID, job.ExternalID)
	}
	if len(store.putIn.Chunks) == 0 {
		t.Fatal("expected at least one chunk written")
	}
	for i, c := range store.putIn.Chunks {
		if c.EmbeddingModel != "voyage-3" {
			t.Fatalf("chunk %d model = %q, want voyage-3", i, c.EmbeddingModel)
		}
	}
	if stats.DocsChanged != 1 {
		t.Fatalf("stats.DocsChanged = %d, want 1", stats.DocsChanged)
	}
}

func TestRunSkipsUnchangedDocument(t *testing.T) {
	store := &fakeStore{unchanged: true}
	fetch := &fakeFetcher{data: []byte("# Title\n\nsome body text here.\n")}
	in := newIngestor(store, fetch)
	stats, err := in.Run(context.Background(), nil, Job{
		SourceID: "55555555-5555-5555-5555-555555555555", ObjectKey: "k", ExternalID: "notes.md", MimeType: "text/markdown",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.putCalled != 0 {
		t.Fatal("unchanged document must not be re-put")
	}
	if stats.DocsUnchanged != 1 {
		t.Fatalf("stats.DocsUnchanged = %d, want 1", stats.DocsUnchanged)
	}
}
