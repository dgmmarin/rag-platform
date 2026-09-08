package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape renders the registry's Prometheus text exposition.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics handler: want 200, got %d", rec.Code)
	}
	return rec.Body.String()
}

// TestCatalogueEmitsRetrievalMetrics proves the retrieval histogram and the
// grounded counter appear once observed, with their SPEC-10 §2 labels.
func TestCatalogueEmitsRetrievalMetrics(t *testing.T) {
	m := NewMetrics()
	m.ObserveRetrieval("acme", true, 0.12)
	m.IncGrounded("acme", false)

	body := scrape(t, m)
	for _, want := range []string{
		"query_retrieval_duration_seconds",
		`reranked="true"`,
		"query_grounded_total",
		`grounded="false"`,
		`tenant="acme"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("retrieval metrics missing %q:\n%s", want, body)
		}
	}
}

// TestCatalogueEmitsJobMetrics proves the per-kind job histogram, failure
// counter and the queue-depth gauge are exposed (SPEC-10 §2).
func TestCatalogueEmitsJobMetrics(t *testing.T) {
	m := NewMetrics()
	m.ObserveJob("ingest_document", 1.5)
	m.IncJobFailed("sync_source")
	m.SetQueueDepth("ingest", 7)

	body := scrape(t, m)
	for _, want := range []string{
		"jobs_duration_seconds",
		`kind="ingest_document"`,
		"jobs_failed_total",
		`kind="sync_source"`,
		"jobs_queue_depth",
		`queue="ingest"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("job metrics missing %q:\n%s", want, body)
		}
	}
}

// TestCatalogueEmitsIngestMetrics proves the ingest counters appear with their
// SPEC-10 §2 labels (documents by result/source_kind, chunks + tokens by provider).
func TestCatalogueEmitsIngestMetrics(t *testing.T) {
	m := NewMetrics()
	m.IncIngestDocument("acme", "web_crawl", "changed")
	m.AddIngestChunks("acme", "voyage", 12)
	m.AddEmbedTokens("acme", "voyage", 3400)

	body := scrape(t, m)
	for _, want := range []string{
		"ingest_documents_total",
		`source_kind="web_crawl"`,
		`result="changed"`,
		"ingest_chunks_total",
		"embed_tokens_total",
		`provider="voyage"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("ingest metrics missing %q:\n%s", want, body)
		}
	}
}

// TestCatalogueEmitsProviderMetrics proves ObserveProvider records the request
// histogram for every call and the error counter only on failure, labelled by
// provider/op/status (SPEC-10 §2/§5).
func TestCatalogueEmitsProviderMetrics(t *testing.T) {
	m := NewMetrics()
	m.ObserveProvider("anthropic", "llm.complete", nil, 0.4)
	m.ObserveProvider("openai", "embed", errBoom, 0.2)

	body := scrape(t, m)
	for _, want := range []string{
		"provider_request_duration_seconds",
		`provider="anthropic"`,
		`op="llm.complete"`,
		`status="ok"`,
		"provider_errors_total",
		`provider="openai"`,
		`op="embed"`,
		`status="error"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("provider metrics missing %q:\n%s", want, body)
		}
	}
	// A successful call must NOT be counted as an error.
	if strings.Contains(body, `provider_errors_total{op="llm.complete",provider="anthropic",status="ok"}`) {
		t.Fatalf("successful provider call wrongly counted as error:\n%s", body)
	}
}

// TestRateLimitedCounterExposed proves the rate-limit counter is owned by the
// catalogue (api_rate_limited_total) and increments through the returned handle.
func TestRateLimitedCounterExposed(t *testing.T) {
	m := NewMetrics()
	m.RateLimitedCounter().Inc()

	body := scrape(t, m)
	if !strings.Contains(body, "api_rate_limited_total") {
		t.Fatalf("missing api_rate_limited_total:\n%s", body)
	}
}

// TestPoolGaugeReflectsCallback proves tenant_pools_open is a GaugeFunc reading
// the live pool count on scrape (SPEC-10 §2).
func TestPoolGaugeReflectsCallback(t *testing.T) {
	m := NewMetrics()
	open := 3
	m.SetPoolGauge(func() int { return open })

	body := scrape(t, m)
	if !strings.Contains(body, "tenant_pools_open 3") {
		t.Fatalf("tenant_pools_open not 3:\n%s", body)
	}
}

// TestNilMetricsMethodsAreSafe proves the emission methods are no-ops on a nil
// Metrics so a subsystem wired without metrics (optional dependency) is safe.
func TestNilMetricsMethodsAreSafe(_ *testing.T) {
	var m *Metrics
	m.ObserveRetrieval("t", false, 1)
	m.IncGrounded("t", true)
	m.ObserveJob("k", 1)
	m.IncJobFailed("k")
	m.SetQueueDepth("q", 1)
	m.IncIngestDocument("t", "upload", "changed")
	m.AddIngestChunks("t", "voyage", 3)
	m.AddEmbedTokens("t", "voyage", 100)
	m.ObserveProvider("p", "op", errBoom, 1)
}

var errBoom = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }
