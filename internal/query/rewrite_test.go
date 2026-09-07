package query

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/retrieve"
)

// --- Test doubles (rewrite step, STORY-08.7) ------------------------------

// recProvider records every Complete call (count + request) and returns a canned
// response. It stands in for the tenant's llm.Provider so a test can assert whether
// the rewrite step made an LLM call and what prompt it sent, hermetically.
type recProvider struct {
	text  string
	err   error
	calls int
	reqs  []llm.Request
}

func (p *recProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	p.calls++
	p.reqs = append(p.reqs, req)
	if p.err != nil {
		return llm.Response{}, p.err
	}
	return llm.Response{Text: p.text, Model: "claude-sonnet-5"}, nil
}

func (p *recProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return &stubStream{}, nil
}

// recFactory records the answer.Settings it was asked to build a provider for, so a
// test can assert the settings.rewrite.model override is threaded through.
type recFactory struct {
	p   llm.Provider
	got []answer.Settings
}

func (f *recFactory) Provider(s answer.Settings) (llm.Provider, error) {
	f.got = append(f.got, s)
	return f.p, nil
}

// --- Fixtures -------------------------------------------------------------

// rewriteDoc is settingsDoc() plus a rewrite object with the given toggle/model.
func rewriteDoc(enabled bool, model string) map[string]any {
	d := settingsDoc()
	rw := map[string]any{"enabled": enabled}
	if model != "" {
		rw["model"] = model
	}
	d["rewrite"] = rw
	return d
}

var multiTurn = []answer.Turn{
	{Role: "user", Content: "How do I reset the X200?"},
	{Role: "assistant", Content: "Hold the reset button for ten seconds."},
}

const (
	followUp   = "What about the X300?"
	standalone = "How do I reset the X300?"
)

// rewriteService wires a Service with a rewrite provider factory (rwf) and a separate
// answer provider (ans), so rewrite LLM calls and answer LLM calls are counted apart.
func rewriteService(ret *fakeRetriever, doc map[string]any, ans llm.Provider, rwf answer.ProviderFactory) *Service {
	return &Service{
		Retrieve:  ret,
		Answer:    &answer.Service{Providers: stubFactory{p: ans}},
		Settings:  fakeSettings{doc: doc},
		Names:     fakeNames{name: "Acme"},
		Providers: rwf,
	}
}

func groundedResults() []retrieve.Result {
	return []retrieve.Result{result("c1", "d1", "the x300 reset procedure", 0.9)}
}

// --- Tests ----------------------------------------------------------------

// Multi-turn + toggle ON: the follow-up is rewritten into a standalone question with
// exactly ONE rewrite LLM call, retrieval runs on the STANDALONE question, and the
// answer stage still receives the ORIGINAL question + history (SPEC-06 §5).
func TestRewriteMultiTurnEnabledUsesStandaloneForRetrieval(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	ans := &recProvider{text: "Do this [1]."}
	rw := &recProvider{text: standalone}
	svc := rewriteService(ret, rewriteDoc(true, ""), ans, &recFactory{p: rw})

	res, err := svc.Query(context.Background(), testTenant, Request{Question: followUp, History: multiTurn})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !res.Grounded {
		t.Fatal("expected grounded answer")
	}
	if rw.calls != 1 {
		t.Fatalf("rewrite LLM calls = %d, want exactly 1", rw.calls)
	}
	if ret.lastReq.Query != standalone {
		t.Fatalf("retrieval query = %q, want the standalone %q", ret.lastReq.Query, standalone)
	}
	// The answer stage gets the ORIGINAL question (+ history), not the standalone.
	if len(ans.reqs) != 1 {
		t.Fatalf("answer LLM calls = %d, want 1", len(ans.reqs))
	}
	last := ans.reqs[0].Messages[len(ans.reqs[0].Messages)-1].Content
	if !strings.Contains(last, followUp) {
		t.Fatalf("answer prompt should carry the original question %q; got %q", followUp, last)
	}
	if strings.Contains(last, standalone) {
		t.Fatalf("answer prompt must NOT be the rewritten question; got %q", last)
	}
	// The rewrite prompt was fed the conversation history.
	rwPrompt := rw.reqs[0].Messages[0].Content
	if !strings.Contains(rwPrompt, "Hold the reset button") {
		t.Fatalf("rewrite prompt should include history; got %q", rwPrompt)
	}
}

