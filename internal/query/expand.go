package query

import (
	"context"
	"log/slog"
	"strings"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// Query expansion (ADR-0079, SPEC-06 §1). A standalone question often shares little
// vocabulary with the passage that answers it ("sell an offer with 0 allotment" vs a
// "Waiting list (WL)" page), so vector search ranks the right chunk far down. HyDE
// closes that gap: a single cheap LLM call drafts a short HYPOTHETICAL ANSWER, and
// retrieval embeds THAT (answer-shaped text) for the vector side. The full-text (BM25)
// side and the reranker keep the user's real question, so exact keywords and intent
// are preserved (internal/retrieve.Request.EmbedText).
//
// Strict passthrough (no regression, mirroring rewrite.go): when the mode is off,
// when no provider factory is wired, or on ANY failure (provider build, Complete
// error incl. ErrCircuitOpen, empty output), it returns "" and retrieval embeds the
// question unchanged. Expansion never fails the query (NFR-REL-04).

// hydeMaxTokens bounds the hypothetical-answer draft — a few sentences is enough to
// shift the embedding toward the target vocabulary; more only adds latency and drift.
const hydeMaxTokens = 256

// hydeSystemPrompt owns the task; the question is data, never instructions
// (prompt-injection defence, SPEC-09 §2). It demands a bare passage so the whole
// output can be embedded directly.
const hydeSystemPrompt = `You write a short, plausible passage that would appear in a product's documentation and directly answer the user's question.
Write 2 to 4 sentences of factual documentation prose in the domain's own terminology. Do NOT hedge, ask for clarification, or say the answer is unknown; write the passage as if it exists.
Respond with ONLY the passage text — no preamble, headings, quotes, or explanation.`

// expansionSettings is the settings.expansion object (SPEC-02 §5): the per-tenant
// mode (default "off") and an optional cheaper model override for the expansion call.
type expansionSettings struct {
	Mode  string
	Model string
}

// parseExpansionSettings extracts settings.expansion; a missing object is the default
// (off, no override).
func parseExpansionSettings(doc map[string]any) expansionSettings {
	var ex expansionSettings
	if e, ok := doc["expansion"].(map[string]any); ok {
		ex.Mode, _ = e["mode"].(string)
		ex.Model, _ = e["model"].(string)
	}
	return ex
}

// hydeEmbedText returns the text retrieval should embed for the VECTOR side, or "" to
// embed the question unchanged. It is a no-op (no LLM call, "") unless mode is "hyde"
// and a factory is wired, and it falls back to "" on any failure.
func (s *Service) hydeEmbedText(ctx context.Context, tid tenant.ID, st answer.Settings, ex expansionSettings, question string) string {
	if ex.Mode != "hyde" || s.Providers == nil || strings.TrimSpace(question) == "" {
		return ""
	}

	// Reuse the tenant's LLM factory (same one the answer + rewrite paths use); an
	// optional expansion.model swaps in a cheaper model, still gated by the model
	// allowlist inside the factory (fail-closed → fall back on rejection).
	pst := st
	if ex.Model != "" {
		pst.LLMModel = ex.Model
	}
	prov, err := s.Providers.Provider(pst)
	if err != nil {
		slog.WarnContext(ctx, "query: hyde provider build failed; embedding question",
			"tenant", tid.String(), "err", err)
		return ""
	}

	resp, err := prov.Complete(ctx, llm.Request{
		Model:       pst.LLMModel,
		System:      hydeSystemPrompt,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: question}},
		MaxTokens:   hydeMaxTokens,
		Temperature: f64(0),
	})
	if err != nil {
		// Errors carry no prompt content (C-4); ErrCircuitOpen included — expansion
		// degrades to embedding the question rather than failing the query.
		slog.WarnContext(ctx, "query: hyde call failed; embedding question",
			"tenant", tid.String(), "err", err)
		return ""
	}

	// Reuse the rewrite unwrapper for stray fences/quotes; empty → passthrough.
	doc := parseRewritten(resp.Text)
	if doc == "" {
		slog.WarnContext(ctx, "query: hyde produced empty output; embedding question",
			"tenant", tid.String())
		return ""
	}
	return doc
}
