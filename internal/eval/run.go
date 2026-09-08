package eval

import (
	"context"
	"strings"
	"time"
)

// This file is the STORY-12.2 eval RUNNER (SPEC-06 §8, FR-ADM-04): given a
// tenant's eval_cases, it runs each through the retrieval + answering pipeline,
// scores recall@k and grounded-ness, times the answer call, and records an
// eval_run plus one eval_result per case. The scoring is pure and the pipeline
// and persistence are behind small ports, so the runner is unit-testable with
// fakes (no DB, no LLM). LLM-as-judge correctness (judged_correct) is STORY-12.3
// and is deliberately left NULL / absent from the summary here.

// Pipeline is the retrieval + answering surface the runner needs for one tenant.
// It is exported because the production adapter (wrapping retrieve.Service +
// query.Service, which the CLI composes) lives outside this package; tests inject
// a fake. Retrieve returns the retrieved document ids in rank order (for
// recall@k); Answer returns whether the answer was grounded (for the grounded
// rate) and the answer text. Both take the same k.
type Pipeline interface {
	Retrieve(ctx context.Context, question string, k int) ([]string, error)
	Answer(ctx context.Context, question string, k int) (grounded bool, answer string, err error)
}

// Judge scores a produced answer against its expected answer (STORY-12.3,
// LLM-as-judge). It is OPTIONAL on the Runner (nil = no judging; the default run
// leaves judged_correct NULL). The runner calls it only for cases with a
// non-empty expected answer; a judge error (or unparseable verdict, surfaced as
// an error by the implementation) is fail-soft — that case's judged_correct stays
// NULL and the run continues (never a silent "correct").
type Judge interface {
	Judge(ctx context.Context, question, expected, actual string) (correct bool, err error)
}

// caseSource supplies the cases to run; runSink persists the run and its
// per-case results. Both are unexported ports satisfied by DB-backed adapters
// (runstore.go) and by test fakes.
type caseSource interface {
	cases(ctx context.Context) ([]Case, error)
}

type runSink interface {
	create(ctx context.Context, config map[string]any) (runID string, err error)
	record(ctx context.Context, runID string, r CaseResult) error
	finish(ctx context.Context, runID string, s Summary) error
}

// CaseResult is one case's computed outcome. It feeds both the eval_results row
// and the summary aggregation. Grounded and Err are aggregated into the summary
// but are not eval_results columns (the schema has no grounded column; a soft
// per-case error is reflected as a recall miss and, for grounded, a false).
type CaseResult struct {
	CaseID          string
	RetrievedDocIDs []string
	RecallHit       *bool // nil when the case has no expected_doc_ids (excluded from recall@k)
	Answer          string
	Grounded        bool
	// JudgedCorrect is the LLM-judge verdict (STORY-12.3). nil (→ NULL) when
	// judging is off, the case has no expected_answer, or the judge failed.
	JudgedCorrect *bool
	LatencyMs     int
	Err           error // pipeline error for this case (fail-soft; the run continues)
}

// Summary is the printed + stored run summary (SPEC-06 §8): recall@k, grounded
// rate and mean latency, plus the denominators that make them interpretable.
type Summary struct {
	RunID                string  `json:"run_id,omitempty"`
	Cases                int     `json:"cases"`
	K                    int     `json:"k"`
	RecallAtK            float64 `json:"recall_at_k"`
	CasesScoredForRecall int     `json:"cases_scored_for_recall"`
	GroundedRate         float64 `json:"grounded_rate"`
	MeanLatencyMs        int     `json:"mean_latency_ms"`
	Errors               int     `json:"errors"`
	// Correctness (STORY-12.3), present only when judging ran: CasesJudged is the
	// count with a non-NULL verdict (the denominator); CorrectnessRate =
	// judged-correct / CasesJudged. Both omitempty so a non-judged run's summary is
	// byte-identical to STORY-12.2. (An all-incorrect judged run reports
	// CasesJudged>0 with the rate omitted at 0 — the denominator still signals that
	// judging happened.)
	CasesJudged     int     `json:"cases_judged,omitempty"`
	CorrectnessRate float64 `json:"correctness_rate,omitempty"`
}

// RunOptions configures a run.
type RunOptions struct {
	// Pipeline runs retrieval + answering for the tenant. Required.
	Pipeline Pipeline
	// Judge scores answers against expected answers (STORY-12.3). Optional: nil
	// disables judging (the default run).
	Judge Judge
	// K is the recall@k / retrieval top-k for the run (resolved from the effective
	// settings.retrieval.final_k, with the --config-file overlay applied).
	K int
	// Config is the EFFECTIVE settings the run used (tenant settings merged with
	// the --config-file overlay); stored verbatim in eval_runs.config.
	Config map[string]any
	// Limit bounds how many cases to run (0 = the store default cap).
	Limit int
}

