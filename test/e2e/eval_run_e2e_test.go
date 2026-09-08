//go:build e2e

// STORY-12.2 DB write path: the eval RUNNER + RunStore (eval_runs/eval_results)
// against a REAL enrolled tenant database (up via `mise run up`), reached ONLY
// through a resolver + *tenant.DB (ADR-0003, C-3). The retrieval/answering
// pipeline is injected as a deterministic fake (eval.Pipeline port) so the test
// exercises the persistence + scoring without LLM/embedding keys — the real
// pipeline wiring is the CLI composition root, covered by unit tests.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/eval"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// fakeRunPipeline is a deterministic eval.Pipeline for the write-path e2e.
type fakeRunPipeline struct {
	docs     map[string][]string
	grounded map[string]bool
}

func (f fakeRunPipeline) Retrieve(_ context.Context, q string, _ int) ([]string, error) {
	return f.docs[q], nil
}
func (f fakeRunPipeline) Answer(_ context.Context, q string, _ int) (bool, string, error) {
	return f.grounded[q], "answer to " + q, nil
}

func TestEvalRunWritePath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "evalrun-" + suffix
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		dbName := tryScalar(slug, "d.database_name")
		role := tryScalar(slug, "d.username")
		if dbName != "" {
			_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		}
		if role != "" {
			_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
		}
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE slug = '%s'", slug))
	})
	const dim = 768
	if out, exit := runEnroll(t, ageKey, blob, slug, "EvalRun "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	tenantID := tenantScalar(t, slug, "t.id")

	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher, CacheTTL: 50 * time.Millisecond})
	svc := eval.NewService(resolver, eval.NewTenantStore())
	svc.Runs = eval.NewRunStore()
	tid := tenant.ID(uuid.MustParse(tenantID))

	// --- Seed three cases: one whose expected doc will be retrieved (hit), one
	// whose expected doc will not (miss), and one with no expected docs (excluded
	// from recall). ---
	docHit := uuid.NewString()
	docMiss := uuid.NewString()
	c1, err := svc.Create(ctx, tid, eval.CaseInput{Question: "q-hit", ExpectedDocIDs: []string{docHit}})
	if err != nil {
		t.Fatalf("create c1: %v", err)
	}
	c2, err := svc.Create(ctx, tid, eval.CaseInput{Question: "q-miss", ExpectedDocIDs: []string{docMiss}})
	if err != nil {
		t.Fatalf("create c2: %v", err)
	}
	c3, err := svc.Create(ctx, tid, eval.CaseInput{Question: "q-none"})
	if err != nil {
		t.Fatalf("create c3: %v", err)
	}

	pipe := fakeRunPipeline{
		docs: map[string][]string{
			"q-hit":  {docHit, docHit}, // duplicate → stored distinct
			"q-miss": {uuid.NewString()},
			"q-none": {uuid.NewString()},
		},
		grounded: map[string]bool{"q-hit": true, "q-miss": true, "q-none": false},
	}

	summary, err := svc.Run(ctx, tid, eval.RunOptions{
		Pipeline: pipe,
		K:        8,
		Config:   map[string]any{"retrieval": map[string]any{"final_k": 8}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// --- Summary math. ---
	if summary.Cases != 3 {
		t.Errorf("cases = %d, want 3", summary.Cases)
	}
	if summary.CasesScoredForRecall != 2 || summary.RecallAtK != 0.5 {
		t.Errorf("recall@k = %v over %d, want 0.5 over 2", summary.RecallAtK, summary.CasesScoredForRecall)
	}
	if summary.GroundedRate < 0.66 || summary.GroundedRate > 0.67 {
		t.Errorf("grounded rate = %v, want ~0.667", summary.GroundedRate)
	}
	if summary.RunID == "" {
		t.Fatal("summary has no run id")
	}

	// --- Persistence: one finished run, three results, recall flags correct. ---
	db, err := resolver.Open(ctx, tid)
	if err != nil {
		t.Fatalf("open tenant db: %v", err)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from eval_runs where finished_at is not null`); got != "1" {
		t.Fatalf("finished eval_runs = %s, want 1", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from eval_results where run_id = $1::uuid`, summary.RunID); got != "3" {
		t.Fatalf("eval_results rows = %s, want 3", got)
	}
	// c1 is a hit; c2 a miss; c3 NULL (no expected docs).
	if got := tenantScalarDB(ctx, t, db, `select recall_hit::text from eval_results where run_id = $1::uuid and case_id = $2::uuid`, summary.RunID, c1.ID); got != "true" {
		t.Errorf("c1 recall_hit = %q, want true", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select recall_hit::text from eval_results where run_id = $1::uuid and case_id = $2::uuid`, summary.RunID, c2.ID); got != "false" {
		t.Errorf("c2 recall_hit = %q, want false", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select coalesce(recall_hit::text, 'NULL') from eval_results where run_id = $1::uuid and case_id = $2::uuid`, summary.RunID, c3.ID); got != "NULL" {
		t.Errorf("c3 recall_hit = %q, want NULL", got)
	}
	// c1 retrieved_doc_ids stored the DISTINCT doc (deduped from the duplicate).
	if got := tenantScalarDB(ctx, t, db, `select array_length(retrieved_doc_ids, 1)::text from eval_results where run_id = $1::uuid and case_id = $2::uuid`, summary.RunID, c1.ID); got != "1" {
		t.Errorf("c1 retrieved_doc_ids length = %q, want 1 (deduped)", got)
	}
	// judged_correct is left NULL (STORY-12.3, not this story).
	if got := tenantScalarDB(ctx, t, db, `select count(*) from eval_results where run_id = $1::uuid and judged_correct is not null`, summary.RunID); got != "0" {
		t.Errorf("judged_correct set on %s rows, want 0 (STORY-12.3)", got)
	}
	// The effective config was stored on the run.
	if got := tenantScalarDB(ctx, t, db, `select (config->'retrieval'->>'final_k') from eval_runs where id = $1::uuid`, summary.RunID); got != "8" {
		t.Errorf("stored config final_k = %q, want 8", got)
	}
}
