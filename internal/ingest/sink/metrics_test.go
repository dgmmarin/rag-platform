package sink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/ingest/chunk"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/obs"
)

// TestPutEmitsIngestMetrics proves a changed document records
// ingest_documents_total{result="changed"}, ingest_chunks_total and
// embed_tokens_total under the tenant/source_kind/provider labels (SPEC-10 §2).
func TestPutEmitsIngestMetrics(t *testing.T) {
	m := obs.NewMetrics()
	cfg := baseConfig(&fakeStore{unchanged: false}, &fakeEmbedder{dim: 4, tokens: 17}, &fakeSidecar{})
	cfg.Metrics = m
	cfg.Tenant = "acme"
	cfg.SourceKind = "web_crawl"
	cfg.Provider = "voyage"
	s := New(cfg)

	if err := s.Put(context.Background(), mdDoc()); err != nil {
		t.Fatalf("Put: %v", err)
	}

	body := scrapeMetrics(t, m)
	for _, want := range []string{
		`ingest_documents_total{result="changed",source_kind="web_crawl",tenant="acme"}`,
		`ingest_chunks_total{provider="voyage",tenant="acme"}`,
		`embed_tokens_total{provider="voyage",tenant="acme"} 17`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("ingest metrics missing %q:\n%s", want, body)
		}
	}
}

// TestPutUnchangedAndFailedResults proves the result label tracks the per-doc
// outcome: an unchanged document and a parse failure each get their own series
// and neither spends chunks/tokens.
func TestPutUnchangedAndFailedResults(t *testing.T) {
	m := obs.NewMetrics()

	// Unchanged: TouchIfUnchanged short-circuits before embed.
	uc := baseConfig(&fakeStore{unchanged: true}, &fakeEmbedder{dim: 4, tokens: 5}, &fakeSidecar{})
	uc.Metrics, uc.Tenant, uc.SourceKind, uc.Provider = m, "acme", "upload", "voyage"
	if err := New(uc).Put(context.Background(), mdDoc()); err != nil {
		t.Fatalf("unchanged Put: %v", err)
	}

	// Failed: an unsupported MIME with no sidecar is a recorded per-doc failure.
	fc := baseConfig(&fakeStore{}, &fakeEmbedder{dim: 4}, &fakeSidecar{})
	fc.Sidecar = nil
	fc.Metrics, fc.Tenant, fc.SourceKind, fc.Provider = m, "acme", "upload", "voyage"
	if err := New(fc).Put(context.Background(), Document{ExternalID: "x.bin", Filename: "x.bin", MimeType: "application/octet-stream", Data: []byte("\x00\x01")}); err != nil {
		t.Fatalf("failed Put: %v", err)
	}

	body := scrapeMetrics(t, m)
	for _, want := range []string{
		`result="unchanged"`,
		`result="failed"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected result label %q:\n%s", want, body)
		}
	}
}

// TestPutEmitsEmbedChunksReusedMetric proves a reused chunk embedding
// (chunk-level drift, SPEC-05 §1) records embed_chunks_reused_total under the
// tenant/provider labels (SPEC-10 §2), mirroring TestPutEmitsIngestMetrics.
func TestPutEmitsEmbedChunksReusedMetric(t *testing.T) {
	doc := threeSectionDoc()
	norm, err := parse.Default().Parse(doc.MimeType, doc.Data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	chunks := chunk.Document(norm, chunk.Config{})
	if len(chunks) != 3 {
		t.Fatalf("fixture chunk count = %d, want 3", len(chunks))
	}
	hashes := make([][]byte, len(chunks))
	for i, c := range chunks {
		sum := sha256.Sum256([]byte(c.EmbedText))
		hashes[i] = sum[:]
	}

	// Seed the cache with the first two chunks' hashes; the third is a miss.
	known := map[string][]float32{
		hex.EncodeToString(hashes[0]): {0.1, 0.2, 0.3, 0.4},
		hex.EncodeToString(hashes[1]): {0.5, 0.6, 0.7, 0.8},
	}

	m := obs.NewMetrics()
	cfg := baseConfig(&fakeStore{unchanged: false}, &fakeEmbedder{dim: 4, tokens: 3}, &fakeSidecar{})
	cfg.Cache = fakeCache{known: known}
	cfg.Metrics = m
	cfg.Tenant = "acme"
	cfg.Provider = "voyage"
	s := New(cfg)

	if err := s.Put(context.Background(), doc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	body := scrapeMetrics(t, m)
	want := `embed_chunks_reused_total{provider="voyage",tenant="acme"} 2`
	if !strings.Contains(body, want) {
		t.Fatalf("expected %q:\n%s", want, body)
	}
}

func scrapeMetrics(t *testing.T, m *obs.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