// Single-turn (no history) + toggle ON: STRICT passthrough — ZERO rewrite calls and
// retrieval sees the original question unchanged (the AC's single-turn no-regression).
func TestRewriteSingleTurnMakesZeroRewriteCalls(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	rw := &recProvider{text: standalone}
	svc := rewriteService(ret, rewriteDoc(true, ""), &recProvider{text: "ans [1]"}, &recFactory{p: rw})

	if _, err := svc.Query(context.Background(), testTenant, Request{Question: followUp}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if rw.calls != 0 {
		t.Fatalf("single-turn rewrite calls = %d, want 0 (strict passthrough)", rw.calls)
	}
	if ret.lastReq.Query != followUp {
		t.Fatalf("retrieval query = %q, want the original %q", ret.lastReq.Query, followUp)
	}
}

// Toggle OFF (with history present): STRICT passthrough — ZERO rewrite calls, original
// question retrieved. rewrite defaults OFF (opt-in, conservative — matches reranker).
func TestRewriteToggleOffMakesZeroRewriteCalls(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	rw := &recProvider{text: standalone}
	svc := rewriteService(ret, rewriteDoc(false, ""), &recProvider{text: "ans [1]"}, &recFactory{p: rw})

	if _, err := svc.Query(context.Background(), testTenant, Request{Question: followUp, History: multiTurn}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if rw.calls != 0 {
		t.Fatalf("toggle-off rewrite calls = %d, want 0", rw.calls)
	}
	if ret.lastReq.Query != followUp {
		t.Fatalf("retrieval query = %q, want the original %q", ret.lastReq.Query, followUp)
	}
}

// No rewrite provider factory wired (the 08.6 wiring): passthrough, never a panic.
func TestRewriteNilFactoryPassthrough(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	svc := rewriteService(ret, rewriteDoc(true, ""), &recProvider{text: "ans [1]"}, nil)

	if _, err := svc.Query(context.Background(), testTenant, Request{Question: followUp, History: multiTurn}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if ret.lastReq.Query != followUp {
		t.Fatalf("retrieval query = %q, want original with no factory", ret.lastReq.Query)
	}
}

// A rewrite provider error falls back to the ORIGINAL question; the query still
// succeeds (NFR-REL-04 — the rewrite never fails the query).
func TestRewriteErrorFallsBackToOriginal(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	rw := &recProvider{err: errors.New("boom")}
	svc := rewriteService(ret, rewriteDoc(true, ""), &recProvider{text: "ans [1]"}, &recFactory{p: rw})

	res, err := svc.Query(context.Background(), testTenant, Request{Question: followUp, History: multiTurn})
	if err != nil {
		t.Fatalf("Query must not fail on rewrite error: %v", err)
	}
	if !res.Grounded {
		t.Fatal("query should still produce a grounded answer")
	}
	if rw.calls != 1 {
		t.Fatalf("rewrite calls = %d, want 1 (attempted then fell back)", rw.calls)
	}
	if ret.lastReq.Query != followUp {
		t.Fatalf("retrieval query = %q, want original on rewrite failure", ret.lastReq.Query)
	}
}

// An empty / whitespace-only rewrite output falls back to the original question.
func TestRewriteEmptyOutputFallsBackToOriginal(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	rw := &recProvider{text: "   \n  "}
	svc := rewriteService(ret, rewriteDoc(true, ""), &recProvider{text: "ans [1]"}, &recFactory{p: rw})

	if _, err := svc.Query(context.Background(), testTenant, Request{Question: followUp, History: multiTurn}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if ret.lastReq.Query != followUp {
		t.Fatalf("retrieval query = %q, want original on empty rewrite", ret.lastReq.Query)
	}
}

// settings.rewrite.model overrides the model for the rewrite call only (a cheap
// model), threaded through the provider factory; the allowlist still applies inside
// the factory (fail-closed, not exercised by this fake).
func TestRewriteModelOverride(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	rwf := &recFactory{p: &recProvider{text: standalone}}
	svc := rewriteService(ret, rewriteDoc(true, "claude-haiku-4-5"), &recProvider{text: "ans [1]"}, rwf)

	if _, err := svc.Query(context.Background(), testTenant, Request{Question: followUp, History: multiTurn}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rwf.got) != 1 {
		t.Fatalf("rewrite provider built %d times, want 1", len(rwf.got))
	}
	if rwf.got[0].LLMModel != "claude-haiku-4-5" {
		t.Fatalf("rewrite model = %q, want the override claude-haiku-4-5", rwf.got[0].LLMModel)
	}
}

// parseRewritten strips code fences and surrounding quotes and trims; empty stays
// empty (so the caller falls back to the original question).
func TestParseRewritten(t *testing.T) {
	cases := map[string]string{
		"How do I reset the X300?":               "How do I reset the X300?",
		"  How do I reset the X300?  ":           "How do I reset the X300?",
		"\"How do I reset the X300?\"":           "How do I reset the X300?",
		"```\nHow do I reset the X300?\n```":     "How do I reset the X300?",
		"```text\nHow do I reset the X300?\n```": "How do I reset the X300?",
		"":                                       "",
		"   \n  ":                                "",
	}
	for in, want := range cases {
		if got := parseRewritten(in); got != want {
			t.Errorf("parseRewritten(%q) = %q, want %q", in, got, want)
		}
	}
}
