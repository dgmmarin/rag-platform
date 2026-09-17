package answer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/cp/usage"
	"github.com/rag-platform/ragctl/internal/llm"
)

// fakeProvider is a hermetic llm.Provider: it records Complete calls (so a test can
// prove no LLM call was made on the refusal path) and returns a canned response.
type fakeProvider struct {
	resp    llm.Response
	err     error
	calls   int
	lastReq llm.Request
}

func (f *fakeProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.calls++
	f.lastReq = req
	return f.resp, f.err
}

func (f *fakeProvider) Stream(_ context.Context, _ llm.Request) (llm.Stream, error) {
	return nil, errors.New("stream not used in these tests")
}

// fakeFactory hands out a fixed provider and records how many times it was asked —
// so a test can prove the factory itself is untouched on the refusal path.
type fakeFactory struct {
	p     *fakeProvider
	err   error
	calls int
}

func (f *fakeFactory) Provider(_ Settings) (llm.Provider, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.p, nil
}

type fakeUsage struct {
	tenants []string
	deltas  []usage.Delta
}

func (f *fakeUsage) Add(tenantID string, d usage.Delta) {
	f.tenants = append(f.tenants, tenantID)
	f.deltas = append(f.deltas, d)
}

type fakeLogger struct {
	records []QueryRecord
}

func (f *fakeLogger) Log(_ context.Context, rec QueryRecord) {
	f.records = append(f.records, rec)
}

// baseChunk builds a chunk with a distinctive content marker.
func chunk(id, doc, marker string, score float64) Chunk {
	return Chunk{
		ChunkID:     id,
		DocumentID:  doc,
		Title:       "Doc " + doc,
		URI:         "https://docs.example.com/" + doc,
		HeadingPath: []string{"Guide", marker},
		Content:     "Content about " + marker + " with details.",
		Score:       score,
	}
}

func groundedSettings() Settings {
	return Settings{
		TenantName: "Acme",
		MinScore:   0.02,
		// The grounding floor applies only on reranker scores; these tests exercise
		// the floor, so mark the chunks as reranked (SPEC-06 §4).
		Reranked:         true,
		TokenBudget:      6000,
		HistoryN:         6,
		MaxTokens:        1024,
		LLMProvider:      "anthropic",
		LLMModel:         "claude-sonnet-5",
		ProvidersAllowed: []string{"anthropic"},
	}
}

// --- Grounding refusal (SPEC-06 §4) ---

// Without a reranker the score is the rank-based fused RRF value (a single-list
// hit tops out at ~0.016), so the absolute grounding floor must NOT apply — a low
// score is not "irrelevant", and flooring it makes retrieval keyword-only. The LLM
// is called and answers from the retrieved context (Settings.Reranked=false).
func TestBelowFloorPassesWithoutReranker(t *testing.T) {
	prov := &fakeProvider{resp: llm.Response{Text: "The answer is 42 [1]."}}
	svc := &Service{Providers: &fakeFactory{p: prov}}

	set := groundedSettings()
	set.Reranked = false // no reranker → RRF scores → floor disabled

	req := Request{
		TenantID: "t-1",
		Question: "How do I reset the X200?",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.01)}, // below the 0.02 floor
		Settings: set,
	}
	res, err := svc.Answer(context.Background(), req)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if !res.Grounded {
		t.Fatalf("without a reranker a low RRF score must not be floored; got a refusal: %q", res.Answer)
	}
}

func TestBelowFloorRefusesWithoutLLMCall(t *testing.T) {
	prov := &fakeProvider{resp: llm.Response{Text: "should never be returned"}}
	fac := &fakeFactory{p: prov}
	svc := &Service{Providers: fac}

	req := Request{
		TenantID: "t-1",
		Question: "How do I reset the X200?",
		// Both chunks score below the 0.02 floor.
		Chunks: []Chunk{
			chunk("c1", "d1", "alpha", 0.01),
			chunk("c2", "d2", "beta", 0.005),
		},
		Settings:    groundedSettings(),
		RetrievalMs: 42,
	}

	res, err := svc.Answer(context.Background(), req)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if res.Grounded {
		t.Fatalf("expected grounded=false, got true")
	}
	want := "I couldn't find information about that in Acme's content."
	if res.Answer != want {
		t.Fatalf("refusal message = %q, want %q", res.Answer, want)
	}
	if len(res.Citations) != 0 {
		t.Fatalf("expected zero citations, got %d", len(res.Citations))
	}
	if prov.calls != 0 {
		t.Fatalf("LLM Complete called %d times on refusal; want 0", prov.calls)
	}
	if fac.calls != 0 {
		t.Fatalf("provider factory called %d times on refusal; want 0", fac.calls)
	}
	if res.Usage.RetrievalMs != 42 {
		t.Fatalf("retrieval_ms = %d, want 42 (preserved)", res.Usage.RetrievalMs)
	}
}