// Runner orchestrates one run over the injected ports. It is DB- and
// pipeline-agnostic (all three collaborators are ports), so it is fully
// unit-testable.
type Runner struct {
	Cases    caseSource
	Sink     runSink
	Pipeline Pipeline
	// Judge scores answers against expected answers (STORY-12.3). Optional: nil
	// disables judging (the default run).
	Judge Judge
	K     int
	// Now is the clock used to time the answer call; defaults to time.Now.
	Now func() time.Time
}

// Run executes every case, recording each result as it goes, then finishes the
// run with the aggregated summary. A per-case pipeline error is fail-soft: the
// case is recorded (as a recall miss / not grounded) and the run continues
// (SPEC-06 §8) — only a persistence error (create/record/finish) aborts.
func (r *Runner) Run(ctx context.Context, config map[string]any) (Summary, error) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	cases, err := r.Cases.cases(ctx)
	if err != nil {
		return Summary{}, err
	}
	runID, err := r.Sink.create(ctx, config)
	if err != nil {
		return Summary{}, err
	}

	results := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		res := r.runCase(ctx, now, c)
		if err := r.Sink.record(ctx, runID, res); err != nil {
			return Summary{}, err
		}
		results = append(results, res)
	}

	summary := summarize(r.K, results)
	if err := r.Sink.finish(ctx, runID, summary); err != nil {
		return Summary{}, err
	}
	summary.RunID = runID
	return summary, nil
}

// runCase runs one case: retrieve (for recall@k), then time the answer call. Any
// pipeline error is captured on the result and left to the caller's fail-soft
// loop; recall_hit is computed against whatever was retrieved (empty on error →
// a miss for a case that has expected docs).
func (r *Runner) runCase(ctx context.Context, now func() time.Time, c Case) CaseResult {
	res := CaseResult{CaseID: c.ID}

	retrieved, rerr := r.Pipeline.Retrieve(ctx, c.Question, r.K)
	res.RetrievedDocIDs = distinct(retrieved)
	res.RecallHit = recallHit(c.ExpectedDocIDs, res.RetrievedDocIDs)

	start := now()
	grounded, ans, aerr := r.Pipeline.Answer(ctx, c.Question, r.K)
	res.LatencyMs = int(now().Sub(start).Milliseconds())
	res.Grounded = grounded
	res.Answer = ans

	switch {
	case rerr != nil:
		res.Err = rerr
	case aerr != nil:
		res.Err = aerr
	}

	// LLM-as-judge (STORY-12.3): only when a judge is wired, the case has ground
	// truth (a non-empty expected answer), and an answer was actually produced.
	// A judge error leaves judged_correct NULL (fail-soft), never a silent pass.
	if r.Judge != nil && res.Err == nil && c.ExpectedAnswer != nil && strings.TrimSpace(*c.ExpectedAnswer) != "" {
		if correct, jerr := r.Judge.Judge(ctx, c.Question, *c.ExpectedAnswer, res.Answer); jerr == nil {
			res.JudgedCorrect = &correct
		}
	}
	return res
}

// distinct returns the ids in first-seen order with duplicates and empties
// removed — the document set behind the top-k retrieved chunks.
func distinct(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// recallHit implements the recall@k rule (ADR-0070): a case with no
// expected_doc_ids is excluded from recall (nil); otherwise it is a hit when at
// least one expected document appears in the retrieved set.
func recallHit(expected, retrieved []string) *bool {
	if len(expected) == 0 {
		return nil
	}
	set := make(map[string]bool, len(retrieved))
	for _, id := range retrieved {
		set[id] = true
	}
	hit := false
	for _, e := range expected {
		if set[e] {
			hit = true
			break
		}
	}
	return &hit
}

// summarize aggregates per-case results into the run summary. recall@k is over
// the cases that HAVE expected docs (nil-recall cases excluded from both numerator
// and denominator); grounded rate and mean latency are over all cases run.
func summarize(k int, results []CaseResult) Summary {
	s := Summary{Cases: len(results), K: k}
	var latencySum, grounded, recallHits, recallDenom, errs, judged, judgedCorrect int
	for _, r := range results {
		latencySum += r.LatencyMs
		if r.Grounded {
			grounded++
		}
		if r.Err != nil {
			errs++
		}
		if r.RecallHit != nil {
			recallDenom++
			if *r.RecallHit {
				recallHits++
			}
		}
		if r.JudgedCorrect != nil {
			judged++
			if *r.JudgedCorrect {
				judgedCorrect++
			}
		}
	}
	s.Errors = errs
	s.CasesScoredForRecall = recallDenom
	if recallDenom > 0 {
		s.RecallAtK = float64(recallHits) / float64(recallDenom)
	}
	s.CasesJudged = judged
	if judged > 0 {
		s.CorrectnessRate = float64(judgedCorrect) / float64(judged)
	}
	if len(results) > 0 {
		s.GroundedRate = float64(grounded) / float64(len(results))
		s.MeanLatencyMs = latencySum / len(results)
	}
	return s
}
