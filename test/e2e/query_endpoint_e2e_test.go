//go:build e2e

// STORY-08.6 golden path: the /v1/query endpoint (FR-RET-06, SPEC-06 §6, SPEC-07
// §2f) driven over the REAL public router (api.New) against a REAL enrolled tenant
// database (up via `mise run up`), reached ONLY through a resolver + *tenant.DB
// (ADR-0003, C-1, C-3). It exercises the full retrieve → answer pipeline with the
// real hybrid SQL, the real grounding floor, real settings + tenant-name loads.
//
// Two providers are external and therefore stubbed (never a real API call, no keys,
// no network): the embedding provider (a deterministic dim-8 embedder, as the retrieve
// e2e does) and the LLM (a canned Complete + Stream). Everything else is real. It
// proves the endpoint:
//   - JSON mode: returns a grounded answer with [n] citations and usage,
//   - SSE mode: emits retrieval (citations FIRST) → delta (text) → done (usage),
//     with the citations arriving before any text,
//   - grounding refusal: below-floor (here, a filter that matches nothing) yields
//     grounded=false with the fixed refusal and NO generation.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/cp/usage"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/query"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// queryStubLLM is a hermetic llm.Provider: canned Complete text with a [1] marker
// and a canned Stream (text deltas + a terminal Done with usage). It never touches a
// network or a real key.
type queryStubLLM struct{}

func (queryStubLLM) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{
		Text:  "Hold the reset button for ten seconds [1].",
		Usage: llm.Usage{InputTokens: 128, OutputTokens: 12},
		Model: "claude-sonnet-5",
	}, nil
}

func (queryStubLLM) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return &queryStubStream{events: []llm.Event{
		{TextDelta: "Hold the reset "},
		{TextDelta: "button [1]."},
		{Done: true, Usage: llm.Usage{InputTokens: 128, OutputTokens: 8}, FinishReason: "stop"},
	}}, nil
}

type queryStubStream struct {
	events []llm.Event
	i      int
}

