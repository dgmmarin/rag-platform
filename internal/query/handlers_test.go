package query

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// withTenant returns a request whose context carries the resolved tenant, standing
// in for the query-scope middleware (FR-ACC-03).
func withTenant(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(body))
	return r.WithContext(tenant.WithTenantID(context.Background(), testTenant))
}

func TestHandlerJSONMode(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{result("c1", "d1", "reset the x200", 0.9)}}
	prov := &stubProvider{completeResp: llm.Response{
		Text:  "Hold reset [1].",
		Usage: llm.Usage{InputTokens: 200, OutputTokens: 15},
		Model: "claude-sonnet-5",
	}}
	h := NewHandlers(newService(ret, prov, &fakeUsage{}))

	rec := httptest.NewRecorder()
	h.Query(rec, withTenant(`{"question":"reset?","stream":false}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var got answer.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rec.Body.String())
	}
	if !got.Grounded || got.Answer != "Hold reset [1]." {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got.ID == "" {
		t.Fatal("expected an id in the JSON response")
	}
	if len(got.Citations) != 1 || got.Citations[0].N != 1 {
		t.Fatalf("citations = %+v, want n=1", got.Citations)
	}
	if got.Usage.InTokens != 200 || got.Usage.OutTokens != 15 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

// sseEvent is one parsed SSE frame.
type sseEvent struct {
	name string
	data string
}

// parseSSE parses an event-stream body into ordered frames.
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(strings.NewReader(body))
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			cur.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if cur.name != "" {
				out = append(out, cur)
				cur = sseEvent{}
			}
		}
	}
	return out
}

func TestHandlerSSEModeOrderAndCitationsBeforeText(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{
		result("c1", "d1", "reset the x200", 0.9),
		result("c2", "d2", "more detail", 0.8),
	}}
	prov := &stubProvider{streamEvents: []llm.Event{
		{TextDelta: "Hold "},
		{TextDelta: "reset [1]."},
		{Done: true, Usage: llm.Usage{InputTokens: 200, OutputTokens: 9}, FinishReason: "stop"},
	}}
	h := NewHandlers(newService(ret, prov, &fakeUsage{}))

	rec := httptest.NewRecorder()
	h.Query(rec, withTenant(`{"question":"reset?","stream":true}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) < 3 {
		t.Fatalf("expected >=3 events, got %d: %v", len(events), events)
	}
	if events[0].name != "retrieval" {
		t.Fatalf("first event = %q, want retrieval (citations first)", events[0].name)
	}
	if events[len(events)-1].name != "done" {
		t.Fatalf("last event = %q, want done", events[len(events)-1].name)
	}
	// Citations-before-text: no delta may precede the retrieval event.
	for i, e := range events {
		if e.name == "delta" && i == 0 {
			t.Fatal("a delta event preceded retrieval; citations must come first")
		}
	}

	// retrieval carries the candidate citations for both context chunks.
	var rv retrievalEvent
	if err := json.Unmarshal([]byte(events[0].data), &rv); err != nil {
		t.Fatalf("decode retrieval: %v", err)
	}
	if len(rv.Citations) != 2 {
		t.Fatalf("retrieval citations = %d, want 2", len(rv.Citations))
	}

	// done carries usage.
	var dv doneEvent
	if err := json.Unmarshal([]byte(events[len(events)-1].data), &dv); err != nil {
		t.Fatalf("decode done: %v", err)
	}
	if dv.Usage.InTokens != 200 || dv.Usage.OutTokens != 9 {
		t.Fatalf("done usage = %+v, want in=200 out=9", dv.Usage)
	}
	if !dv.Grounded {
		t.Fatal("done.grounded should be true")
	}

	// The concatenated deltas reconstruct the answer.
	var text string
	for _, e := range events {
		if e.name == "delta" {
			var d deltaEvent
			_ = json.Unmarshal([]byte(e.data), &d)
			text += d.Text
		}
	}
	if text != "Hold reset [1]." {
		t.Fatalf("reassembled text = %q", text)
	}
}

func TestHandlerSSERefusal(t *testing.T) {
	ret := &fakeRetriever{results: []retrieve.Result{result("c1", "d1", "x", 0.001)}}
	h := NewHandlers(newService(ret, &stubProvider{}, &fakeUsage{}))

	rec := httptest.NewRecorder()
	h.Query(rec, withTenant(`{"question":"q","stream":true}`))

	events := parseSSE(t, rec.Body.String())
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.name
	}
	if len(names) != 3 || names[0] != "retrieval" || names[1] != "delta" || names[2] != "done" {
		t.Fatalf("refusal SSE order = %v, want [retrieval delta done]", names)
	}
}

func TestHandlerNoTenantIs401(t *testing.T) {
	h := NewHandlers(newService(&fakeRetriever{}, &stubProvider{}, &fakeUsage{}))
	rec := httptest.NewRecorder()
	// No tenant in context.
	r := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(`{"question":"q"}`))
	h.Query(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandlerEmptyQueryIs400(t *testing.T) {
	// The retrieve layer validates the query; the handler maps its ErrEmptyQuery to
	// a 400 envelope BEFORE any SSE headers are written.
	ret := &fakeRetriever{err: retrieve.ErrEmptyQuery}
	h := NewHandlers(newService(ret, &stubProvider{}, &fakeUsage{}))

	rec := httptest.NewRecorder()
	h.Query(rec, withTenant(`{"question":"  ","stream":true}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("empty-query error must be a JSON envelope, got content-type %q", ct)
	}
}

func TestHandlerTenantUnavailableIs503(t *testing.T) {
	ret := &fakeRetriever{err: retrieve.ErrTenantUnavailable}
	h := NewHandlers(newService(ret, &stubProvider{}, &fakeUsage{}))
	rec := httptest.NewRecorder()
	h.Query(rec, withTenant(`{"question":"q"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandlerRejectsUnknownFields(t *testing.T) {
	h := NewHandlers(newService(&fakeRetriever{}, &stubProvider{}, &fakeUsage{}))
	rec := httptest.NewRecorder()
	h.Query(rec, withTenant(`{"question":"q","bogus":true}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field", rec.Code)
	}
}
