package retrieve

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/rerank"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// --- rerank fakes (STORY-08.3) -----------------------------------------------

// fakeReranker returns a fixed ranking (or error) and records the docs it saw.
type fakeReranker struct {
	scored  []rerank.Scored
	err     error
	gotDocs []rerank.Doc
	calls   int
}

func (f *fakeReranker) Rerank(_ context.Context, _ string, docs []rerank.Doc) ([]rerank.Scored, error) {
	f.calls++
	f.gotDocs = docs
	if f.err != nil {
		return nil, f.err
	}
	return f.scored, nil
}

// fakeRerankFactory hands back a fixed reranker (possibly nil = disabled) or error.
type fakeRerankFactory struct {
	rr          rerank.Reranker
	err         error
	gotSettings Settings
}

func (f *fakeRerankFactory) Reranker(_ context.Context, s Settings) (rerank.Reranker, error) {
	f.gotSettings = s
	return f.rr, f.err
}

// rerankSettingsDoc is a settings doc with the reranker enabled.
func rerankSettingsDoc(topN int) map[string]any {
	d := newTestSettingsDoc()
	d["reranker"] = map[string]any{"enabled": true, "provider": "cohere", "model": "rerank-v3.5", "top_n": float64(topN)}
	return d
}

// fixedRetriever returns a fixed fused result set (highest score first).
func fixedRetriever(results []Result, capture *Params) func(context.Context, *tenant.DB, Params) ([]Result, error) {
	return func(_ context.Context, _ *tenant.DB, p Params) ([]Result, error) {
		if capture != nil {
			*capture = p
		}
		return results, nil
	}
}

func fused3() []Result {
	return []Result{
		{ChunkID: "c1", Content: "one", Score: 0.5},
		{ChunkID: "c2", Content: "two", Score: 0.4},
		{ChunkID: "c3", Content: "three", Score: 0.3},
	}
}

// TestSearchReranksWhenEnabled: with the reranker enabled, the fused order is
// re-ordered by the reranker's scores, and each result's Score becomes the
// reranker score (SPEC-06 §3).
func TestSearchReranksWhenEnabled(t *testing.T) {
	rr := &fakeReranker{scored: []rerank.Scored{
		{ID: "c3", Score: 0.99}, {ID: "c1", Score: 0.60}, {ID: "c2", Score: 0.10},
	}}
	svc := &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: rerankSettingsDoc(20)},
		Embedder:  &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Reranker:  &fakeRerankFactory{rr: rr},
		Retriever: fixedRetriever(fused3(), nil),
	}
	res, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	gotIDs := []string{res[0].ChunkID, res[1].ChunkID, res[2].ChunkID}
	if !reflect.DeepEqual(gotIDs, []string{"c3", "c1", "c2"}) {
		t.Fatalf("order = %v, want [c3 c1 c2]", gotIDs)
	}
	if res[0].Score != 0.99 {
		t.Fatalf("top Score = %v, want reranker score 0.99", res[0].Score)
	}
	if rr.calls != 1 {
		t.Fatalf("reranker calls = %d, want 1", rr.calls)
	}
	// The reranker saw the chunk content as document text.
	if len(rr.gotDocs) != 3 || rr.gotDocs[0].Text != "one" {
		t.Fatalf("reranker docs = %+v", rr.gotDocs)
	}
}

// TestSearchRerankFallbackOnRerankerError: a reranker failure must NOT fail the
// query — the original fused order is returned (FR-RET-03 AC, NFR-REL-04).
func TestSearchRerankFallbackOnRerankerError(t *testing.T) {
	rr := &fakeReranker{err: errors.New("cohere 503")}
	svc := &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: rerankSettingsDoc(20)},
		Embedder:  &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Reranker:  &fakeRerankFactory{rr: rr},
		Retriever: fixedRetriever(fused3(), nil),
	}
	res, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Search must not fail on reranker error: %v", err)
	}
	gotIDs := []string{res[0].ChunkID, res[1].ChunkID, res[2].ChunkID}
	if !reflect.DeepEqual(gotIDs, []string{"c1", "c2", "c3"}) {
		t.Fatalf("order = %v, want fused [c1 c2 c3] on fallback", gotIDs)
	}
}

// TestSearchRerankFallbackOnFactoryError: a factory failure (e.g. missing key,
// fail-closed) also falls back to fused order rather than failing the query.
func TestSearchRerankFallbackOnFactoryError(t *testing.T) {
	svc := &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: rerankSettingsDoc(20)},
		Embedder:  &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Reranker:  &fakeRerankFactory{err: rerank.ErrMissingKey},
		Retriever: fixedRetriever(fused3(), nil),
	}
	res, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Search must not fail on factory error: %v", err)
	}
	if res[0].ChunkID != "c1" {
		t.Fatalf("order[0] = %q, want fused c1", res[0].ChunkID)
	}
}

// TestSearchNoRerankWhenDisabled: with the toggle off (factory returns nil), no
// rerank happens and the fused order (and the fetch size) is unchanged.
func TestSearchNoRerankWhenDisabled(t *testing.T) {
	var got Params
	svc := &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: newTestSettingsDoc()}, // no reranker block
		Embedder:  &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Reranker:  &fakeRerankFactory{rr: nil}, // disabled
		Retriever: fixedRetriever(fused3(), &got),
	}
	res, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q", TopK: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res[0].ChunkID != "c1" {
		t.Fatalf("order[0] = %q, want fused c1", res[0].ChunkID)
	}
	if got.K != 3 {
		t.Fatalf("fetch K = %d, want 3 (no over-fetch when rerank disabled)", got.K)
	}
}

