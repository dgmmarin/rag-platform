package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/obs"
)

// TestCompleteEmitsProviderMetric proves a successful Complete records
// provider_request_duration_seconds{provider,op="llm.complete",status="ok"}
// through the resilience wrapper (SPEC-10 §2). Labels carry no prompt content.
func TestCompleteEmitsProviderMetric(t *testing.T) {
	tc := cases()[0] // openai non-stream shape
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tc.nonStream(w) }))
	defer srv.Close()

	m := obs.NewMetrics()
	p, err := New(Config{
		Provider: tc.provider,
		Model:    "test-model",
		Allowed:  []string{tc.provider},
		APIKey:   "k",
		BaseURL:  srv.URL,
		Metrics:  m,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 8}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"provider_request_duration_seconds", `op="llm.complete"`, `provider="` + tc.provider + `"`, `status="ok"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("llm provider metric missing %q:\n%s", want, body)
		}
	}
}
