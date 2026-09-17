// Package answer is the answering stage of retrieval (SPEC-06 §4–5, FR-RET-04/05):
// it turns a ranked set of retrieved chunks plus the user's question into a
// grounded, cited answer — or a fixed refusal when nothing is relevant enough.
//
// It is the seam STORY-08.6 wraps with the /v1/query HTTP endpoint + SSE. It does
// NOT retrieve or rerank (STORY-08.1/08.3 own that; the caller hands the ranked
// chunks in, their Score already being the reranker score when reranking is
// enabled, else the fused RRF score — SPEC-06 §3), it does NOT rewrite follow-up
// questions (STORY-08.7 — this stage includes provided history verbatim only), and
// it does NOT persist the query log (STORY-08.8 — a clean QueryLogger seam is left,
// called on BOTH the grounded and the grounded=false paths).
//
// Responsibilities (Answer):
//  1. Grounding gate (SPEC-06 §4): keep only chunks whose Score ≥ min_score. If
//     none pass, respond grounded=false with the fixed refusal message
//     ("I couldn't find information about that in <tenant name>'s content."), zero
//     citations, and NO LLM call — the provider is never even built.
//  2. Prompt assembly (SPEC-06 §5): a system prompt (answer only from the sources,
//     cite as [n], say when unsure, default to English), a context block of
//     numbered chunks truncated to a token budget, and any provided history turns.
//  3. Generation via the internal/llm Complete seam (STORY-08.4).
//  4. Post-processing: parse [n] markers, map each to its chunk, build citations,
//     and DROP chunks no marker referenced.
//  5. Usage accounting (FR-RET-04, ADR-0024): fold the provider's token Usage into
//     usage_daily and into the response usage object.
//
// Ingested/crawled chunk text is presented as DATA, never instructions
// (prompt-injection defence, SPEC-09 §2): a fixed system prompt owns the task and
// the sources are delimited under a "Sources:" header inside the user turn.
package answer

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/cp/usage"
	"github.com/rag-platform/ragctl/internal/llm"
)

// Defaults (SPEC-06 §5). Applied when the tenant's settings omit the field.
const (
	// DefaultMinScore is the grounding floor when settings.retrieval.min_score is
	// unset (matches settings_defaults.json). A non-positive configured value is
	// treated as unset so grounding never silently degrades to "everything passes".
	DefaultMinScore = 0.02
	// DefaultTokenBudget bounds the context block (SPEC-06 §5, default 6k).
	DefaultTokenBudget = 6000
	// DefaultHistoryN is how many trailing history turns are included (SPEC-06 §5).
	DefaultHistoryN = 6
	// DefaultMaxTokens caps generation when settings.llm.max_tokens is unset
	// (matches settings_defaults.json).
	DefaultMaxTokens = 1024
	// snippetRunes bounds the citation snippet length. ponytail: a fixed character
	// window, not a sentence-aware excerpt; good enough for a UI hint.
	snippetRunes = 240
)

// Chunk is one retrieved, ranked chunk the answering stage consumes. It carries
// exactly the citation metadata SPEC-06 §5 needs plus the grounding Score. The
// caller (STORY-08.6) maps retrieve.Result → Chunk; keeping a local type keeps this
// package independent of the retrieval pipeline's shape.
type Chunk struct {
	ChunkID     string
	DocumentID  string
	Title       string
	URI         string
	HeadingPath []string
	Content     string
	// Score is the reranker score when reranking is enabled, else the fused RRF
	// score (SPEC-06 §3). The grounding floor (min_score) is applied to it.
	Score float64
}

// Turn is one conversation-history turn provided by the client (SPEC-06 §5). Role
// is "user" or "assistant". History is included verbatim; the follow-up→standalone
// rewrite is STORY-08.7.
type Turn struct {
	Role    string
	Content string
}

