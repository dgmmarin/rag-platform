package query

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/retrieve"
)

// TestQueryEmitsGroundedMetric proves a grounded answer records
// query_grounded_total{grounded="true"} and a below-floor refusal records
// grounded="false" (SPEC-10 §2/§5, the per-tenant grounded rate).
func TestQueryEmitsGroundedMetric(t *testing.T) {
	m := obs.NewMetrics()

	grounded := &fakeRetriever{results: []retrieve.Result{result("c1", "d1", "reset the x200", 0.9)}}
	prov := &stubProvider{completeResp: llm.Response{Text: "Hold reset [1].", Model: "m"}}
	svc := newService(grounded, prov, &fakeUsage{})
	svc.Metrics = m
	if _, err := svc.Query(context.Background(), testTenant, Request{Question: "reset?"}); err != nil {
		t.Fatalf("grounded Query: %v", err)
	}

	refuse := &fakeRetriever{results: []retrieve.Result{result("c9", "d9", "irrelevant", 0.01)}}
	svc2 := newService(refuse, prov, &fakeUsage{})
	svc2.Metrics = m
	if _, err := svc2.Query(context.Background(), testTenant, Request{Question: "reset?"}); err != nil {
		t.Fatalf("refusing Query: %v", err)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"query_grounded_total", `grounded="true"`, `grounded="false"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("grounded metric missing %q:\n%s", want, body)
		}
	}
}
