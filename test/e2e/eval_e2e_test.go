//go:build e2e

// STORY-12.1 golden path: the per-tenant eval-case store (internal/eval,
// FR-ADM-04) against a REAL enrolled tenant database (up via `mise run up`),
// reached ONLY through a resolver + *tenant.DB (ADR-0003, C-3) — no mocks. It
// proves the real SQL round-trip the unit tests cannot cover:
//   - Create/Get/List/Update/Delete map cleanly onto eval_cases, including the
//     uuid[] expected_doc_ids and text[] tags columns,
//   - CSV import (ADR-0069) creates rows and upserts by id in one transaction,
//   - the `ragctl eval import` CLI command drives the same path end to end.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/eval"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestEvalCasesGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "eval-" + suffix
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
	if out, exit := runEnroll(t, ageKey, blob, slug, "Eval "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	tenantID := tenantScalar(t, slug, "t.id")

	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher, CacheTTL: 50 * time.Millisecond})
	svc := eval.NewService(resolver, eval.NewTenantStore())
	tid := tenant.ID(uuid.MustParse(tenantID))

	// --- Create with an expected answer, doc ids and tags. ---
	docA := uuid.NewString()
	docB := uuid.NewString()
	ans := "Refunds within 30 days."
	created, err := svc.Create(ctx, tid, eval.CaseInput{
		Question:       "What is the refund policy?",
		ExpectedAnswer: &ans,
		ExpectedDocIDs: []string{docA, docB},
		Tags:           []string{"billing", "policy"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || len(created.ExpectedDocIDs) != 2 || len(created.Tags) != 2 {
		t.Fatalf("Create returned %+v", created)
	}

	// --- Get round-trips the arrays and the answer. ---
	got, err := svc.Get(ctx, tid, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExpectedAnswer == nil || *got.ExpectedAnswer != ans {
		t.Fatalf("Get expected_answer = %v", got.ExpectedAnswer)
	}
	if got.ExpectedDocIDs[0] != docA || got.ExpectedDocIDs[1] != docB {
		t.Fatalf("Get expected_doc_ids = %v", got.ExpectedDocIDs)
	}

	// --- Update replaces mutable fields. ---
	upd, err := svc.Update(ctx, tid, created.ID, eval.CaseInput{
		Question: "What is the updated refund policy?",
		Tags:     []string{"billing"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Question != "What is the updated refund policy?" || len(upd.Tags) != 1 || len(upd.ExpectedDocIDs) != 0 {
		t.Fatalf("Update returned %+v", upd)
	}
	if upd.ExpectedAnswer != nil {
		t.Fatalf("Update should have cleared expected_answer, got %v", *upd.ExpectedAnswer)
	}

	// --- List sees the one case. ---
	list, err := svc.List(ctx, tid, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}

	// --- CSV import: one new case, plus an upsert of the existing case by id. ---
	csv := strings.Join([]string{
		"id,question,expected_answer,expected_doc_ids,tags",
		",How do I reset my password?,Use the reset link.," + docA + ",account",
		created.ID + ",Upserted question,,," + "policy",
	}, "\n") + "\n"
	res, err := svc.Import(ctx, tid, strings.NewReader(csv))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Created != 1 || res.Updated != 1 {
		t.Fatalf("Import result = %+v, want {Created:1 Updated:1}", res)
	}
	after, err := svc.List(ctx, tid, 100)
	if err != nil {
		t.Fatalf("List after import: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("case count after import = %d, want 2", len(after))
	}
	// The upserted case kept its id and took the new question.
	up, err := svc.Get(ctx, tid, created.ID)
	if err != nil {
		t.Fatalf("Get upserted: %v", err)
	}
	if up.Question != "Upserted question" {
		t.Fatalf("upserted question = %q", up.Question)
	}

	// --- Delete is idempotent. ---
	if err := svc.Delete(ctx, tid, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := svc.Delete(ctx, tid, created.ID); err == nil {
		t.Fatal("second Delete should report ErrNotFound")
	}

	// --- The `ragctl eval import` CLI drives the same path end to end. ---
	runEvalImportCLI(t, ageKey, blob, slug, "How is data backed up?,pgBackRest to MinIO.")
	final, err := svc.List(ctx, tid, 100)
	if err != nil {
		t.Fatalf("List after CLI import: %v", err)
	}
	if len(final) != 2 {
		t.Fatalf("case count after CLI import = %d, want 2", len(final))
	}
}

// runEvalImportCLI writes a small CSV and runs the real ragctl binary to import
// it, asserting a clean exit. row is "question,expected_answer".
func runEvalImportCLI(t *testing.T, ageKey, blob, slug, row string) {
	t.Helper()
	bin := buildRagctl(t)
	path := filepath.Join(t.TempDir(), "cases.csv")
	if err := os.WriteFile(path, []byte("question,expected_answer\n"+row+"\n"), 0o600); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "eval", "import", "--slug", slug, "--file", path)
	env := dekEnv()
	for _, k := range []string{"CONTROL_PLANE_URL", "PROVISION_DB_URL"} {
		env = withoutEnv(env, k)
	}
	env = append(env,
		"CONTROL_PLANE_URL="+controlPlaneURL(),
		"KMS_PROVIDER=local",
		"AGE_SECRET_KEY="+ageKey,
		"DEK_WRAPPED_PATH="+blob,
		"DEK_KEY_VERSION=1",
	)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("eval import failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "1 created") {
		t.Fatalf("eval import output = %q, want a created count", string(out))
	}
}