// Settings is the subset of the tenant's settings document (SPEC-02 §5) the
// answering stage needs. The caller resolves it (the tenant display name from the
// control-plane tenants row, the rest from settings.{retrieval,answering,llm}).
type Settings struct {
	// TenantName is the tenant's display name (control-plane tenants.name),
	// substituted into the refusal message (SPEC-06 §4).
	TenantName string

	// MinScore is settings.retrieval.min_score, the grounding floor (SPEC-06 §4).
	MinScore float64
	// Reranked is true when the retrieved chunks carry reranker relevance scores
	// (settings.reranker.enabled). The grounding floor is only meaningful on that
	// 0..1 scale; without a reranker the score is the rank-based fused RRF value
	// (a single-list hit tops out at ~0.016), so an absolute floor there drops every
	// semantic-only match and makes retrieval keyword-only. minScore() applies the
	// floor only when Reranked.
	Reranked bool
	// TokenBudget is settings.answering.token_budget (default 6k, SPEC-06 §5).
	TokenBudget int
	// HistoryN is settings.answering.history_n, trailing turns to include.
	HistoryN int

	// LLM (settings.llm) — provider/model + fail-closed allowlists (SPEC-09 §2).
	// MaxTokens is settings.llm.max_tokens (generation cap).
	LLMProvider      string
	LLMModel         string
	LLMModelsAllowed []string
	ProvidersAllowed []string
	MaxTokens        int
}

func (s Settings) minScore() float64 {
	// The grounding floor is a relevance threshold, meaningful only on the reranker's
	// 0..1 score. Without a reranker the score is the fused RRF value (rank-based,
	// ~0.016 for a single-list hit), so an absolute floor would drop every
	// semantic-only match; disable the floor and rely on final_k plus the model's own
	// grounding refusal. See Settings.Reranked.
	if !s.Reranked {
		return 0
	}
	if s.MinScore <= 0 {
		return DefaultMinScore
	}
	return s.MinScore
}

func (s Settings) tokenBudget() int {
	if s.TokenBudget <= 0 {
		return DefaultTokenBudget
	}
	return s.TokenBudget
}

func (s Settings) historyN() int {
	if s.HistoryN <= 0 {
		return DefaultHistoryN
	}
	return s.HistoryN
}

func (s Settings) maxTokens() int {
	if s.MaxTokens <= 0 {
		return DefaultMaxTokens
	}
	return s.MaxTokens
}

// Request is one answering call: the tenant, the question, the ranked chunks, any
// conversation history, the resolved settings, and the retrieval latency the caller
// already measured (folded into the response usage; the answering stage does not
// retrieve).
type Request struct {
	TenantID    string
	Question    string
	Chunks      []Chunk
	History     []Turn
	Settings    Settings
	RetrievalMs int64
}

// Usage is the SPEC-06 §6 usage object. RetrievalMs is measured by the caller;
// GenerationMs is measured around the Complete call; token counts come from the
// provider's normalised Usage.
type Usage struct {
	RetrievalMs  int64 `json:"retrieval_ms"`
	GenerationMs int64 `json:"generation_ms"`
	InTokens     int   `json:"in_tokens"`
	OutTokens    int   `json:"out_tokens"`
}

// Citation is one [n] reference resolved to its chunk (SPEC-06 §5). N is the marker
// number as it appears in the answer text — citations are NOT renumbered when
// unreferenced chunks are dropped, so [n] in the answer always lines up.
type Citation struct {
	N           int      `json:"n"`
	DocumentID  string   `json:"document_id"`
	Title       string   `json:"title"`
	URI         string   `json:"uri"`
	HeadingPath []string `json:"heading_path"`
	Snippet     string   `json:"snippet"`
}

// Result is the SPEC-06 §6 answer shape.
type Result struct {
	ID        string     `json:"id"`
	Answer    string     `json:"answer"`
	Grounded  bool       `json:"grounded"`
	Citations []Citation `json:"citations"`
	Usage     Usage      `json:"usage"`
	Model     string     `json:"model"`
}

// ProviderFactory builds the tenant's llm.Provider from its settings (fail-closed
// on the provider + model allowlists inside llm.New). Tests inject a fake;
// production wires KeyedProviderFactory. It mirrors retrieve.RerankerFactory.
type ProviderFactory interface {
	Provider(s Settings) (llm.Provider, error)
}

// UsageRecorder folds LLM token usage into usage_daily (ADR-0024). *usage.Counter
// satisfies it. Nil is a no-op (usage still appears in the response).
type UsageRecorder interface {
	Add(tenantID string, d usage.Delta)
}

