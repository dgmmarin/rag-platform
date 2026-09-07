package query

import (
	"context"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/cp/usage"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// --- Test doubles ---------------------------------------------------------

// fakeRetriever returns canned ranked chunks, standing in for the real hybrid
// retrieval + rerank pipeline (STORY-08.1/08.3) so the query orchestration is
// tested without a database.
type fakeRetriever struct {
	results []retrieve.Result
	err     error
	lastReq retrieve.Request
	calls   int
}

func (f *fakeRetriever) Search(_ context.Context, _ tenant.ID, req retrieve.Request) ([]retrieve.Result, error) {
	f.calls++
	f.lastReq = req
	return f.results, f.err
}

// stubStream is a hermetic llm.Stream: it yields the canned events then io.EOF.
type stubStream struct {
	events []llm.Event
	i      int
	closed bool
}

func (s *stubStream) Recv() (llm.Event, error) {
	if s.i >= len(s.events) {
		return llm.Event{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}
func (s *stubStream) Close() error { s.closed = true; return nil }

// stubProvider is a hermetic llm.Provider supporting both Complete and Stream.
type stubProvider struct {
	completeResp  llm.Response
	completeErr   error
	streamEvents  []llm.Event
	streamErr     error
	completeCalls int
	streamCalls   int
}

func (p *stubProvider) Complete(context.Context, llm.Request) (llm.Response, error) {
	p.completeCalls++
	return p.completeResp, p.completeErr
}
func (p *stubProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	p.streamCalls++
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	return &stubStream{events: p.streamEvents}, nil
}

type stubFactory struct{ p llm.Provider }

func (f stubFactory) Provider(answer.Settings) (llm.Provider, error) { return f.p, nil }

type fakeSettings struct{ doc map[string]any }

func (f fakeSettings) Get(context.Context, string) (map[string]any, error) { return f.doc, nil }

type fakeNames struct{ name string }

func (f fakeNames) Name(context.Context, string) (string, error) { return f.name, nil }

// fakeUsage records usage deltas so a test can assert the Queries counter and the
// folded LLM tokens. It satisfies both answer's and query's UsageRecorder seams.
type fakeUsage struct {
	deltas  []usage.Delta
	tenants []string
}

func (f *fakeUsage) Add(tenantID string, d usage.Delta) {
	f.deltas = append(f.deltas, d)
	f.tenants = append(f.tenants, tenantID)
}

func (f *fakeUsage) totals() (queries, llmIn, llmOut int64) {
	for _, d := range f.deltas {
		queries += d.Queries
		llmIn += d.LLMInTokens
		llmOut += d.LLMOutTokens
	}
	return
}

// recSink records emitted SSE events in order, so a test can assert the wire order
// (retrieval → delta → done) and the payloads without an HTTP round trip.
type recSink struct{ events []recEvent }

type recEvent struct {
	name string
	data any
}

func (s *recSink) Send(name string, data any) error {
	s.events = append(s.events, recEvent{name: name, data: data})
	return nil
}

func (s *recSink) names() []string {
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = e.name
	}
	return out
}

// --- Fixtures -------------------------------------------------------------

func settingsDoc() map[string]any {
	return map[string]any{
		"llm": map[string]any{
			"provider": "anthropic", "model": "claude-sonnet-5",
			"max_tokens": float64(1024), "models_allowed": []any{"claude-sonnet-5"},
		},
		"retrieval":         map[string]any{"min_score": 0.02, "final_k": float64(8)},
		"answering":         map[string]any{"token_budget": float64(6000), "history_n": float64(6)},
		"providers_allowed": []any{"anthropic"},
	}
}

func result(id, doc, content string, score float64) retrieve.Result {
	return retrieve.Result{
		ChunkID: id, DocumentID: doc, SourceID: "src", Content: content,
		URI: "https://docs.example.com/" + doc, Title: "Doc " + doc,
		HeadingPath: []string{"Guide"}, Score: score,
	}
}

func newService(ret *fakeRetriever, prov llm.Provider, rec *fakeUsage) *Service {
	return &Service{
		Retrieve: ret,
		Answer:   &answer.Service{Providers: stubFactory{p: prov}, Usage: rec},
		Settings: fakeSettings{doc: settingsDoc()},
		Names:    fakeNames{name: "Acme"},
		Usage:    rec,
	}
}

var testTenant = tenant.ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))

// --- JSON mode ------------------------------------------------------------

