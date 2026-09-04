package retrieve

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// --- fakes -------------------------------------------------------------------

// fakeResolver returns a fixed (possibly nil) *tenant.DB; the fake retriever
// never dereferences the handle, so Search is unit-testable without a database.
type fakeResolver struct {
	db     *tenant.DB
	err    error
	opened []tenant.ID
}

func (f *fakeResolver) Open(_ context.Context, id tenant.ID) (*tenant.DB, error) {
	f.opened = append(f.opened, id)
	return f.db, f.err
}
func (f *fakeResolver) Close(tenant.ID) {}

type fakeSettings struct {
	doc map[string]any
	err error
}

func (f fakeSettings) Get(context.Context, string) (map[string]any, error) {
	return f.doc, f.err
}

// fakeEmbedder records the texts it was asked to embed and returns a fixed vector.
type fakeEmbedder struct {
	vec      []float32
	err      error
	gotTexts []string
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) (embed.Result, error) {
	f.gotTexts = texts
	if f.err != nil {
		return embed.Result{}, f.err
	}
	return embed.Result{Vectors: [][]float32{f.vec}}, nil
}

// fakeFactory records the Settings it was handed and returns a fixed embedder.
type fakeFactory struct {
	emb         embed.Embedder
	err         error
	gotSettings Settings
}

func (f *fakeFactory) Embedder(_ context.Context, s Settings) (embed.Embedder, error) {
	f.gotSettings = s
	return f.emb, f.err
}

func newTestSettingsDoc() map[string]any {
	return map[string]any{
		"embedding":         map[string]any{"provider": "voyage", "model": "voyage-3", "dim": float64(8)},
		"providers_allowed": []any{"voyage", "cohere"},
		"retrieval":         map[string]any{"k_vector": float64(30), "k_text": float64(20), "final_k": float64(5)},
	}
}

func testTenantID() tenant.ID { return tenant.ID(uuid.New()) }

// --- tests -------------------------------------------------------------------

// TestSearchEmbedsQueryAndPassesFiltersAndTopK proves the golden path: the query
// string is embedded via the settings-built embedder, and the resulting vector,
// query text, top_k and filters are passed through to the hybrid retriever.
func TestSearchEmbedsQueryAndPassesFiltersAndTopK(t *testing.T) {
	emb := &fakeEmbedder{vec: []float32{1, 0, 0, 0, 0, 0, 0, 0}}
	factory := &fakeFactory{emb: emb}
	var got Params
	svc := &Service{
		Resolver: &fakeResolver{},
		Settings: fakeSettings{doc: newTestSettingsDoc()},
		Embedder: factory,
		Retriever: func(_ context.Context, _ *tenant.DB, p Params) ([]Result, error) {
			got = p
			return []Result{{ChunkID: "c1"}}, nil
		},
	}

	req := Request{
		Query: "how to reset the X200",
		TopK:  3,
		Filters: Filters{
			SourceIDs: []string{"11111111-1111-1111-1111-111111111111"},
			URIPrefix: "https://docs.acme.com/",
		},
	}
	res, err := svc.Search(context.Background(), testTenantID(), req)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].ChunkID != "c1" {
		t.Fatalf("results = %+v, want one chunk c1", res)
	}
	if len(emb.gotTexts) != 1 || emb.gotTexts[0] != req.Query {
		t.Fatalf("embedder got texts %v, want [%q]", emb.gotTexts, req.Query)
	}
	if !reflect.DeepEqual(got.Embedding, emb.vec) {
		t.Fatalf("retriever embedding = %v, want %v", got.Embedding, emb.vec)
	}
	if got.QueryText != req.Query {
		t.Fatalf("retriever QueryText = %q, want %q", got.QueryText, req.Query)
	}
	if got.K != 3 {
		t.Fatalf("retriever K = %d, want 3", got.K)
	}
	if !reflect.DeepEqual(got.Filters, req.Filters) {
		t.Fatalf("retriever Filters = %+v, want %+v", got.Filters, req.Filters)
	}
	// Settings drove the embedder factory.
	if factory.gotSettings.EmbeddingProvider != "voyage" || factory.gotSettings.EmbeddingModel != "voyage-3" {
		t.Fatalf("factory settings = %+v", factory.gotSettings)
	}
	if !reflect.DeepEqual(factory.gotSettings.ProvidersAllowed, []string{"voyage", "cohere"}) {
		t.Fatalf("factory providers = %v", factory.gotSettings.ProvidersAllowed)
	}
	// k_vector / k_text from settings flow through.
	if got.KVector != 30 || got.KText != 20 {
		t.Fatalf("retriever KVector/KText = %d/%d, want 30/20", got.KVector, got.KText)
	}
}

// TestSearchTopKDefaultsToSettingsFinalK: an unset top_k uses retrieval.final_k.
func TestSearchTopKDefaultsToSettingsFinalK(t *testing.T) {
	var got Params
	svc := &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: newTestSettingsDoc()},
		Embedder:  &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Retriever: func(_ context.Context, _ *tenant.DB, p Params) ([]Result, error) { got = p; return nil, nil },
	}
	if _, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.K != 5 {
		t.Fatalf("K = %d, want 5 (settings final_k)", got.K)
	}
}