func TestEmptyChunkListRefuses(t *testing.T) {
	prov := &fakeProvider{}
	svc := &Service{Providers: &fakeFactory{p: prov}}
	res, err := svc.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q", Chunks: nil, Settings: groundedSettings(),
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if res.Grounded || prov.calls != 0 {
		t.Fatalf("empty chunks must refuse without LLM call; grounded=%v calls=%d", res.Grounded, prov.calls)
	}
}

// --- Citation [n] mapping + unreferenced drop (SPEC-06 §5) ---

func TestCitationMappingDropsUnreferenced(t *testing.T) {
	prov := &fakeProvider{resp: llm.Response{
		Text:  "The reset is here [1]. See also the appendix [3].",
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 20},
		Model: "claude-sonnet-5",
	}}
	svc := &Service{Providers: &fakeFactory{p: prov}}

	req := Request{
		TenantID: "t-1",
		Question: "reset?",
		Chunks: []Chunk{
			chunk("c1", "d1", "alpha", 0.9),
			chunk("c2", "d2", "beta", 0.8), // never referenced -> dropped
			chunk("c3", "d3", "gamma", 0.7),
		},
		Settings: groundedSettings(),
	}
	res, err := svc.Answer(context.Background(), req)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if !res.Grounded {
		t.Fatal("expected grounded=true")
	}
	if len(res.Citations) != 2 {
		t.Fatalf("expected 2 citations (1 and 3), got %d: %+v", len(res.Citations), res.Citations)
	}
	// citation order by marker number; n preserved (not renumbered).
	if res.Citations[0].N != 1 || res.Citations[0].DocumentID != "d1" {
		t.Fatalf("citation[0] = {n:%d doc:%s}, want {n:1 doc:d1}", res.Citations[0].N, res.Citations[0].DocumentID)
	}
	if res.Citations[1].N != 3 || res.Citations[1].DocumentID != "d3" {
		t.Fatalf("citation[1] = {n:%d doc:%s}, want {n:3 doc:d3}", res.Citations[1].N, res.Citations[1].DocumentID)
	}
	// metadata carried through
	if res.Citations[0].URI != "https://docs.example.com/d1" || res.Citations[0].Title != "Doc d1" {
		t.Fatalf("citation[0] metadata not mapped: %+v", res.Citations[0])
	}
	if len(res.Citations[0].HeadingPath) == 0 || res.Citations[0].Snippet == "" {
		t.Fatalf("citation[0] missing heading_path/snippet: %+v", res.Citations[0])
	}
	// chunk d2 (n=2) never referenced
	for _, c := range res.Citations {
		if c.DocumentID == "d2" {
			t.Fatalf("unreferenced chunk d2 leaked into citations")
		}
	}
}

func TestOutOfRangeMarkerIgnored(t *testing.T) {
	prov := &fakeProvider{resp: llm.Response{Text: "Answer [1] and a bogus [9] marker."}}
	svc := &Service{Providers: &fakeFactory{p: prov}}
	res, err := svc.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		Settings: groundedSettings(),
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(res.Citations) != 1 || res.Citations[0].N != 1 {
		t.Fatalf("expected only citation n=1, got %+v", res.Citations)
	}
}

// --- Token budget (SPEC-06 §5) ---

