package worker

import (
	"context"
	"testing"

	"github.com/rag-platform/ragctl/internal/ingest/ingestdoc"
)

// TestEmbedderFactoryFailsClosed: the production factory must refuse to build an
// embedder for a provider the tenant has not allowed (SPEC-09 §2 fail-closed),
// before any key is used.
func TestEmbedderFactoryFailsClosed(t *testing.T) {
	f := KeyedEmbedderFactory{APIKey: "secret"}
	_, err := f.Embedder(context.Background(), ingestdoc.Settings{
		EmbeddingProvider: "openai",
		EmbeddingModel:    "text-embedding-3-small",
		ProvidersAllowed:  []string{"voyage"}, // openai NOT allowed
	})
	if err == nil {
		t.Fatal("expected fail-closed error when provider is not in providers_allowed")
	}
}

// TestEmbedderFactoryBuildsAllowed: an allowed provider yields a usable embedder.
func TestEmbedderFactoryBuildsAllowed(t *testing.T) {
	f := KeyedEmbedderFactory{APIKey: "secret"}
	emb, err := f.Embedder(context.Background(), ingestdoc.Settings{
		EmbeddingProvider: "openai",
		EmbeddingModel:    "text-embedding-3-small",
		ProvidersAllowed:  []string{"openai"},
	})
	if err != nil {
		t.Fatalf("build allowed embedder: %v", err)
	}
	if emb == nil {
		t.Fatal("embedder is nil")
	}
}
