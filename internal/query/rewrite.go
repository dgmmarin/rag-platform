package query

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// Conversation-history question rewrite (STORY-08.7, SPEC-06 §1/§5, FR-RET-07).
//
// When a tenant opts in (settings.rewrite.enabled) AND the request carries
// conversation history, a single cheap llm.Complete call turns the follow-up into a
// self-contained standalone question — resolving pronouns and ellipsis — BEFORE
// retrieval, so vector + full-text search embed/match the standalone question rather
// than a context-dependent fragment. RETRIEVAL uses the standalone question; the
// ANSWER stage still receives the ORIGINAL question + history verbatim (SPEC-06 §5),
// so the model answers the user's actual turn in context.
//
// Strict passthrough (the AC's single-turn no-regression): when the toggle is off,
// when there is no history (single-turn), or when no provider factory is wired, the
// original question is used UNCHANGED and NO rewrite LLM call is made — the pipeline
// is byte-identical to pre-08.7. A full eval harness is EPIC-12; the "eval shows no
// regression on single-turn" AC is satisfied here by this zero-call passthrough
// guarantee, proven in rewrite_test.go (ADR-0057, ISSUE-0034).
//
// The rewrite never fails the query (NFR-REL-04): any provider-build error, Complete
// error (incl. a model-allowlist rejection of the override), or empty/garbled output
// falls back to the original question.

// rewriteMaxTokens bounds the rewrite completion — a standalone question is short.
const rewriteMaxTokens = 256

// rewriteSystemPrompt owns the task (the conversation is data, never instructions —
// prompt-injection defence, SPEC-09 §2). It demands ONLY the rewritten question so
// parseRewritten has little to clean up.
const rewriteSystemPrompt = `You rewrite a user's follow-up question into a standalone, self-contained question.
Using the conversation history for context, resolve pronouns, references, and any ellipsis so the resulting question can be fully understood on its own, without the history.
Preserve the user's original language, meaning, and level of detail. Do NOT answer the question or add information that is not implied by the conversation.
Respond with ONLY the rewritten question as a single line — no preamble, quotes, labels, or explanation.`

// rewriteSettings is the settings.rewrite object (SPEC-02 §5): the per-tenant toggle
// (default OFF, opt-in, mirroring settings.reranker.enabled) and an optional cheap
// model override for the rewrite call only.
type rewriteSettings struct {
	Enabled bool
	Model   string
}

// parseRewriteSettings extracts settings.rewrite; a missing object is the default
// (disabled, no override).
func parseRewriteSettings(doc map[string]any) rewriteSettings {
	var rw rewriteSettings
	if r, ok := doc["rewrite"].(map[string]any); ok {
		rw.Enabled, _ = r["enabled"].(bool)
		rw.Model, _ = r["model"].(string)
	}
	return rw
}

// standaloneQuestion returns the question to retrieve on. It is the strict passthrough
// (returns question unchanged, no LLM call) unless rewrite is enabled, history is
// present, and a factory is wired; and it falls back to the original question on any
// rewrite failure (see the package-level comment).
func (s *Service) standaloneQuestion(ctx context.Context, tid tenant.ID, st answer.Settings, rw rewriteSettings, history []answer.Turn, question string) string {
	if !rw.Enabled || len(history) == 0 || s.Providers == nil {
		return question
	}

	// Reuse the tenant's LLM provider/model (the same factory the answer path uses);
	// the optional rewrite.model swaps in a cheaper model, still gated by the model
	// allowlist inside the factory (fail-closed → fall back on rejection).
	pst := st
	if rw.Model != "" {
		pst.LLMModel = rw.Model
	}
	prov, err := s.Providers.Provider(pst)
	if err != nil {
		slog.WarnContext(ctx, "query: rewrite provider build failed; using original question",
			"tenant", tid.String(), "err", err)
		return question
	}

	resp, err := prov.Complete(ctx, llm.Request{
		Model:       pst.LLMModel,
		System:      rewriteSystemPrompt,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: rewriteUserMessage(lastTurns(history, historyN(st)), question)}},
		MaxTokens:   rewriteMaxTokens,
		Temperature: f64(0), // deterministic where the provider honours it (Anthropic ignores)
	})
	if err != nil {
		// Errors carry no prompt content (C-4); ErrCircuitOpen included — the rewrite
		// degrades to the original question rather than failing the query.
		slog.WarnContext(ctx, "query: rewrite call failed; using original question",
			"tenant", tid.String(), "err", err)
		return question
	}

	standalone := parseRewritten(resp.Text)
	if standalone == "" {
		slog.WarnContext(ctx, "query: rewrite produced empty output; using original question",
			"tenant", tid.String())
		return question
	}
	return standalone
}

// rewriteUserMessage renders the conversation history + the follow-up as delimited
// DATA (not instructions — SPEC-09 §2). A single user message avoids provider
// role-alternation constraints and keeps the whole history clearly bounded.
func rewriteUserMessage(history []answer.Turn, question string) string {
	var b strings.Builder
	b.WriteString("Conversation history:\n")
	for _, t := range history {
		fmt.Fprintf(&b, "%s: %s\n", roleLabel(t.Role), oneLine(t.Content))
	}
	b.WriteString("\nFollow-up question: ")
	b.WriteString(question)
	return b.String()
}

// roleLabel renders a turn's role for the history transcript; unknown roles read as
// the user (matching internal/answer.mapRole's default).
func roleLabel(role string) string {
	if strings.EqualFold(role, string(llm.RoleAssistant)) {
		return "Assistant"
	}
	return "User"
}

// parseRewritten defensively extracts the rewritten question: it strips a surrounding
// ``` code fence and wrapping quotes, and trims whitespace. An empty result signals
// the caller to fall back to the original question.
//
// ponytail: a light unwrapper, not a full parser — the system prompt already asks for
// a bare single-line question; this only tolerates the common fence/quote noise.
func parseRewritten(text string) string {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:] // drop the ``` / ```lang opening line
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	return s
}

// lastTurns returns the trailing n history turns (SPEC-06 §5 "last N turns"), matching
// internal/answer's own history bound so the rewrite and the answer see the same window.
func lastTurns(history []answer.Turn, n int) []answer.Turn {
	if n <= 0 || len(history) <= n {
		return history
	}
	return history[len(history)-n:]
}

// historyN is the tenant's settings.answering.history_n bound (default 6), read from
// the resolved answer settings (its own accessor is unexported).
func historyN(st answer.Settings) int {
	if st.HistoryN > 0 {
		return st.HistoryN
	}
	return answer.DefaultHistoryN
}

// oneLine collapses whitespace so one turn is one transcript line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func f64(v float64) *float64 { return &v }