// TestSearchOverFetchesTopNThenTruncates: when reranking, the hybrid query fetches
// top_n candidates (not just final_k), reranks them, then truncates to final_k.
func TestSearchOverFetchesTopNThenTruncates(t *testing.T) {
	var got Params
	// top_n = 3, final_k (top_k) = 2. Fetch 3, rerank, return 2.
	rr := &fakeReranker{scored: []rerank.Scored{
		{ID: "c3", Score: 0.99}, {ID: "c1", Score: 0.60}, {ID: "c2", Score: 0.10},
	}}
	svc := &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: rerankSettingsDoc(3)},
		Embedder:  &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Reranker:  &fakeRerankFactory{rr: rr},
		Retriever: fixedRetriever(fused3(), &got),
	}
	res, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q", TopK: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.K != 3 {
		t.Fatalf("fetch K = %d, want 3 (top_n over-fetch)", got.K)
	}
	if len(res) != 2 {
		t.Fatalf("len(res) = %d, want 2 (truncated to final_k)", len(res))
	}
	if res[0].ChunkID != "c3" || res[1].ChunkID != "c1" {
		t.Fatalf("order = [%s %s], want [c3 c1]", res[0].ChunkID, res[1].ChunkID)
	}
}

// TestKeyedRerankerFactory: the production factory returns nil when disabled,
// builds a Cohere reranker when enabled+allowed+keyed, fails closed on the
// allowlist (SPEC-09 §2), and builds an LLM reranker reusing the tenant's llm.
func TestKeyedRerankerFactory(t *testing.T) {
	f := KeyedRerankerFactory{
		CohereAPIKey: "co-key",
		LLM:          llm.Factory{Keys: llm.Keys{Anthropic: "sk"}},
	}

	// Disabled → nil reranker, no error.
	rr, err := f.Reranker(context.Background(), Settings{RerankEnabled: false})
	if err != nil || rr != nil {
		t.Fatalf("disabled: rr=%v err=%v, want nil,nil", rr, err)
	}

	// Cohere enabled + allowed + key → a reranker is built.
	rr, err = f.Reranker(context.Background(), Settings{
		RerankEnabled: true, RerankProvider: "cohere", RerankModel: "rerank-v3.5",
		RerankTopN: 20, ProvidersAllowed: []string{"cohere"},
	})
	if err != nil || rr == nil {
		t.Fatalf("cohere permitted: rr=%v err=%v, want non-nil,nil", rr, err)
	}

	// Cohere not in providers_allowed → fail closed.
	_, err = f.Reranker(context.Background(), Settings{
		RerankEnabled: true, RerankProvider: "cohere", RerankModel: "rerank-v3.5",
		ProvidersAllowed: []string{"voyage"},
	})
	if !errors.Is(err, rerank.ErrProviderNotAllowed) {
		t.Fatalf("cohere not allowed: err=%v, want ErrProviderNotAllowed", err)
	}

	// LLM reranker reusing the tenant's anthropic provider/model.
	rr, err = f.Reranker(context.Background(), Settings{
		RerankEnabled: true, RerankProvider: "llm",
		LLMProvider: "anthropic", LLMModel: "claude-sonnet-5",
		ProvidersAllowed: []string{"anthropic"}, LLMModelsAllowed: []string{"claude-sonnet-5"},
	})
	if err != nil || rr == nil {
		t.Fatalf("llm reranker: rr=%v err=%v, want non-nil,nil", rr, err)
	}

	// LLM reranker with a model outside the allowlist → fail closed (via llm.New).
	_, err = f.Reranker(context.Background(), Settings{
		RerankEnabled: true, RerankProvider: "llm",
		LLMProvider: "anthropic", LLMModel: "claude-sonnet-5", RerankLLMModel: "gpt-4o",
		ProvidersAllowed: []string{"anthropic"}, LLMModelsAllowed: []string{"claude-sonnet-5"},
	})
	if err == nil {
		t.Fatal("llm reranker with disallowed override model: want error, got nil")
	}
}

// TestParseSettingsReranker: the reranker + llm blocks are read from the settings
// document for the reranker factory.
func TestParseSettingsReranker(t *testing.T) {
	doc := rerankSettingsDoc(15)
	doc["reranker"].(map[string]any)["llm_model"] = "claude-haiku-4-5"
	doc["llm"] = map[string]any{"provider": "anthropic", "model": "claude-sonnet-5", "models_allowed": []any{"claude-sonnet-5"}}
	st := parseSettings(doc)
	if !st.RerankEnabled || st.RerankProvider != "cohere" || st.RerankModel != "rerank-v3.5" || st.RerankTopN != 15 {
		t.Fatalf("reranker settings = %+v", st)
	}
	if st.RerankLLMModel != "claude-haiku-4-5" {
		t.Fatalf("RerankLLMModel = %q", st.RerankLLMModel)
	}
	if st.LLMProvider != "anthropic" || st.LLMModel != "claude-sonnet-5" {
		t.Fatalf("llm settings = %+v", st)
	}
	if !reflect.DeepEqual(st.LLMModelsAllowed, []string{"claude-sonnet-5"}) {
		t.Fatalf("LLMModelsAllowed = %v", st.LLMModelsAllowed)
	}
}

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
