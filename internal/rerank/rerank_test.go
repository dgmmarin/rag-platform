package rerank

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rag-platform/ragctl/internal/llm"
)

// --- factory (New) fail-closed / toggle ---------------------------------------

// TestNewDisabledReturnsNil: the per-tenant toggle off yields a nil reranker (the
// service treats nil as "no rerank"), never an error.
func TestNewDisabledReturnsNil(t *testing.T) {
	rr, err := New(Config{Enabled: false, Provider: "cohere"})
	if err != nil {
		t.Fatalf("New(disabled): %v", err)
	}
	if rr != nil {
		t.Fatalf("New(disabled) = %v, want nil reranker", rr)
	}
}

// TestNewCohereMissingKeyFailsClosed: cohere selected + enabled but no key → a
// clean ErrMissingKey (fail-closed; the service falls back to fused order).
func TestNewCohereMissingKeyFailsClosed(t *testing.T) {
	_, err := New(Config{Enabled: true, Provider: "cohere", Allowed: []string{"cohere"}, Model: "rerank-v3.5"})
	if !errors.Is(err, ErrMissingKey) {
		t.Fatalf("err = %v, want ErrMissingKey", err)
	}
}

// TestNewCohereProviderNotAllowed: cohere absent from providers_allowed →
// fail-closed (SPEC-09 §2), the tenant's data never reaches cohere.
func TestNewCohereProviderNotAllowed(t *testing.T) {
	_, err := New(Config{Enabled: true, Provider: "cohere", Allowed: []string{"voyage"}, CohereAPIKey: "k", Model: "rerank-v3.5"})
	if !errors.Is(err, ErrProviderNotAllowed) {
		t.Fatalf("err = %v, want ErrProviderNotAllowed", err)
	}
}

// TestNewUnknownProvider: an unrecognised reranker provider is rejected.
func TestNewUnknownProvider(t *testing.T) {
	_, err := New(Config{Enabled: true, Provider: "bogus", Allowed: []string{"bogus"}})
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("err = %v, want ErrUnknownProvider", err)
	}
}

// TestNewLLMMissingProvider: the LLM reranker selected but no llm.Provider wired →
// fail-closed ErrMissingKey.
func TestNewLLMMissingProvider(t *testing.T) {
	_, err := New(Config{Enabled: true, Provider: "llm", LLMModel: "claude-sonnet-5"})
	if !errors.Is(err, ErrMissingKey) {
		t.Fatalf("err = %v, want ErrMissingKey", err)
	}
}

// --- Cohere reranker (real HTTP against an httptest fixture) -------------------

func testDocs() []Doc {
	return []Doc{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}, {ID: "c", Text: "gamma"}}
}

// TestCohereReordersByRelevance: the Cohere v2 rerank response's index→score
// mapping is applied back onto the input docs, ordered by relevance_score desc.
func TestCohereReordersByRelevance(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/rerank" {
			t.Errorf("path = %q, want /v2/rerank", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer secret-key" {
			t.Errorf("auth header = %q, want bearer secret-key", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, `{"results":[
			{"index":2,"relevance_score":0.91},
			{"index":0,"relevance_score":0.50},
			{"index":1,"relevance_score":0.10}]}`)
	}))
	defer srv.Close()

	rr, err := New(Config{
		Enabled: true, Provider: "cohere", Allowed: []string{"cohere"},
		Model: "rerank-v3.5", CohereAPIKey: "secret-key", CohereBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	scored, err := rr.Rerank(context.Background(), "the query", testDocs())
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	assertOrder(t, scored, "c", "a", "b")
	if scored[0].Score != 0.91 {
		t.Errorf("top score = %v, want 0.91", scored[0].Score)
	}
	// The query and every document text was sent.
	if gotBody["query"] != "the query" {
		t.Errorf("query in body = %v", gotBody["query"])
	}
	if gotBody["model"] != "rerank-v3.5" {
		t.Errorf("model in body = %v", gotBody["model"])
	}
	if docs, ok := gotBody["documents"].([]any); !ok || len(docs) != 3 {
		t.Errorf("documents in body = %v, want 3", gotBody["documents"])
	}
}

// TestCohereRetriesOn429: a 429 with Retry-After:0 is retried and then succeeds —
// the resilience wrapper (copied from internal/ingest/embed) drives it.
func TestCohereRetriesOn429(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt64(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"slow down"}`)
			return
		}
		writeJSON(w, `{"results":[{"index":0,"relevance_score":0.7}]}`)
	}))
	defer srv.Close()

	rr, err := New(Config{
		Enabled: true, Provider: "cohere", Allowed: []string{"cohere"},
		Model: "rerank-v3.5", CohereAPIKey: "k", CohereBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	scored, err := rr.Rerank(context.Background(), "q", []Doc{{ID: "a", Text: "alpha"}})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(scored) != 1 || scored[0].ID != "a" {
		t.Fatalf("scored = %+v, want [a]", scored)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("server calls = %d, want 2 (one 429 then success)", got)
	}
}

// TestCohereTerminalErrorNotRetriedNorLeaked: a 400 is terminal (not retried) and
// the error never carries the API key (C-4).
func TestCohereTerminalErrorNotRetriedNorLeaked(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"bad model"}`)
	}))
	defer srv.Close()

	rr, _ := New(Config{
		Enabled: true, Provider: "cohere", Allowed: []string{"cohere"},
		Model: "rerank-v3.5", CohereAPIKey: "super-secret-key", CohereBaseURL: srv.URL,
	})
	_, err := rr.Rerank(context.Background(), "q", testDocs())
	if err == nil {
		t.Fatal("Rerank: want error on 400")
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("server calls = %d, want 1 (terminal, not retried)", got)
	}
	if containsStr(err.Error(), "super-secret-key") {
		t.Fatalf("error leaked the API key: %v", err)
	}
}