func TestTokenBudgetExcludesOverflowChunks(t *testing.T) {
	// Three fat chunks; a small budget only fits the first two.
	big := strings.Repeat("word ", 80) // ~400 chars ~ 100 tokens per chunk block
	mk := func(id, doc, marker string) Chunk {
		c := chunk(id, doc, marker, 0.9)
		c.Content = big + marker // unique marker at the end so we can search for it
		return c
	}
	prov := &fakeProvider{resp: llm.Response{Text: "cite [1] [2] [3]"}}
	svc := &Service{Providers: &fakeFactory{p: prov}}

	st := groundedSettings()
	st.TokenBudget = 250 // fits two ~100-token blocks, not three

	res, err := svc.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{mk("c1", "d1", "ALPHAMARK"), mk("c2", "d2", "BETAMARK"), mk("c3", "d3", "GAMMAMARK")},
		Settings: st,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	// The third chunk's content must not appear in the assembled prompt.
	assembled := prov.lastReq.System
	for _, m := range prov.lastReq.Messages {
		assembled += "\n" + m.Content
	}
	if strings.Contains(assembled, "GAMMAMARK") {
		t.Fatalf("chunk 3 (GAMMAMARK) should have been excluded by the token budget")
	}
	if !strings.Contains(assembled, "ALPHAMARK") || !strings.Contains(assembled, "BETAMARK") {
		t.Fatalf("chunks 1 and 2 should be within budget and present")
	}
	// [3] refers to an excluded chunk -> no citation for it.
	for _, c := range res.Citations {
		if c.N == 3 {
			t.Fatalf("citation for excluded chunk 3 must be dropped")
		}
	}
}

// --- History (SPEC-06 §5: provided history included verbatim) ---

func TestHistoryIncludedInPrompt(t *testing.T) {
	prov := &fakeProvider{resp: llm.Response{Text: "ok [1]"}}
	svc := &Service{Providers: &fakeFactory{p: prov}}
	res, err := svc.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "what about the sequel?",
		Chunks: []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		History: []Turn{
			{Role: "user", Content: "PRIORUSERTURN"},
			{Role: "assistant", Content: "PRIORASSISTANTTURN"},
		},
		Settings: groundedSettings(),
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	_ = res
	var user, asst bool
	for _, m := range prov.lastReq.Messages {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "PRIORUSERTURN") {
			user = true
		}
		if m.Role == llm.RoleAssistant && strings.Contains(m.Content, "PRIORASSISTANTTURN") {
			asst = true
		}
	}
	if !user || !asst {
		t.Fatalf("history turns not included verbatim: user=%v assistant=%v msgs=%+v", user, asst, prov.lastReq.Messages)
	}
}

func TestHistoryTruncatedToLastN(t *testing.T) {
	prov := &fakeProvider{resp: llm.Response{Text: "ok [1]"}}
	svc := &Service{Providers: &fakeFactory{p: prov}}
	st := groundedSettings()
	st.HistoryN = 2
	// four turns provided; only the last two should survive.
	res, err := svc.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks: []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		History: []Turn{
			{Role: "user", Content: "OLDESTTURN"},
			{Role: "assistant", Content: "OLDREPLY"},
			{Role: "user", Content: "RECENTTURN"},
			{Role: "assistant", Content: "RECENTREPLY"},
		},
		Settings: st,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	_ = res
	var all string
	for _, m := range prov.lastReq.Messages {
		all += m.Content + "\n"
	}
	if strings.Contains(all, "OLDESTTURN") {
		t.Fatalf("history should be truncated to last N; OLDESTTURN leaked")
	}
	if !strings.Contains(all, "RECENTTURN") || !strings.Contains(all, "RECENTREPLY") {
		t.Fatalf("recent history missing after truncation")
	}
}

// --- System prompt content (SPEC-06 §5) ---

func TestSystemPromptInstructions(t *testing.T) {
	prov := &fakeProvider{resp: llm.Response{Text: "ok [1]"}}
	svc := &Service{Providers: &fakeFactory{p: prov}}
	_, err := svc.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		Settings: groundedSettings(),
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	sys := strings.ToLower(prov.lastReq.System)
	if !strings.Contains(prov.lastReq.System, "Acme") {
		t.Errorf("system prompt missing tenant name: %q", prov.lastReq.System)
	}
	if !strings.Contains(sys, "language") {
		t.Errorf("system prompt missing the language-match instruction: %q", prov.lastReq.System)
	}
	if !strings.Contains(sys, "[n]") {
		t.Errorf("system prompt missing the [n] citation instruction: %q", prov.lastReq.System)
	}
	if !strings.Contains(sys, "only") {
		t.Errorf("system prompt should instruct to answer only from the sources: %q", prov.lastReq.System)
	}
}

// --- Usage accounting (FR-RET-04, ADR-0024) ---

