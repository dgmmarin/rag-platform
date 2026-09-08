package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/obs"
)

// TestEmbedEmitsProviderMetric proves a successful Embed records
// provider_request_duration_seconds{provider,op="embed",status="ok"} through the
// batching wrapper (SPEC-10 §2). Labels carry no embedded text.
func TestEmbedEmitsProviderMetric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var data []map[string]any
		for i := range body.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float64{0.1}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"total_tokens": 5}})
	}))
	defer srv.Close()

	m := obs.NewMetrics()
	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"},
		APIKey: "k", Model: "text-embedding-3-small", BaseURL: srv.URL,
		Metrics: m,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"provider_request_duration_seconds", `op="embed"`, `provider="openai"`, `status="ok"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("embed provider metric missing %q:\n%s", want, body)
		}
	}
}