func TestQueryJSONReturnsGroundedAnswer(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{
		result("c1", "d1", "how to reset the x200", 0.9),
		result("c2", "d2", "unrelated", 0.5),
	}}
	prov := &stubProvider{completeResp: llm.Response{
		Text:  "Press and hold reset [1].",
		Usage: llm.Usage{InputTokens: 300, OutputTokens: 20},
		Model: "claude-sonnet-5",
	}}
	rec := &fakeUsage{}
	svc := newService(ret, prov, rec)

	res, err := svc.Query(context.Background(), testTenant, Request{Question: "reset x200?"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !res.Grounded {
		t.Fatal("expected grounded=true")
	}
	if res.Answer != "Press and hold reset [1]." {
		t.Fatalf("answer = %q", res.Answer)
	}
	// JSON keeps only the referenced citation (unreferenced [2] dropped).
	if len(res.Citations) != 1 || res.Citations[0].N != 1 || res.Citations[0].DocumentID != "d1" {
		t.Fatalf("citations = %+v, want only n=1 d1", res.Citations)
	}
	if res.Usage.InTokens != 300 || res.Usage.OutTokens != 20 {
		t.Fatalf("usage = %+v, want in=300 out=20", res.Usage)
	}
	if res.Model != "claude-sonnet-5" {
		t.Fatalf("model = %q", res.Model)
	}
}

func TestQueryJSONBelowFloorRefusesWithoutLLM(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{result("c1", "d1", "x", 0.001)}}
	prov := &stubProvider{}
	svc := newService(ret, prov, &fakeUsage{})

	res, err := svc.Query(context.Background(), testTenant, Request{Question: "reset?"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Grounded {
		t.Fatal("expected grounded=false")
	}
	if res.Answer != "I couldn't find information about that in Acme's content." {
		t.Fatalf("refusal = %q", res.Answer)
	}
	if len(res.Citations) != 0 {
		t.Fatalf("expected zero citations, got %d", len(res.Citations))
	}
	if prov.completeCalls != 0 {
		t.Fatalf("LLM Complete called %d times on refusal; want 0", prov.completeCalls)
	}
}

// The Queries counter is incremented here (08.6), exactly once — 08.5 deliberately
// leaves it to avoid a double count.
func TestQueryIncrementsQueriesCounterOnce(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{result("c1", "d1", "x", 0.9)}}
	prov := &stubProvider{completeResp: llm.Response{Text: "ans [1]", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}}
	rec := &fakeUsage{}
	svc := newService(ret, prov, rec)

	if _, err := svc.Query(context.Background(), testTenant, Request{Question: "q"}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	queries, llmIn, _ := rec.totals()
	if queries != 1 {
		t.Fatalf("Queries counter = %d, want exactly 1", queries)
	}
	if llmIn != 10 {
		t.Fatalf("LLMInTokens = %d, want 10 (folded by answer)", llmIn)
	}
}

// Graceful degradation (NFR-REL-04): an ErrCircuitOpen from the provider degrades to
// retrieval-only (200 with candidate citations + a generation-unavailable message),
// never a hard error.
func TestQueryJSONDegradesOnCircuitOpen(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{result("c1", "d1", "x200 reset steps", 0.9)}}
	prov := &stubProvider{completeErr: llm.ErrCircuitOpen}
	rec := &fakeUsage{}
	svc := newService(ret, prov, rec)

	res, err := svc.Query(context.Background(), testTenant, Request{Question: "reset?"})
	if err != nil {
		t.Fatalf("Query must not hard-fail on ErrCircuitOpen: %v", err)
	}
	if !res.Grounded {
		t.Fatal("degraded result should stay grounded (relevant content was found)")
	}
	if len(res.Citations) != 1 || res.Citations[0].DocumentID != "d1" {
		t.Fatalf("degraded result should carry candidate citations, got %+v", res.Citations)
	}
	if res.Answer == "" {
		t.Fatal("degraded result should carry a generation-unavailable message")
	}
}

// --- SSE mode -------------------------------------------------------------

func TestQueryStreamEmitsRetrievalThenDeltaThenDone(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{
		result("c1", "d1", "reset the x200", 0.9),
		result("c2", "d2", "more", 0.8),
	}}
	prov := &stubProvider{streamEvents: []llm.Event{
		{TextDelta: "Press "},
		{TextDelta: "reset [1]."},
		{Done: true, Usage: llm.Usage{InputTokens: 300, OutputTokens: 12}, FinishReason: "stop"},
	}}
	rec := &fakeUsage{}
	svc := newService(ret, prov, rec)

	sink := &recSink{}
	if err := svc.QueryStream(context.Background(), testTenant, Request{Question: "reset?"}, sink); err != nil {
		t.Fatalf("QueryStream: %v", err)
	}

	names := sink.names()
	if len(names) < 3 {
		t.Fatalf("expected at least retrieval+delta+done, got %v", names)
	}
	// Citations-before-text: the first event is retrieval, and every delta follows it.
	if names[0] != "retrieval" {
		t.Fatalf("first event = %q, want retrieval (citations first)", names[0])
	}
	if names[len(names)-1] != "done" {
		t.Fatalf("last event = %q, want done", names[len(names)-1])
	}
	firstDelta := indexOf(names, "delta")
	if firstDelta <= 0 {
		t.Fatalf("no delta event after retrieval: %v", names)
	}

	// The retrieval event carries candidate citations for every context chunk.
	rv, ok := sink.events[0].data.(retrievalEvent)
	if !ok {
		t.Fatalf("retrieval event data type = %T", sink.events[0].data)
	}
	if len(rv.Citations) != 2 {
		t.Fatalf("retrieval citations = %d, want 2 (candidates up front)", len(rv.Citations))
	}
	if rv.Citations[0].N != 1 || rv.Citations[1].N != 2 {
		t.Fatalf("candidate citations must be numbered 1..N: %+v", rv.Citations)
	}

	// The done event carries usage (AC: "usage in done").
	dv := sink.events[len(sink.events)-1].data.(doneEvent)
	if dv.Usage.InTokens != 300 || dv.Usage.OutTokens != 12 {
		t.Fatalf("done usage = %+v, want in=300 out=12", dv.Usage)
	}
	if !dv.Grounded {
		t.Fatal("done.grounded should be true")
	}

	// The streamed text is reassembled from the delta events.
	var text string
	for _, e := range sink.events {
		if d, ok := e.data.(deltaEvent); ok {
			text += d.Text
		}
	}
	if text != "Press reset [1]." {
		t.Fatalf("reassembled text = %q", text)
	}

	// Queries counter incremented once; LLM usage folded from the done event.
	queries, _, llmOut := rec.totals()
	if queries != 1 {
		t.Fatalf("Queries = %d, want 1", queries)
	}
	if llmOut != 12 {
		t.Fatalf("LLMOutTokens = %d, want 12 (folded from stream done)", llmOut)
	}
}

