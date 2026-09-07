package answer

import (
	"context"
	"testing"

	"github.com/rag-platform/ragctl/internal/llm"
)

// Prepare is the shared front half (grounding gate + prompt assembly) that both
// Answer (JSON) and the STORY-08.6 SSE path build on. These tests pin the parts
// SSE needs that Answer's Result does not expose: the built llm.Request, the
// candidate citations, and the no-provider refusal path.

func TestPrepareGroundedBuildsRequestAndCandidateCitations(t *testing.T) {
	prov := &fakeProvider{}
	fac := &fakeFactory{p: prov}
	svc := &Service{Providers: fac}

	req := Request{
		TenantID: "t-1",
		Question: "reset?",
		Chunks: []Chunk{
			chunk("c1", "d1", "alpha", 0.9),
			chunk("c2", "d2", "beta", 0.8),
		},
		Settings:    groundedSettings(),
		RetrievalMs: 55,
	}
	p, err := svc.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !p.Grounded {
		t.Fatal("expected grounded=true")
	}
	if p.ID == "" {
		t.Fatal("expected an id")
	}
	if p.RetrievalMs != 55 {
		t.Fatalf("retrieval_ms = %d, want 55", p.RetrievalMs)
	}
	if p.Provider == nil {
		t.Fatal("grounded Prepared must carry the built provider")
	}
	// The assembled request is what the SSE path streams: system + the sources turn.
	if p.LLMRequest.System == "" || len(p.LLMRequest.Messages) == 0 {
		t.Fatalf("LLMRequest not assembled: %+v", p.LLMRequest)
	}
	if p.LLMRequest.MaxTokens != 1024 {
		t.Fatalf("max_tokens = %d, want 1024", p.LLMRequest.MaxTokens)
	}
	// Candidate citations: one per included chunk, numbered 1..N (approach (a):
	// emitted up front so the client can map [n] as text streams).
	if len(p.Citations) != 2 {
		t.Fatalf("candidate citations = %d, want 2 (one per included chunk)", len(p.Citations))
	}
	if p.Citations[0].N != 1 || p.Citations[0].DocumentID != "d1" {
		t.Fatalf("candidate[0] = {n:%d doc:%s}, want {1 d1}", p.Citations[0].N, p.Citations[0].DocumentID)
	}
	if p.Citations[1].N != 2 || p.Citations[1].DocumentID != "d2" {
		t.Fatalf("candidate[1] = {n:%d doc:%s}, want {2 d2}", p.Citations[1].N, p.Citations[1].DocumentID)
	}
	if p.Citations[0].Snippet == "" || p.Citations[0].URI == "" {
		t.Fatalf("candidate citation missing metadata: %+v", p.Citations[0])
	}
	// Prepare must NOT call the model (streaming does that later).
	if prov.calls != 0 {
		t.Fatalf("Prepare called Complete %d times; want 0", prov.calls)
	}
}

func TestPrepareBelowFloorRefusesWithoutProvider(t *testing.T) {
	fac := &fakeFactory{p: &fakeProvider{}}
	svc := &Service{Providers: fac}

	p, err := svc.Prepare(context.Background(), Request{
		TenantID: "t-1",
		Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.001)},
		Settings: groundedSettings(),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if p.Grounded {
		t.Fatal("expected grounded=false below floor")
	}
	if p.RefusalText != "I couldn't find information about that in Acme's content." {
		t.Fatalf("refusal text = %q", p.RefusalText)
	}
	if len(p.Citations) != 0 {
		t.Fatalf("refusal must carry zero candidate citations, got %d", len(p.Citations))
	}
	if p.Provider != nil {
		t.Fatal("refusal path must not build a provider")
	}
	if fac.calls != 0 {
		t.Fatalf("provider factory called %d times on refusal; want 0", fac.calls)
	}
}

func TestPrepareProviderBuildErrorSurfaced(t *testing.T) {
	svc := &Service{Providers: &fakeFactory{err: llm.ErrProviderNotAllowed}}
	_, err := svc.Prepare(context.Background(), Request{
		TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		Settings: groundedSettings(),
	})
	if err == nil {
		t.Fatal("expected an error when the provider factory fails")
	}
}

// RecordStreamed is the SSE path's usage/logging counterpart to the accounting
// Answer does inline: it folds the streamed LLM usage into usage_daily and logs
// the query on both the grounded and refusal paths (STORY-08.8 seam).

func TestRecordStreamedFoldsUsageAndLogs(t *testing.T) {
	rec := &fakeUsage{}
	log := &fakeLogger{}
	svc := &Service{Providers: &fakeFactory{p: &fakeProvider{}}, Usage: rec, Logger: log}

	req := Request{TenantID: "t-9", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.9)},
		Settings: groundedSettings()}
	p, err := svc.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	svc.RecordStreamed(context.Background(), req, p, Usage{RetrievalMs: 10, GenerationMs: 20, InTokens: 300, OutTokens: 40})

	if len(rec.deltas) != 1 || rec.deltas[0].LLMInTokens != 300 || rec.deltas[0].LLMOutTokens != 40 {
		t.Fatalf("usage not folded: %+v", rec.deltas)
	}
	if rec.tenants[0] != "t-9" {
		t.Fatalf("usage attributed to %q, want t-9", rec.tenants[0])
	}
	if len(log.records) != 1 || !log.records[0].Grounded {
		t.Fatalf("expected one grounded log record, got %+v", log.records)
	}
}

func TestRecordStreamedRefusalFoldsNoLLMUsage(t *testing.T) {
	rec := &fakeUsage{}
	log := &fakeLogger{}
	svc := &Service{Providers: &fakeFactory{p: &fakeProvider{}}, Usage: rec, Logger: log}

	req := Request{TenantID: "t-1", Question: "q",
		Chunks:   []Chunk{chunk("c1", "d1", "alpha", 0.001)},
		Settings: groundedSettings()}
	p, _ := svc.Prepare(context.Background(), req)
	svc.RecordStreamed(context.Background(), req, p, Usage{RetrievalMs: 10})

	if len(rec.deltas) != 0 {
		t.Fatalf("refusal must fold no LLM usage, got %+v", rec.deltas)
	}
	if len(log.records) != 1 || log.records[0].Grounded {
		t.Fatalf("expected one grounded=false log record, got %+v", log.records)
	}
}