func (s *queryStubStream) Recv() (llm.Event, error) {
	if s.i >= len(s.events) {
		return llm.Event{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}
func (s *queryStubStream) Close() error { return nil }

type queryStubLLMFactory struct{}

func (queryStubLLMFactory) Provider(answer.Settings) (llm.Provider, error) {
	return queryStubLLM{}, nil
}

// recordingRewriteLLM is the hermetic rewrite provider (STORY-08.7): it records each
// Complete call (count + the prompt it received) and returns a canned standalone
// question that still matches the seeded doc, so the rewritten query stays grounded.
type recordingRewriteLLM struct {
	calls   int
	prompts []string
}

func (r *recordingRewriteLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	r.calls++
	if n := len(req.Messages); n > 0 {
		r.prompts = append(r.prompts, req.Messages[n-1].Content)
	}
	// The standalone question's terms all appear in the seeded docA content ("How to
	// reset the X200 device"), so the full-text CTE matches and the rewritten query
	// clears the grounding floor — unlike the context-dependent original follow-up.
	return llm.Response{Text: "reset the X200 device", Model: "claude-sonnet-5"}, nil
}

func (r *recordingRewriteLLM) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return &queryStubStream{}, nil
}

type recordingRewriteFactory struct{ p llm.Provider }

func (f recordingRewriteFactory) Provider(answer.Settings) (llm.Provider, error) { return f.p, nil }

func TestQueryEndpointGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const dim = 8
	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "queryapi-" + suffix
	tenantDisplay := "Query API " + suffix
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		var dbName, role string
		_ = pool.QueryRow(cctx,
			`select d.database_name, d.username from tenant_databases d
			 join tenants t on t.id = d.tenant_id where t.slug = $1`, slug).Scan(&dbName, &role)
		if dbName != "" {
			_, _ = pool.Exec(cctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		}
		if role != "" {
			_, _ = pool.Exec(cctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
		}
		_, _ = pool.Exec(cctx, `DELETE FROM tenants WHERE slug = $1`, slug)
	})
	if out, exit := runEnroll(t, ageKey, blob, slug, tenantDisplay, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `select t.id::text from tenants t where t.slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}

	sourceDocs := seedSource(ctx, t, pool, tenantID, "docs")

	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher, CacheTTL: 50 * time.Millisecond})
	db, err := resolver.Open(ctx, tenant.ID(uuid.MustParse(tenantID)))
	if err != nil {
		t.Fatalf("resolver.Open: %v", err)
	}
	store := documents.NewTenantStore()

	vec := func(v ...float32) []float32 {
		out := make([]float32, dim)
		copy(out, v)
		return out
	}
	const model = "text-embedding-3-small"

	// docA is rank 1 in BOTH the vector list (embedding == query vector) and the
	// full-text list (its text carries the query terms), so its fused RRF score clears
	// the default 0.02 grounding floor — the query is grounded.
	docA := putDoc(ctx, t, store, db, putSpec{
		src: sourceDocs, ext: "a", title: "X200 Reset Guide",
		uri: "https://docs.acme.com/x200", meta: `{"team":"support"}`,
		content: "How to reset the X200 device", emb: vec(1, 0, 0, 0, 0, 0, 0, 0), model: model,
	})

	// --- Assemble the REAL router with the real query + retrieve + answer wiring;
	// only the two external providers (embedding, LLM) are stubbed. ---
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	retrieveSvc := retrieve.NewService(resolver, settingsSvc, retrieveStubFactory{vec: vec(1, 0, 0, 0, 0, 0, 0, 0)})
	counter := usage.NewCounter(nil)
	rewriteStub := &recordingRewriteLLM{}
	querySvc := &query.Service{
		Retrieve:  retrieveSvc,
		Answer:    &answer.Service{Providers: queryStubLLMFactory{}, Usage: counter},
		Settings:  settingsSvc,
		Names:     tenants.NewNameService(tenants.SettingsFromPool(pool)),
		Usage:     counter,
		Providers: recordingRewriteFactory{p: rewriteStub},
	}
	h := query.NewHandlers(querySvc)

	deps := api.Deps{
		Log:               obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:           obs.NewMetrics(),
		RequireScopeQuery: injectTenantMW(tenantID),
		Query:             http.HandlerFunc(h.Query),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()
	client := srv.Client()

	// --- JSON mode: grounded answer + [1] citation to docA + usage. ---
	jsonBody := postQueryJSON(t, client, srv.URL, `{"question":"reset the X200 device","stream":false}`)
	if !jsonBody.Grounded {
		t.Fatalf("expected grounded=true, got body %+v", jsonBody)
	}
	if jsonBody.ID == "" || jsonBody.Answer == "" {
		t.Fatalf("missing id/answer: %+v", jsonBody)
	}
	if len(jsonBody.Citations) != 1 || jsonBody.Citations[0].N != 1 || jsonBody.Citations[0].DocumentID != docA {
		t.Fatalf("citations = %+v, want a single n=1 → docA %s", jsonBody.Citations, docA)
	}
	if jsonBody.Citations[0].URI != "https://docs.acme.com/x200" || jsonBody.Citations[0].Title != "X200 Reset Guide" {
		t.Fatalf("citation metadata mismapped: %+v", jsonBody.Citations[0])
	}
	if jsonBody.Usage.InTokens != 128 || jsonBody.Usage.OutTokens != 12 {
		t.Fatalf("usage = %+v, want in=128 out=12", jsonBody.Usage)
	}
	if jsonBody.Model != "claude-sonnet-5" {
		t.Fatalf("model = %q", jsonBody.Model)
	}

	// --- SSE mode: retrieval (citations first) → delta (text) → done (usage). ---
	events := postQuerySSE(t, client, srv.URL, `{"question":"reset the X200 device","stream":true}`)
	if len(events) < 3 {
		t.Fatalf("expected >=3 SSE events, got %d: %v", len(events), events)
	}
	if events[0].name != "retrieval" {
		t.Fatalf("first SSE event = %q, want retrieval (citations first)", events[0].name)
	}
	if events[len(events)-1].name != "done" {
		t.Fatalf("last SSE event = %q, want done", events[len(events)-1].name)
	}
	var rv struct {
		Citations []answer.Citation `json:"citations"`
	}
	if err := json.Unmarshal([]byte(events[0].data), &rv); err != nil {
		t.Fatalf("decode retrieval event: %v", err)
	}
	if len(rv.Citations) != 1 || rv.Citations[0].DocumentID != docA {
		t.Fatalf("retrieval citations = %+v, want candidate for docA %s", rv.Citations, docA)
	}
	// Citations must precede all text (the AC): no delta before the retrieval event.
	sawRetrieval := false
	var text string
	for _, e := range events {
		switch e.name {
		case "retrieval":
			sawRetrieval = true
		case "delta":
			if !sawRetrieval {
				t.Fatal("a delta event preceded retrieval; citations must come first")
			}
			var d struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(e.data), &d)
			text += d.Text
		}
	}
	if text != "Hold the reset button [1]." {
		t.Fatalf("streamed text = %q", text)
	}
	var dv struct {
		Grounded bool `json:"grounded"`
		Usage    struct {
			InTokens  int `json:"in_tokens"`
			OutTokens int `json:"out_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(events[len(events)-1].data), &dv); err != nil {
		t.Fatalf("decode done event: %v", err)
	}
	if !dv.Grounded || dv.Usage.InTokens != 128 || dv.Usage.OutTokens != 8 {
		t.Fatalf("done event = %+v, want grounded + usage in=128 out=8", dv)
	}

	// --- Grounding refusal (SPEC-06 §4): a source filter that matches nothing yields
	// zero chunks → grounded=false, the fixed refusal (with the tenant name), no
	// generation. ---
	noSource := uuid.NewString()
	refusalBody := postQueryJSON(t, client, srv.URL,
		fmt.Sprintf(`{"question":"reset the X200 device","filters":{"source_ids":[%q]}}`, noSource))
	if refusalBody.Grounded {
		t.Fatalf("expected grounded=false for a no-match filter, got %+v", refusalBody)
	}
	wantRefusal := fmt.Sprintf("I couldn't find information about that in %s's content.", tenantDisplay)
	if refusalBody.Answer != wantRefusal {
		t.Fatalf("refusal = %q, want %q", refusalBody.Answer, wantRefusal)
	}
	if len(refusalBody.Citations) != 0 {
		t.Fatalf("refusal must have zero citations, got %d", len(refusalBody.Citations))
	}

	// --- Question rewrite (STORY-08.7, FR-RET-07). rewrite defaults OFF, so every
	// query above was a strict passthrough: ZERO rewrite calls (the AC's single-turn
	// no-regression, proven end-to-end). Enabling settings.rewrite.enabled and sending
	// a MULTI-TURN query makes exactly one rewrite LLM call fed the conversation
	// history, and the rewritten query is still grounded. ---
	if rewriteStub.calls != 0 {
		t.Fatalf("rewrite made %d calls with the toggle off; want 0 (single-turn passthrough)", rewriteStub.calls)
	}
	if _, err := settingsSvc.Patch(ctx, tenants.PatchParams{
		TenantID: tenantID,
		Patch:    map[string]any{"rewrite": map[string]any{"enabled": true}},
	}); err != nil {
		t.Fatalf("enable rewrite: %v", err)
	}
	mtBody := postQueryJSON(t, client, srv.URL,
		`{"question":"what about resetting it?","history":[{"role":"user","content":"Tell me about the X200 device"},{"role":"assistant","content":"The X200 is a router."}],"stream":false}`)
	if !mtBody.Grounded {
		t.Fatalf("multi-turn rewritten query should be grounded, got %+v", mtBody)
	}
	if rewriteStub.calls != 1 {
		t.Fatalf("rewrite calls = %d after one multi-turn query, want exactly 1", rewriteStub.calls)
	}
	if len(rewriteStub.prompts) != 1 || !strings.Contains(rewriteStub.prompts[0], "X200 is a router") {
		t.Fatalf("rewrite prompt should carry the conversation history; got %q", rewriteStub.prompts)
	}
}