// QueryRecord is what a query logger persists (STORY-08.8). It carries what the log
// needs from BOTH the grounded and grounded=false paths: the retrieved chunk ids
// and scores, whether the answer was grounded, the citations, and usage.
type QueryRecord struct {
	ID                string
	TenantID          string
	Question          string
	Grounded          bool
	RetrievedChunkIDs []string
	RetrievedScores   []float64
	CitationChunkIDs  []string
	Usage             Usage
	Model             string
}

// QueryLogger is the seam STORY-08.8 fills (async query log + feedback). It is
// called on every Answer, refusal included, so the grounded=false path is
// loggable. Nil is a no-op in this story.
type QueryLogger interface {
	Log(ctx context.Context, rec QueryRecord)
}

// Service produces grounded answers. It is stateless and safe for concurrent use.
type Service struct {
	// Providers builds the tenant's llm.Provider. Required.
	Providers ProviderFactory
	// Usage records LLM token usage into usage_daily (ADR-0024). Optional (nil = no
	// counter; the response still carries usage).
	Usage UsageRecorder
	// Logger is the STORY-08.8 query-log seam. Optional (nil = no logging).
	Logger QueryLogger
}

// markerRe matches a citation marker like [1] or [12] in the model's answer.
var markerRe = regexp.MustCompile(`\[(\d+)\]`)

// Prepared is the shared front half of the answering pipeline — the grounding gate
// plus prompt assembly, with generation NOT yet run. Answer (JSON, non-streaming)
// and the STORY-08.6 SSE query endpoint both build on it so the two modes assemble
// the SAME prompt and the SAME numbered citations. On the refusal path Grounded is
// false, RefusalText carries the fixed message, and Provider/LLMRequest are zero —
// the provider is never even built (SPEC-06 §4).
type Prepared struct {
	// ID is the response id (SPEC-06 §6, "q_..."). Prepare mints it once so the
	// SSE `done` event, the JSON body, and the query log all agree.
	ID string
	// Grounded is false when no chunk passed the min_score floor.
	Grounded bool
	// RefusalText is the fixed SPEC-06 §4 message, set only when !Grounded.
	RefusalText string
	// Citations are the CANDIDATE citations: one per numbered context chunk
	// (N = 1..len(Included)), in context order. The SSE path emits these up front
	// (SPEC-06 §6 "retrieval (citations first)", approach (a), ADR-0056): the [n]
	// markers in the streamed text index into them. JSON mode instead keeps only the
	// referenced subset (mapCitations, unreferenced dropped) — the numbering is the
	// same, JSON simply drops the ones no marker used. Empty on refusal.
	Citations []Citation
	// Included is the context chunk set actually numbered into the prompt (the [n] →
	// chunk mapping), for post-hoc citation mapping and query logging.
	Included []Chunk
	// Provider is the tenant's llm.Provider, built only on the grounded path (nil on
	// refusal). The SSE path calls Provider.Stream; Answer calls Provider.Complete.
	Provider llm.Provider
	// LLMRequest is the assembled generation request (system + history + sources turn,
	// model, max tokens). Identical for Complete (JSON) and Stream (SSE).
	LLMRequest llm.Request
	// Model is the tenant's configured model (LLMRequest.Model), the fallback for the
	// response `model` when the provider does not echo one.
	Model string
	// RetrievalMs is the caller-measured retrieval latency, carried into usage.
	RetrievalMs int64
}

// Prepare runs the grounding gate (SPEC-06 §4) and, when grounded, assembles the
// prompt and builds the provider (SPEC-06 §5) WITHOUT generating. It is the front
// half Answer reuses and the SSE path (STORY-08.6) drives directly. On the refusal
// path it returns before the provider is built (no LLM call is possible).
func (s *Service) Prepare(_ context.Context, req Request) (Prepared, error) {
	st := req.Settings

	// 1. Grounding gate (SPEC-06 §4): keep only chunks above the floor.
	grounded := passFloor(req.Chunks, st.minScore())
	if len(grounded) == 0 {
		return Prepared{
			ID:          newID(),
			Grounded:    false,
			RefusalText: refusalMessage(st.TenantName),
			RetrievalMs: req.RetrievalMs,
		}, nil
	}

	// 2. Prompt assembly (SPEC-06 §5). Number the grounded chunks, then trim the
	// context to the token budget; included is the definitive [n] → chunk mapping.
	included := withinBudget(grounded, st.tokenBudget())
	system := systemPrompt(st.TenantName)
	messages := buildMessages(req.History, st.historyN(), included, req.Question)

	// 3. Build the provider only now — never on refusal.
	prov, err := s.Providers.Provider(st)
	if err != nil {
		return Prepared{}, fmt.Errorf("answer: build provider: %w", err)
	}
	return Prepared{
		ID:        newID(),
		Grounded:  true,
		Citations: candidateCitations(included),
		Included:  included,
		Provider:  prov,
		LLMRequest: llm.Request{
			Model:     st.LLMModel,
			System:    system,
			Messages:  messages,
			MaxTokens: st.maxTokens(),
		},
		Model:       st.LLMModel,
		RetrievalMs: req.RetrievalMs,
	}, nil
}