func TestQueryStreamBelowFloorRefusesWithoutStreamCall(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{result("c1", "d1", "x", 0.001)}}
	prov := &stubProvider{}
	svc := newService(ret, prov, &fakeUsage{})

	sink := &recSink{}
	if err := svc.QueryStream(context.Background(), testTenant, Request{Question: "q"}, sink); err != nil {
		t.Fatalf("QueryStream: %v", err)
	}
	if prov.streamCalls != 0 {
		t.Fatalf("Stream called %d times on refusal; want 0", prov.streamCalls)
	}
	names := sink.names()
	// retrieval (empty citations) → delta (refusal message) → done.
	if len(names) != 3 || names[0] != "retrieval" || names[1] != "delta" || names[2] != "done" {
		t.Fatalf("refusal SSE order = %v, want [retrieval delta done]", names)
	}
	rv := sink.events[0].data.(retrievalEvent)
	if len(rv.Citations) != 0 {
		t.Fatalf("refusal retrieval event should carry zero citations, got %d", len(rv.Citations))
	}
	dv := sink.events[1].data.(deltaEvent)
	if dv.Text != "I couldn't find information about that in Acme's content." {
		t.Fatalf("refusal delta = %q", dv.Text)
	}
	done := sink.events[2].data.(doneEvent)
	if done.Grounded {
		t.Fatal("refusal done.grounded should be false")
	}
}

func TestQueryStreamDegradesOnCircuitOpen(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{result("c1", "d1", "x200 reset", 0.9)}}
	prov := &stubProvider{streamErr: llm.ErrCircuitOpen}
	svc := newService(ret, prov, &fakeUsage{})

	sink := &recSink{}
	if err := svc.QueryStream(context.Background(), testTenant, Request{Question: "q"}, sink); err != nil {
		t.Fatalf("QueryStream must not hard-fail on ErrCircuitOpen: %v", err)
	}
	names := sink.names()
	// retrieval (candidates) → delta (unavailable) → done (generation_unavailable).
	if names[0] != "retrieval" || names[len(names)-1] != "done" {
		t.Fatalf("degraded SSE order = %v", names)
	}
	rv := sink.events[0].data.(retrievalEvent)
	if len(rv.Citations) != 1 {
		t.Fatalf("degraded retrieval should still carry candidate citations, got %d", len(rv.Citations))
	}
	done := sink.events[len(sink.events)-1].data.(doneEvent)
	if !done.GenerationUnavailable {
		t.Fatal("degraded done should signal generation_unavailable")
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