// TestSearchTopKCeiling: an absurd top_k is clamped to the ceiling.
func TestSearchTopKCeiling(t *testing.T) {
	var got Params
	svc := &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: newTestSettingsDoc()},
		Embedder:  &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Retriever: func(_ context.Context, _ *tenant.DB, p Params) ([]Result, error) { got = p; return nil, nil },
	}
	if _, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q", TopK: 100000}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.K != defaultMaxTopK {
		t.Fatalf("K = %d, want ceiling %d", got.K, defaultMaxTopK)
	}
}

// TestSearchEmptyQuery: a blank query is a validation error, before any embed.
func TestSearchEmptyQuery(t *testing.T) {
	emb := &fakeEmbedder{vec: []float32{1}}
	svc := &Service{
		Resolver: &fakeResolver{},
		Settings: fakeSettings{doc: newTestSettingsDoc()},
		Embedder: &fakeFactory{emb: emb},
		Retriever: func(context.Context, *tenant.DB, Params) ([]Result, error) {
			t.Fatal("retriever called")
			return nil, nil
		},
	}
	if _, err := svc.Search(context.Background(), testTenantID(), Request{Query: "   "}); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("err = %v, want ErrEmptyQuery", err)
	}
	if emb.gotTexts != nil {
		t.Fatalf("embedder was called for an empty query")
	}
}

// TestSearchEmbedFailureMapped: a provider failure becomes ErrEmbedding (the
// handler maps that to a generic envelope, never leaking provider internals).
func TestSearchEmbedFailureMapped(t *testing.T) {
	svc := &Service{
		Resolver: &fakeResolver{},
		Settings: fakeSettings{doc: newTestSettingsDoc()},
		Embedder: &fakeFactory{emb: &fakeEmbedder{err: errors.New("429 from voyage: secret-token leaked")}},
		Retriever: func(context.Context, *tenant.DB, Params) ([]Result, error) {
			t.Fatal("retriever called")
			return nil, nil
		},
	}
	_, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q"})
	if !errors.Is(err, ErrEmbedding) {
		t.Fatalf("err = %v, want ErrEmbedding", err)
	}
}

// TestSearchFactoryFailureMapped: a factory failure (e.g. provider not allowed)
// also maps to ErrEmbedding.
func TestSearchFactoryFailureMapped(t *testing.T) {
	svc := &Service{
		Resolver: &fakeResolver{},
		Settings: fakeSettings{doc: newTestSettingsDoc()},
		Embedder: &fakeFactory{err: embed.ErrProviderNotAllowed},
		Retriever: func(context.Context, *tenant.DB, Params) ([]Result, error) {
			t.Fatal("retriever called")
			return nil, nil
		},
	}
	if _, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q"}); !errors.Is(err, ErrEmbedding) {
		t.Fatalf("err = %v, want ErrEmbedding", err)
	}
}

// TestSearchTenantUnavailable: a resolver lifecycle error becomes
// ErrTenantUnavailable (503), never a leaked internal error.
func TestSearchTenantUnavailable(t *testing.T) {
	svc := &Service{
		Resolver: &fakeResolver{err: tenant.ErrTenantUnavailable},
		Settings: fakeSettings{doc: newTestSettingsDoc()},
		Embedder: &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Retriever: func(context.Context, *tenant.DB, Params) ([]Result, error) {
			t.Fatal("retriever called")
			return nil, nil
		},
	}
	if _, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q"}); !errors.Is(err, ErrTenantUnavailable) {
		t.Fatalf("err = %v, want ErrTenantUnavailable", err)
	}
}

// TestKeyedEmbedderFactoryFailsClosed: the production factory refuses a provider
// absent from the tenant's providers_allowed (SPEC-09 §2) and builds an embedder
// for a permitted one — the query can never reach an un-permitted provider.
func TestKeyedEmbedderFactoryFailsClosed(t *testing.T) {
	f := KeyedEmbedderFactory{APIKey: "secret"}

	// Not in the allowlist → fail closed.
	if _, err := f.Embedder(context.Background(), Settings{
		EmbeddingProvider: "voyage", EmbeddingModel: "voyage-3", ProvidersAllowed: []string{"openai"},
	}); !errors.Is(err, embed.ErrProviderNotAllowed) {
		t.Fatalf("err = %v, want ErrProviderNotAllowed", err)
	}

	// Permitted → a usable embedder is built.
	emb, err := f.Embedder(context.Background(), Settings{
		EmbeddingProvider: "voyage", EmbeddingModel: "voyage-3", ProvidersAllowed: []string{"voyage"},
	})
	if err != nil {
		t.Fatalf("Embedder(permitted): %v", err)
	}
	if emb == nil {
		t.Fatal("Embedder(permitted) returned nil embedder")
	}
}