// Answer runs the grounding gate, assembles the prompt, generates, and maps
// citations. On the refusal path it makes no LLM call. It is the JSON
// (non-streaming) entry point; the SSE path (STORY-08.6) uses Prepare + Stream +
// RecordStreamed instead, sharing Prepare's grounding + assembly.
func (s *Service) Answer(ctx context.Context, req Request) (Result, error) {
	p, err := s.Prepare(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if !p.Grounded {
		res := Result{
			ID:       p.ID,
			Answer:   p.RefusalText,
			Grounded: false,
			Usage:    Usage{RetrievalMs: req.RetrievalMs},
		}
		s.logQuery(ctx, req, res, nil)
		return res, nil
	}

	// Generation (STORY-08.4).
	start := time.Now()
	resp, err := p.Provider.Complete(ctx, p.LLMRequest)
	genMs := time.Since(start).Milliseconds()
	if err != nil {
		// Surface a clean error preserving the sentinel (e.g. llm.ErrCircuitOpen so
		// STORY-08.6 can degrade to retrieval-only). Never includes prompt content.
		return Result{}, fmt.Errorf("answer: generation failed: %w", err)
	}

	// Citation mapping + unreferenced drop (SPEC-06 §5).
	citations := mapCitations(resp.Text, p.Included)

	// Usage accounting (FR-RET-04, ADR-0024).
	model := resp.Model
	if model == "" {
		model = p.Model
	}
	res := Result{
		ID:        p.ID,
		Answer:    resp.Text,
		Grounded:  true,
		Citations: citations,
		Usage: Usage{
			RetrievalMs:  req.RetrievalMs,
			GenerationMs: genMs,
			InTokens:     resp.Usage.InputTokens,
			OutTokens:    resp.Usage.OutputTokens,
		},
		Model: model,
	}
	if s.Usage != nil && req.TenantID != "" {
		s.Usage.Add(req.TenantID, usage.Delta{
			LLMInTokens:  int64(resp.Usage.InputTokens),
			LLMOutTokens: int64(resp.Usage.OutputTokens),
		})
	}
	s.logQuery(ctx, req, res, p.Included)
	return res, nil
}

// RecordStreamed folds a streamed generation's usage into usage_daily and logs the
// query (STORY-08.8 seam) — the SSE-path counterpart of the accounting Answer does
// inline. STORY-08.6 calls it once the SSE stream drains, with the final usage (zero
// LLM tokens on the refusal or generation-unavailable paths, so nothing is folded
// then). The log record carries the candidate citation chunk ids (the SSE contract
// emits all context chunks up front and maps [n] client-side).
func (s *Service) RecordStreamed(ctx context.Context, req Request, p Prepared, u Usage) {
	if s.Usage != nil && req.TenantID != "" && (u.InTokens > 0 || u.OutTokens > 0) {
		s.Usage.Add(req.TenantID, usage.Delta{
			LLMInTokens:  int64(u.InTokens),
			LLMOutTokens: int64(u.OutTokens),
		})
	}
	res := Result{
		ID:        p.ID,
		Grounded:  p.Grounded,
		Citations: p.Citations,
		Usage:     u,
		Model:     p.Model,
	}
	s.logQuery(ctx, req, res, p.Included)
}

// refusalMessage is the fixed SPEC-06 §4 refusal, substituting the tenant name.
func refusalMessage(tenantName string) string {
	return fmt.Sprintf("I couldn't find information about that in %s's content.", tenantName)
}

// candidateCitations builds one citation per numbered context chunk (N = 1..len),
// in context order — the candidate set the SSE path emits up front (SPEC-06 §6,
// ADR-0056). Unlike mapCitations it drops nothing: the client maps [n] markers to
// these as the answer streams.
func candidateCitations(chunks []Chunk) []Citation {
	out := make([]Citation, 0, len(chunks))
	for i, c := range chunks {
		out = append(out, Citation{
			N:           i + 1,
			DocumentID:  c.DocumentID,
			Title:       c.Title,
			URI:         c.URI,
			HeadingPath: c.HeadingPath,
			Snippet:     snippet(c.Content),
		})
	}
	return out
}

// passFloor returns the chunks whose Score meets the grounding floor, preserving
// rank order (SPEC-06 §4).
func passFloor(chunks []Chunk, minScore float64) []Chunk {
	out := make([]Chunk, 0, len(chunks))
	for _, c := range chunks {
		if c.Score >= minScore {
			out = append(out, c)
		}
	}
	return out
}

// withinBudget selects the leading chunks whose numbered context blocks fit the
// token budget (SPEC-06 §5), preserving order. At least the top chunk is always
// included — its content truncated to fit if it alone exceeds the budget — so a
// grounded query never assembles an empty context.
//
// ponytail: token count is estimated as len(text)/4, a defensible heuristic, not a
// real tokenizer. Upgrade path: swap estimateTokens for a model-specific
// count_tokens (e.g. tiktoken/anthropic count-tokens) when exactness matters.
func withinBudget(chunks []Chunk, budget int) []Chunk {
	out := make([]Chunk, 0, len(chunks))
	used := 0
	for i, c := range chunks {
		blk := estimateTokens(renderBlock(i+1, c))
		if used+blk > budget {
			if len(out) == 0 {
				out = append(out, truncateToBudget(c, i+1, budget))
			}
			break
		}
		used += blk
		out = append(out, c)
	}
	return out
}

// truncateToBudget trims a single chunk's content so its numbered block fits the
// budget, guaranteeing the top chunk is always groundable.
func truncateToBudget(c Chunk, n, budget int) Chunk {
	// Budget in characters (≈4 per token), minus the header/uri overhead.
	overhead := len(renderBlock(n, Chunk{Title: c.Title, URI: c.URI, HeadingPath: c.HeadingPath}))
	maxChars := budget*4 - overhead
	if maxChars < 0 {
		maxChars = 0
	}
	r := []rune(c.Content)
	if len(r) > maxChars {
		c.Content = string(r[:maxChars])
	}
	return c
}

// renderBlock formats one numbered source block for the context (SPEC-06 §5:
// "title > heading_path header and its uri").
func renderBlock(n int, c Chunk) string {
	header := c.Title
	if len(c.HeadingPath) > 0 {
		header += " > " + strings.Join(c.HeadingPath, " > ")
	}
	return fmt.Sprintf("[%d] %s\n%s\n%s", n, header, c.URI, c.Content)
}

// estimateTokens approximates a token count from character length (chars/4).
// ponytail: heuristic, not a tokenizer — see withinBudget.
func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// systemPrompt is the SPEC-06 §5 system prompt: tenant name, answer only from the
// provided sources, cite as [n], say when unsure, and default to English.
// The language match is a prompt instruction (no detection library) — the model
// answers in the question's language.
func systemPrompt(tenantName string) string {
	return fmt.Sprintf(`You are a question-answering assistant for %s.
Answer the user's question using ONLY the information in the provided sources.
Cite every claim with a bracketed marker of the form [n] (e.g. [1] or [2]) that refers to the numbered source it came from; you may cite more than one.
If the sources do not contain the answer, say that you are not sure rather than guessing or using outside knowledge.
Reply in English by default, even when the sources are in another language; only reply in another language if the user's question is clearly written in that language.`, tenantName)
}

// buildMessages assembles the conversation: the last historyN turns verbatim
// (SPEC-06 §5; the follow-up rewrite is STORY-08.7), then the final user turn
// carrying the numbered sources and the question. Sources are delimited data, not
// instructions (SPEC-09 §2).
func buildMessages(history []Turn, historyN int, chunks []Chunk, question string) []llm.Message {
	msgs := make([]llm.Message, 0, len(history)+1)
	for _, t := range lastTurns(history, historyN) {
		msgs = append(msgs, llm.Message{Role: mapRole(t.Role), Content: t.Content})
	}

	var b strings.Builder
	b.WriteString("Sources:\n")
	for i, c := range chunks {
		b.WriteString(renderBlock(i+1, c))
		b.WriteString("\n\n")
	}
	b.WriteString("Question: ")
	b.WriteString(question)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: b.String()})
	return msgs
}

// lastTurns returns the trailing n turns (SPEC-06 §5: "last N turns").
func lastTurns(history []Turn, n int) []Turn {
	if n <= 0 || len(history) <= n {
		return history
	}
	return history[len(history)-n:]
}

// mapRole maps a Turn role to an llm.Role, defaulting unknown roles to user.
func mapRole(role string) llm.Role {
	if strings.EqualFold(role, string(llm.RoleAssistant)) {
		return llm.RoleAssistant
	}
	return llm.RoleUser
}

// mapCitations parses [n] markers from the answer and maps each to its chunk,
// dropping any chunk no marker referenced and any marker out of range (SPEC-06 §5).
// Markers keep their number; citations are returned in ascending marker order,
// de-duplicated.
func mapCitations(answer string, chunks []Chunk) []Citation {
	seen := map[int]bool{}
	var ns []int
	for _, m := range markerRe.FindAllStringSubmatch(answer, -1) {
		n := atoi(m[1])
		if n < 1 || n > len(chunks) || seen[n] {
			continue
		}
		seen[n] = true
		ns = append(ns, n)
	}
	sort.Ints(ns)
	out := make([]Citation, 0, len(ns))
	for _, n := range ns {
		c := chunks[n-1]
		out = append(out, Citation{
			N:           n,
			DocumentID:  c.DocumentID,
			Title:       c.Title,
			URI:         c.URI,
			HeadingPath: c.HeadingPath,
			Snippet:     snippet(c.Content),
		})
	}
	return out
}