func TestUsageFoldedIntoCounterAndResponse(t *testing.T) {
	prov := &fakeProvider{resp: llm.Response{
		Text:  "answer [1]",
		Usage: llm.Usage{InputTokens: 3200, OutputTokens: 180},
		Model: "claude-sonnet-5",
	}}
	rec := &fakeUsage{}
	svc := &Service{Providers: &fakeFactory{p: prov}, Usage: rec}

	res, err := svc.Answer(context.Background(), Request{
		TenantID: "t-42", Question: "q",
		Chunks:      []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		Settings:    groundedSettings(),
		RetrievalMs: 120,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if res.Usage.InTokens != 3200 || res.Usage.OutTokens != 180 {
		t.Fatalf("response usage = %+v, want in=3200 out=180", res.Usage)
	}
	if res.Usage.RetrievalMs != 120 {
		t.Fatalf("retrieval_ms = %d, want 120", res.Usage.RetrievalMs)
	}
	if res.Model != "claude-sonnet-5" {
		t.Fatalf("model = %q, want claude-sonnet-5", res.Model)
	}
	if len(rec.deltas) != 1 {
		t.Fatalf("usage counter got %d deltas, want 1", len(rec.deltas))
	}
	if rec.tenants[0] != "t-42" {
		t.Fatalf("usage attributed to %q, want t-42", rec.tenants[0])
	}
	if rec.deltas[0].LLMInTokens != 3200 || rec.deltas[0].LLMOutTokens != 180 {
		t.Fatalf("usage delta = %+v, want LLMInTokens=3200 LLMOutTokens=180", rec.deltas[0])
	}
}

func TestRefusalRecordsNoLLMUsage(t *testing.T) {
	rec := &fakeUsage{}
	svc := &Service{Providers: &fakeFactory{p: &fakeProvider{}}, Usage: rec}
	_, err := svc.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.001)},
		Settings: groundedSettings(),
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rec.deltas) != 0 {
		t.Fatalf("refusal recorded %d usage deltas; want 0 (no LLM call)", len(rec.deltas))
	}
}

// --- Provider failure surfaces cleanly (NFR-REL-04) ---

func TestProviderErrorSurfaced(t *testing.T) {
	prov := &fakeProvider{err: llm.ErrCircuitOpen}
	svc := &Service{Providers: &fakeFactory{p: prov}}
	_, err := svc.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		Settings: groundedSettings(),
	})
	if err == nil {
		t.Fatal("expected an error when the provider fails")
	}
	if !errors.Is(err, llm.ErrCircuitOpen) {
		t.Fatalf("error should wrap llm.ErrCircuitOpen, got %v", err)
	}
}

// --- Logging seam for STORY-08.8 (both paths loggable) ---

func TestLoggingSeamCalledOnBothPaths(t *testing.T) {
	// Refusal path
	log1 := &fakeLogger{}
	svc1 := &Service{Providers: &fakeFactory{p: &fakeProvider{}}, Logger: log1}
	_, _ = svc1.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.001)},
		Settings: groundedSettings(),
	})
	if len(log1.records) != 1 || log1.records[0].Grounded {
		t.Fatalf("refusal path should log exactly one grounded=false record, got %+v", log1.records)
	}
	if len(log1.records[0].RetrievedChunkIDs) != 1 || log1.records[0].RetrievedChunkIDs[0] != "c1" {
		t.Fatalf("refusal log should carry retrieved chunk ids, got %+v", log1.records[0])
	}

	// Grounded path
	log2 := &fakeLogger{}
	svc2 := &Service{Providers: &fakeFactory{p: &fakeProvider{resp: llm.Response{Text: "a [1]"}}}, Logger: log2}
	_, _ = svc2.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		Settings: groundedSettings(),
	})
	if len(log2.records) != 1 || !log2.records[0].Grounded {
		t.Fatalf("grounded path should log exactly one grounded=true record, got %+v", log2.records)
	}
}

// --- Default token budget applied when unset ---

func TestDefaultsAppliedWhenUnset(t *testing.T) {
	prov := &fakeProvider{resp: llm.Response{Text: "ok [1]"}}
	svc := &Service{Providers: &fakeFactory{p: prov}}
	st := groundedSettings()
	st.TokenBudget = 0 // -> DefaultTokenBudget
	st.MaxTokens = 0   // -> DefaultMaxTokens
	_, err := svc.Answer(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		Settings: st,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if prov.lastReq.MaxTokens != DefaultMaxTokens {
		t.Fatalf("max_tokens = %d, want default %d", prov.lastReq.MaxTokens, DefaultMaxTokens)
	}
}
