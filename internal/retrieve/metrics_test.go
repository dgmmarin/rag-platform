package retrieve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/rerank"
)

// TestSearchEmitsRetrievalMetric proves a successful Search records
// query_retrieval_duration_seconds with the reranked label (SPEC-10 §2).
func TestSearchEmitsRetrievalMetric(t *testing.T) {
	m := obs.NewMetrics()
	rr := &fakeReranker{scored: []rerank.Scored{{ID: "c1", Score: 0.9}}}
	svc := &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: rerankSettingsDoc(20)},
		Embedder:  &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Reranker:  &fakeRerankFactory{rr: rr},
		Retriever: fixedRetriever(fused3(), nil),
		Metrics:   m,
	}
	if _, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q"}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "query_retrieval_duration_seconds") || !strings.Contains(body, `reranked="true"`) {
		t.Fatalf("missing reranked retrieval metric:\n%s", body)
	}
}

// TestSearchWithoutMetricsIsSafe proves the metric is optional (nil Metrics).
func TestSearchWithoutMetricsIsSafe(t *testing.T) {
	svc := &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: newTestSettingsDoc()},
		Embedder:  &fakeFactory{emb: &fakeEmbedder{vec: []float32{1}}},
		Retriever: fixedRetriever(fused3(), nil),
	}
	if _, err := svc.Search(context.Background(), testTenantID(), Request{Query: "q"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
}