// queryResult mirrors the SPEC-06 §6 JSON body.
type queryResult struct {
	ID        string            `json:"id"`
	Answer    string            `json:"answer"`
	Grounded  bool              `json:"grounded"`
	Citations []answer.Citation `json:"citations"`
	Usage     struct {
		RetrievalMs  int64 `json:"retrieval_ms"`
		GenerationMs int64 `json:"generation_ms"`
		InTokens     int   `json:"in_tokens"`
		OutTokens    int   `json:"out_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
}

func postQueryJSON(t *testing.T, client *http.Client, base, body string) queryResult {
	t.Helper()
	resp, err := client.Post(base+"/v1/query", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/query: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/query = %d, want 200; body=%s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var out queryResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /v1/query response: %v", err)
	}
	return out
}

type sseFrame struct {
	name string
	data string
}

func postQuerySSE(t *testing.T, client *http.Client, base, body string) []sseFrame {
	t.Helper()
	resp, err := client.Post(base+"/v1/query", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/query (sse): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/query (sse) = %d, want 200; body=%s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	var frames []sseFrame
	sc := bufio.NewScanner(resp.Body)
	var cur sseFrame
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			cur.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if cur.name != "" {
				frames = append(frames, cur)
				cur = sseFrame{}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read SSE stream: %v", err)
	}
	return frames
}
