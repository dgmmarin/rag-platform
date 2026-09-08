package eval

import (
	"context"
	"fmt"
	"regexp"

	"github.com/rag-platform/ragctl/internal/llm"
)

// This file is the STORY-12.3 LLM-as-judge (FR-ADM-04). It scores a produced
// answer against its expected answer with a single, strict LLM call and a
// deterministic verdict parser. It is opt-in (the CLI --judge flag builds it) and
// reuses the same llm.Provider seam the answer path uses (built via llm.Factory,
// fail-closed on the provider + model allowlists in llm.New). LLM-judged
// correctness is inherently noisy, so the rubric asks for one machine-parseable
// word and anything else is an error (never a silent pass) — ADR-0071.

// judgeMaxTokens caps the judge completion. The verdict is a single word, but a
// small budget leaves room for a provider that prefixes it.
//
// ponytail: a fixed small cap, not a per-tenant setting. Ceiling: a model that
// pads heavily could be truncated before the verdict; the parser then errors and
// the case is NULL (fail-soft), never wrongly scored. Upgrade path: make it a
// flag if a deployment's judge model needs more.
const judgeMaxTokens = 256

// judgeSystemPrompt is the rubric. It is deliberately strict about the output
// format so the verdict is machine-parseable (ADR-0071).
const judgeSystemPrompt = `You are a strict evaluation judge for a question-answering system.
Decide whether the ACTUAL answer is correct, given the QUESTION and the EXPECTED answer.
The ACTUAL answer is CORRECT when it conveys the same key facts as the EXPECTED answer;
paraphrasing and extra correct detail are fine, but a contradiction, a missing key fact,
or a refusal to answer is INCORRECT.
Reply with exactly one word on a single line: CORRECT or INCORRECT. Do not explain.`

// verdictIncorrect is checked BEFORE verdictCorrect: "CORRECT" is a substring of
// "INCORRECT", but the \b word boundaries make each match only as a whole word,
// so "INCORRECT" never triggers the CORRECT pattern.
var (
	verdictIncorrect = regexp.MustCompile(`(?i)\bINCORRECT\b`)
	verdictCorrect   = regexp.MustCompile(`(?i)\bCORRECT\b`)
)

// llmJudge is the production Judge over an llm.Provider.
type llmJudge struct {
	provider llm.Provider
	model    string
}

// NewLLMJudge builds an LLM-backed Judge. The provider is built (and its model
// gated by the tenant allowlists) by the caller; model is the judge model id set
// on each request.
func NewLLMJudge(provider llm.Provider, model string) Judge {
	return &llmJudge{provider: provider, model: model}
}

// Judge asks the LLM for a verdict and parses it. A provider error or an
// unparseable verdict is returned as an error, so the runner records the case as
// NULL (fail-soft) rather than guessing.
func (j *llmJudge) Judge(ctx context.Context, question, expected, actual string) (bool, error) {
	zero := 0.0
	resp, err := j.provider.Complete(ctx, llm.Request{
		Model:       j.model,
		System:      judgeSystemPrompt,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: buildJudgePrompt(question, expected, actual)}},
		MaxTokens:   judgeMaxTokens,
		Temperature: &zero, // deterministic where the provider honours it (OpenAI)
	})
	if err != nil {
		return false, fmt.Errorf("eval judge: complete: %w", err)
	}
	return parseVerdict(resp.Text)
}

// buildJudgePrompt renders the three inputs as clearly delimited DATA so the model
// treats them as content to compare, not instructions to follow (SPEC-09 §2
// prompt-injection defence).
func buildJudgePrompt(question, expected, actual string) string {
	return fmt.Sprintf(
		"QUESTION:\n<<<\n%s\n>>>\n\nEXPECTED answer:\n<<<\n%s\n>>>\n\nACTUAL answer:\n<<<\n%s\n>>>\n\nVerdict (CORRECT or INCORRECT):",
		question, expected, actual)
}

// parseVerdict extracts a strict CORRECT/INCORRECT verdict. INCORRECT is checked
// first (it contains "correct"); an absent or ambiguous verdict is an error, never
// a silent pass (ADR-0071).
func parseVerdict(text string) (bool, error) {
	switch {
	case verdictIncorrect.MatchString(text):
		return false, nil
	case verdictCorrect.MatchString(text):
		return true, nil
	default:
		return false, fmt.Errorf("eval judge: unparseable verdict %q", text)
	}
}