// --- LLM reranker (single batched Complete call against a fake provider) -------

// fakeCompleter is a stand-in llm.Provider (Completer subset): one Complete call,
// returning a canned Response.
type fakeCompleter struct {
	resp   llm.Response
	err    error
	calls  int64
	gotReq llm.Request
}

func (f *fakeCompleter) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	atomic.AddInt64(&f.calls, 1)
	f.gotReq = req
	return f.resp, f.err
}

// TestLLMReranksInOneCall: the listwise prompt is scored in a SINGLE Complete call
// and the returned {id,score} ranking reorders the docs.
func TestLLMReranksInOneCall(t *testing.T) {
	fc := &fakeCompleter{resp: llm.Response{Text: `[{"id":3,"score":0.9},{"id":1,"score":0.4},{"id":2,"score":0.1}]`}}
	rr, err := New(Config{Enabled: true, Provider: "llm", LLM: fc, LLMModel: "claude-sonnet-5", Allowed: []string{"anthropic"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	scored, err := rr.Rerank(context.Background(), "q", testDocs())
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	assertOrder(t, scored, "c", "a", "b")
	if got := atomic.LoadInt64(&fc.calls); got != 1 {
		t.Fatalf("Complete calls = %d, want exactly 1 (batched)", got)
	}
	if fc.gotReq.Model != "claude-sonnet-5" {
		t.Errorf("model = %q, want claude-sonnet-5", fc.gotReq.Model)
	}
}

// TestLLMToleratesFencedJSON: model noise (a ```json fence + prose) around the
// array is parsed defensively.
func TestLLMToleratesFencedJSON(t *testing.T) {
	fc := &fakeCompleter{resp: llm.Response{Text: "Sure! Here is the ranking:\n```json\n[{\"id\":2,\"score\":0.8},{\"id\":1,\"score\":0.2}]\n```\n"}}
	rr, _ := New(Config{Enabled: true, Provider: "llm", LLM: fc, LLMModel: "m", Allowed: []string{"anthropic"}})
	scored, err := rr.Rerank(context.Background(), "q", []Doc{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	assertOrder(t, scored, "b", "a")
}

// TestLLMUnparseableIsError: unparseable model output is a provider failure the
// caller (service) turns into a fallback-to-fused.
func TestLLMUnparseableIsError(t *testing.T) {
	fc := &fakeCompleter{resp: llm.Response{Text: "I cannot rank these passages."}}
	rr, _ := New(Config{Enabled: true, Provider: "llm", LLM: fc, LLMModel: "m", Allowed: []string{"anthropic"}})
	_, err := rr.Rerank(context.Background(), "q", testDocs())
	if !errors.Is(err, ErrUnparseable) {
		t.Fatalf("err = %v, want ErrUnparseable", err)
	}
}

// TestLLMProviderErrorPropagates: a Complete error propagates (→ service fallback).
func TestLLMProviderErrorPropagates(t *testing.T) {
	fc := &fakeCompleter{err: llm.ErrCircuitOpen}
	rr, _ := New(Config{Enabled: true, Provider: "llm", LLM: fc, LLMModel: "m", Allowed: []string{"anthropic"}})
	_, err := rr.Rerank(context.Background(), "q", testDocs())
	if err == nil {
		t.Fatal("Rerank: want provider error to propagate")
	}
}

// TestLLMOmittedDocsSurviveLast: a doc the model omits is not dropped — it lands
// last, so a later final_k truncation still has a full candidate set.
func TestLLMOmittedDocsSurviveLast(t *testing.T) {
	fc := &fakeCompleter{resp: llm.Response{Text: `[{"id":2,"score":0.9},{"id":1,"score":0.5}]`}} // omits doc 3 (c)
	rr, _ := New(Config{Enabled: true, Provider: "llm", LLM: fc, LLMModel: "m", Allowed: []string{"anthropic"}})
	scored, err := rr.Rerank(context.Background(), "q", testDocs())
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(scored) != 3 {
		t.Fatalf("len = %d, want 3 (no doc dropped)", len(scored))
	}
	if scored[len(scored)-1].ID != "c" {
		t.Fatalf("last = %q, want c (omitted doc last)", scored[len(scored)-1].ID)
	}
}

// TestRerankEmptyDocsNoop: no docs → no work, no provider call.
func TestRerankEmptyDocsNoop(t *testing.T) {
	fc := &fakeCompleter{}
	rr, _ := New(Config{Enabled: true, Provider: "llm", LLM: fc, LLMModel: "m", Allowed: []string{"anthropic"}})
	scored, err := rr.Rerank(context.Background(), "q", nil)
	if err != nil {
		t.Fatalf("Rerank(empty): %v", err)
	}
	if len(scored) != 0 {
		t.Fatalf("scored = %v, want empty", scored)
	}
	if atomic.LoadInt64(&fc.calls) != 0 {
		t.Fatal("provider called for empty docs")
	}
}

// --- helpers -------------------------------------------------------------------

func assertOrder(t *testing.T, scored []Scored, want ...string) {
	t.Helper()
	if len(scored) < len(want) {
		t.Fatalf("scored len = %d, want >= %d: %+v", len(scored), len(want), scored)
	}
	for i, id := range want {
		if scored[i].ID != id {
			t.Fatalf("scored[%d].ID = %q, want %q (full: %+v)", i, scored[i].ID, id, scored)
		}
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
