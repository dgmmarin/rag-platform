package query

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// expansionDoc is settingsDoc() plus an expansion object with the given mode/model.
func expansionDoc(mode, model string) map[string]any {
	d := settingsDoc()
	ex := map[string]any{"mode": mode}
	if model != "" {
		ex["model"] = model
	}
	d["expansion"] = ex
	return d
}

const hypo = "When a hotel has no allotment (sold out), you can still take the booking by creating a Waiting list (WL) booking."

// HyDE mode ON (single-turn, so rewrite is skipped): exactly ONE expansion LLM call
// drafts a hypothetical answer, retrieval EMBEDS that (EmbedText) while the full-text
// Query stays the user's question, and the answer stage still gets the question.
func TestHydeEnabledEmbedsHypotheticalKeepsQueryForText(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	ans := &recProvider{text: "Do this [1]."}
	hyde := &recProvider{text: hypo}
	svc := rewriteService(ret, expansionDoc("hyde", ""), ans, &recFactory{p: hyde})

	const q = "can I still sell an offer if I have 0 allotment on the hotel?"
	if _, err := svc.Query(context.Background(), testTenant, Request{Question: q}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if hyde.calls != 1 {
		t.Fatalf("hyde LLM calls = %d, want exactly 1", hyde.calls)
	}
	if ret.lastReq.EmbedText != hypo {
		t.Fatalf("EmbedText = %q, want the hypothetical answer", ret.lastReq.EmbedText)
	}
	if ret.lastReq.Query != q {
		t.Fatalf("full-text Query = %q, want the original question %q", ret.lastReq.Query, q)
	}
	// The hyde prompt was fed the user's question as data.
	if !strings.Contains(hyde.reqs[0].Messages[0].Content, "0 allotment") {
		t.Fatalf("hyde prompt should carry the question; got %q", hyde.reqs[0].Messages[0].Content)
	}
}

// Mode OFF (default): no expansion call, and retrieval embeds the query unchanged
// (EmbedText empty) — strict no-regression passthrough.
func TestExpansionOffIsPassthrough(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	hyde := &recProvider{text: hypo}
	svc := rewriteService(ret, expansionDoc("off", ""), &recProvider{text: "a [1]."}, &recFactory{p: hyde})

	if _, err := svc.Query(context.Background(), testTenant, Request{Question: "q"}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if hyde.calls != 0 {
		t.Fatalf("expansion off must make no LLM call, got %d", hyde.calls)
	}
	if ret.lastReq.EmbedText != "" {
		t.Fatalf("EmbedText = %q, want empty (embed the query)", ret.lastReq.EmbedText)
	}
}

// A HyDE provider failure degrades to embedding the question (EmbedText empty) and
// never fails the query (NFR-REL-04).
func TestHydeFailureFallsBackToQuery(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	hyde := &recProvider{err: errors.New("boom")}
	svc := rewriteService(ret, expansionDoc("hyde", ""), &recProvider{text: "a [1]."}, &recFactory{p: hyde})

	if _, err := svc.Query(context.Background(), testTenant, Request{Question: "q"}); err != nil {
		t.Fatalf("Query must not fail on hyde error: %v", err)
	}
	if ret.lastReq.EmbedText != "" {
		t.Fatalf("EmbedText = %q, want empty after hyde failure", ret.lastReq.EmbedText)
	}
}

// No provider factory wired: HyDE is a no-op even when mode=hyde.
func TestHydeNoFactoryIsPassthrough(t *testing.T) {
	ret := &fakeRetriever{results: groundedResults()}
	svc := rewriteService(ret, expansionDoc("hyde", ""), &recProvider{text: "a [1]."}, nil)
	svc.Providers = nil

	if _, err := svc.Query(context.Background(), testTenant, Request{Question: "q"}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if ret.lastReq.EmbedText != "" {
		t.Fatalf("EmbedText = %q, want empty with no factory", ret.lastReq.EmbedText)
	}
}
