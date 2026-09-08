package rerank

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/obs"
)

// TestRerankEmitsProviderMetric proves a Rerank call records
// provider_request_duration_seconds{provider="llm",op="rerank",status="ok"}
// through the metered decorator (SPEC-10 §2). Uses the LLM reranker with a fake
// Completer so no network is touched; labels carry no doc content.
func TestRerankEmitsProviderMetric(t *testing.T) {
	m := obs.NewMetrics()
	fc := &fakeCompleter{resp: llm.Response{Text: `[{"id":1,"score":0.9},{"id":2,"score":0.1},{"id":3,"score":0.0}]`}}
	rr, err := New(Config{Enabled: true, Provider: "llm", LLM: fc, LLMModel: "m", Allowed: []string{"anthropic"}, Metrics: m})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := rr.Rerank(context.Background(), "q", testDocs()); err != nil {
		t.Fatalf("Rerank: %v", err)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"provider_request_duration_seconds", `op="rerank"`, `provider="llm"`, `status="ok"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("rerank provider metric missing %q:\n%s", want, body)
		}
	}
}
