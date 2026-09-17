package eval

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// This file is the STORY-12.4 machine-readable eval REPORT (FR-ADM-04): a stored
// run plus its per-case results, read from eval_runs/eval_results (tenant content,
// reached only through a *tenant.DB — ADR-0003, C-3) and serialised as JSON. This
// is the data contract the EPIC-11 admin UI will render; the UI rendering itself
// is deferred to EPIC-11 (ADR-0072).

// RunView is a stored run's header (eval_runs). Config and Summary are passed
// through verbatim as raw JSON so the report never re-shapes what the run stored.
type RunView struct {
	ID         string          `json:"id"`
	Config     json.RawMessage `json:"config"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Summary    json.RawMessage `json:"summary,omitempty"`
}

// ResultView is one case's stored result (eval_results) enriched with the case's
// question/expected_answer (LEFT JOIN eval_cases, so a case deleted since the run
// still shows its result with a null question).
type ResultView struct {
	CaseID          string   `json:"case_id"`
	Question        *string  `json:"question,omitempty"`
	ExpectedAnswer  *string  `json:"expected_answer,omitempty"`
	RetrievedDocIDs []string `json:"retrieved_doc_ids"`
	RecallHit       *bool    `json:"recall_hit"`
	JudgedCorrect   *bool    `json:"judged_correct"`
	Answer          *string  `json:"answer,omitempty"`
	LatencyMs       int      `json:"latency_ms"`
}

// Report is the full machine-readable view of a run.
type Report struct {
	Run     RunView      `json:"run"`
	Results []ResultView `json:"results"`
}

// ReportReader reads stored runs and their results. RunStore implements it over a
// tenant database; the SQL is exercised by the e2e suite.
type ReportReader interface {
	ListRuns(ctx context.Context, db *tenant.DB, limit int) ([]RunView, error)
	GetRun(ctx context.Context, db *tenant.DB, runID string) (RunView, error)
	Results(ctx context.Context, db *tenant.DB, runID string) ([]ResultView, error)
}

// defaultRunListLimit caps a runs listing when the caller passes 0.
const defaultRunListLimit = 50

// ListRuns reads run headers newest-first (started_at desc), for the admin report's
// runs list (STORY-12.4). A non-positive limit falls back to defaultRunListLimit.
func (RunStore) ListRuns(ctx context.Context, db *tenant.DB, limit int) ([]RunView, error) {
	if limit <= 0 {
		limit = defaultRunListLimit
	}
	rows, err := db.Query(ctx,
		`select id::text, config, started_at, finished_at, summary
		 from eval_runs order by started_at desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RunView
	for rows.Next() {
		var (
			rv     RunView
			config []byte
			sumry  []byte
		)
		if err := rows.Scan(&rv.ID, &config, &rv.StartedAt, &rv.FinishedAt, &sumry); err != nil {
			return nil, err
		}
		rv.Config = json.RawMessage(config)
		if sumry != nil {
			rv.Summary = json.RawMessage(sumry)
		}
		out = append(out, rv)
	}
	return out, rows.Err()
}

// GetRun reads a run header, or ErrNotFound.
func (RunStore) GetRun(ctx context.Context, db *tenant.DB, runID string) (RunView, error) {
	if !validUUID(runID) {
		return RunView{}, ErrNotFound
	}
	var (
		rv     RunView
		config []byte
		sumry  []byte
	)
	err := db.QueryRow(ctx,
		`select id::text, config, started_at, finished_at, summary from eval_runs where id = $1`, runID).
		Scan(&rv.ID, &config, &rv.StartedAt, &rv.FinishedAt, &sumry)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunView{}, ErrNotFound
	}
	if err != nil {
		return RunView{}, err
	}
	rv.Config = json.RawMessage(config)
	if sumry != nil {
		rv.Summary = json.RawMessage(sumry)
	}
	return rv, nil
}

// Results reads a run's per-case results, joined to eval_cases for the question
// and expected answer, ordered by the case's creation for a stable report.
func (RunStore) Results(ctx context.Context, db *tenant.DB, runID string) ([]ResultView, error) {
	if !validUUID(runID) {
		return nil, ErrNotFound
	}
	rows, err := db.Query(ctx, `
		select r.case_id::text, c.question, c.expected_answer,
		       r.retrieved_doc_ids::text[], r.recall_hit, r.judged_correct, r.answer, r.latency_ms
		from eval_results r
		left join eval_cases c on c.id = r.case_id
		where r.run_id = $1
		order by c.created_at nulls last, r.case_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ResultView
	for rows.Next() {
		rv, err := scanReportResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
	}
	return out, rows.Err()
}

// scanReportResult maps one joined result row into a ResultView. Nullable columns
// (question, expected_answer, recall_hit, judged_correct, answer) scan to nil
// pointers; an empty array scans to a nil slice.
func scanReportResult(row rowScanner) (ResultView, error) {
	var (
		rv   ResultView
		q    *string
		exp  *string
		docs []string
		rh   *bool
		jc   *bool
		ans  *string
		lat  int
	)
	if err := row.Scan(&rv.CaseID, &q, &exp, &docs, &rh, &jc, &ans, &lat); err != nil {
		return ResultView{}, err
	}
	rv.Question = q
	rv.ExpectedAnswer = exp
	rv.RetrievedDocIDs = docs
	rv.RecallHit = rh
	rv.JudgedCorrect = jc
	rv.Answer = ans
	rv.LatencyMs = lat
	return rv, nil
}