// snippet returns a short single-line excerpt of a chunk's content for a citation.
func snippet(content string) string {
	s := strings.Join(strings.Fields(content), " ")
	r := []rune(s)
	if len(r) > snippetRunes {
		return strings.TrimSpace(string(r[:snippetRunes])) + "…"
	}
	return s
}

// atoi parses a non-negative integer, returning -1 on overflow/garbage (regex
// already guarantees digits, but a very long run could overflow int).
func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
		if n < 0 {
			return -1
		}
	}
	return n
}

// logQuery calls the query-log seam (STORY-08.8) on both the grounded and refusal
// paths. included is the chunk set actually put in the prompt (nil on refusal); the
// retrieved ids/scores come from the full request so the log reflects what
// retrieval returned.
func (s *Service) logQuery(ctx context.Context, req Request, res Result, included []Chunk) {
	if s.Logger == nil {
		return
	}
	ids := make([]string, len(req.Chunks))
	scores := make([]float64, len(req.Chunks))
	for i, c := range req.Chunks {
		ids[i] = c.ChunkID
		scores[i] = c.Score
	}
	citeIDs := make([]string, 0, len(res.Citations))
	for _, c := range res.Citations {
		if c.N >= 1 && c.N <= len(included) {
			citeIDs = append(citeIDs, included[c.N-1].ChunkID)
		}
	}
	s.Logger.Log(ctx, QueryRecord{
		ID:                res.ID,
		TenantID:          req.TenantID,
		Question:          req.Question,
		Grounded:          res.Grounded,
		RetrievedChunkIDs: ids,
		RetrievedScores:   scores,
		CitationChunkIDs:  citeIDs,
		Usage:             res.Usage,
		Model:             res.Model,
	})
}

// newID mints the response id (SPEC-06 §6, "q_..."), matching the documents package
// convention of google/uuid.
func newID() string {
	return "q_" + uuid.NewString()
}

// KeyedProviderFactory is the production ProviderFactory: it builds the tenant's
// llm.Provider from settings.llm via the shared llm.Factory (which enforces the
// provider + model allowlists fail-closed, SPEC-09 §2). Keys are never logged (C-4).
type KeyedProviderFactory struct {
	LLM llm.Factory
}

// Provider builds the provider for the tenant's configured LLM provider/model.
func (f KeyedProviderFactory) Provider(s Settings) (llm.Provider, error) {
	return f.LLM.Provider(s.LLMProvider, s.LLMModel, s.ProvidersAllowed, s.LLMModelsAllowed)
}
