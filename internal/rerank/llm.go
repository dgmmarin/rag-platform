package rerank

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/rag-platform/ragctl/internal/llm"
)

// llmReranker scores ALL candidates in ONE batched llm.Complete call (the
// user's decision, SPEC-06 §3): a listwise prompt lists the numbered passages and
// the query and asks the model to return a JSON ranking ([{id, score}] ordered most
// to least relevant). This is one extra LLM request per query — never per-document
// calls. It reuses the tenant's llm.Provider (built from settings.llm), so provider
// resilience (retry/breaker) and the provider/model allowlist are enforced by
// internal/llm; this type adds only the prompt + defensive parse.
type llmReranker struct {
	provider Completer
	model    string
	tracer   trace.Tracer
}

// rerankSystemPrompt instructs a strict, machine-parseable listwise ranking. It is
// deliberately terse and demands JSON-only output; the parser tolerates noise
// anyway (parseRanking), but a clean contract keeps most models on the JSON path.
const rerankSystemPrompt = `You are a search-result reranker. Given a user query and a numbered list of passages, rank the passages by how well each answers the query.
Respond with ONLY a JSON array, most relevant first, each element {"id": <passage number>, "score": <relevance from 0.0 to 1.0>}. Include every passage exactly once. Output no text outside the JSON array.`

func (r *llmReranker) Rerank(ctx context.Context, query string, docs []Doc) ([]Scored, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	ctx, span := r.tracer.Start(ctx, "rerank.llm")
	defer span.End()

	resp, err := r.provider.Complete(ctx, llm.Request{
		Model:       r.model,
		System:      rerankSystemPrompt,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: buildRerankPrompt(query, docs)}},
		MaxTokens:   maxRerankTokens(len(docs)),
		Temperature: ptr(0.0), // deterministic ranking where the provider honours it
	})
	if err != nil {
		span.RecordError(err)
		return nil, err // provider failure → caller falls back to fused order
	}

	ranking, err := parseRanking(resp.Text, len(docs))
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	// Map 1-based passage numbers to doc ids, in the model's returned order.
	scored := make([]Scored, 0, len(ranking))
	for _, e := range ranking {
		scored = append(scored, Scored{ID: docs[e.idx].ID, Score: e.score})
	}
	// Some models return a correct order but degenerate (all-equal / all-zero)
	// scores; synthesise a strictly descending score from position so the service's
	// Score-descending re-order preserves the model's intended order.
	scored = ensureDescending(scored)
	return orderMissingLast(docs, scored), nil
}

// buildRerankPrompt renders the query + numbered passages. Passages are 1-based so
// the model's ids map to docs[id-1]. Content is presented as data, not
// instructions (prompt-injection defence, SPEC-09 §2): the system prompt fixes the
// task and the passages are clearly delimited.
func buildRerankPrompt(query string, docs []Doc) string {
	var b strings.Builder
	b.WriteString("Query: ")
	b.WriteString(query)
	b.WriteString("\n\nPassages:\n")
	for i, d := range docs {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, oneLine(d.Text))
	}
	return b.String()
}

// oneLine collapses whitespace so one passage is one prompt line, keeping the
// numbered list unambiguous even for multi-line chunk content.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// maxRerankTokens sizes the output budget for a JSON array of n {id,score} objects
// (~16 tokens each) plus slack; bounded so a huge top_n cannot request an unbounded
// completion.
func maxRerankTokens(n int) int {
	t := 64 + 16*n
	if t > 4096 {
		return 4096
	}
	return t
}

// rankEntry is one parsed ranking element: the 0-based doc index and its score.
type rankEntry struct {
	idx   int
	score float64
}

// parseRanking extracts the JSON ranking from a model response defensively: it
// isolates the first top-level JSON array (tolerating code fences and surrounding
// prose), unmarshals {id, score} objects, drops out-of-range/duplicate ids, and
// requires at least one valid entry — otherwise ErrUnparseable (→ caller fallback).
func parseRanking(text string, n int) ([]rankEntry, error) {
	arr := extractJSONArray(text)
	if arr == "" {
		return nil, fmt.Errorf("%w: no JSON array in output", ErrUnparseable)
	}
	var raw []struct {
		ID    int     `json:"id"`
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal([]byte(arr), &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnparseable, err)
	}
	seen := make(map[int]bool, len(raw))
	out := make([]rankEntry, 0, len(raw))
	for _, e := range raw {
		i := e.ID - 1 // passages are 1-based in the prompt
		if i < 0 || i >= n || seen[i] {
			continue
		}
		seen[i] = true
		out = append(out, rankEntry{idx: i, score: e.Score})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no valid entries", ErrUnparseable)
	}
	return out, nil
}

// extractJSONArray returns the substring from the first '[' to its matching ']'
// (by bracket depth, ignoring brackets inside strings), or "" if none. This
// tolerates ```json fences and leading/trailing prose without a full parser.
func extractJSONArray(s string) string {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// inside a string literal: ignore brackets
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// ensureDescending guarantees a strictly descending score sequence in the model's
// returned order: if the provider's scores already descend they are kept; if they
// are flat or non-monotone, a position-derived score replaces them so the intended
// order survives the service's Score-descending sort. A stable sort by the (kept or
// synthesised) score then yields exactly the returned order.
func ensureDescending(scored []Scored) []Scored {
	descending := true
	for i := 1; i < len(scored); i++ {
		if scored[i].Score >= scored[i-1].Score {
			descending = false
			break
		}
	}
	if !descending {
		n := len(scored)
		for i := range scored {
			scored[i].Score = float64(n-i) / float64(n) // 1.0 .. 1/n, strictly descending
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	return scored
}

func ptr[T any](v T) *T { return &v }
