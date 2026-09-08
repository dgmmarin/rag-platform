package eval

import (
	"context"
	"encoding/json"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// RunWriter is the tenant-content persistence port for eval_runs/eval_results
// (STORY-12.2). Like the eval Store (ADR-0003, C-3) it is reached ONLY through a
// *tenant.DB. RunStore implements it over a live tenant database; the SQL is
// exercised by the e2e suite.
type RunWriter interface {
	// CreateRun opens a run row with the effective config and returns its id.
	CreateRun(ctx context.Context, db *tenant.DB, config map[string]any) (runID string, err error)
	// RecordResult writes one eval_results row. judged_correct is left NULL
	// (STORY-12.3).
	RecordResult(ctx context.Context, db *tenant.DB, runID string, r CaseResult) error
	// FinishRun stamps finished_at and stores the summary.
	FinishRun(ctx context.Context, db *tenant.DB, runID string, s Summary) error
}

// RunStore is the production RunWriter over a tenant database.
type RunStore struct{}

// NewRunStore builds the tenant-schema run store.
func NewRunStore() RunStore { return RunStore{} }

// CreateRun inserts an eval_runs row and returns its id. A nil config is stored
// as an empty JSON object (the column is NOT NULL).
func (RunStore) CreateRun(ctx context.Context, db *tenant.DB, config map[string]any) (string, error) {
	body := []byte("{}")
	if config != nil {
		b, err := json.Marshal(config)
		if err != nil {
			return "", err
		}
		body = b
	}
	var id string
	err := db.QueryRow(ctx,
		`insert into eval_runs (config) values ($1) returning id::text`, body).Scan(&id)
	return id, err
}

// RecordResult inserts one eval_results row. recall_hit and judged_correct are
// *bool so a nil (no ground truth, or a fail-soft judge/pipeline error) is written
// as NULL; retrieved_doc_ids is cast to uuid[]. judged_correct is nil unless the
// LLM judge scored the case (STORY-12.3).
func (RunStore) RecordResult(ctx context.Context, db *tenant.DB, runID string, r CaseResult) error {
	var answer *string
	if r.Answer != "" {
		answer = &r.Answer
	}
	_, err := db.Exec(ctx, `
		insert into eval_results (run_id, case_id, retrieved_doc_ids, recall_hit, answer, judged_correct, latency_ms)
		values ($1, $2, $3::uuid[], $4, $5, $6, $7)`,
		runID, r.CaseID, docIDsParam(r.RetrievedDocIDs), r.RecallHit, answer, r.JudgedCorrect, r.LatencyMs)
	return err
}

// FinishRun stamps finished_at and stores the summary JSON.
func (RunStore) FinishRun(ctx context.Context, db *tenant.DB, runID string, s Summary) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx,
		`update eval_runs set finished_at = now(), summary = $2 where id = $1`, runID, body)
	return err
}

// --- DB-backed adapters binding a *tenant.DB to the runner's ports. ---

type dbCaseSource struct {
	store Store
	db    *tenant.DB
	limit int
}

func (d dbCaseSource) cases(ctx context.Context) ([]Case, error) {
	return d.store.List(ctx, d.db, d.limit)
}

type dbRunSink struct {
	runs RunWriter
	db   *tenant.DB
}

func (d dbRunSink) create(ctx context.Context, config map[string]any) (string, error) {
	return d.runs.CreateRun(ctx, d.db, config)
}

func (d dbRunSink) record(ctx context.Context, runID string, r CaseResult) error {
	return d.runs.RecordResult(ctx, d.db, runID, r)
}

func (d dbRunSink) finish(ctx context.Context, runID string, s Summary) error {
	return d.runs.FinishRun(ctx, d.db, runID, s)
}
