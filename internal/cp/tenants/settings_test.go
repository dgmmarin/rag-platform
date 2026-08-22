package tenants

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// fakeDB is a minimal SettingsDB for unit-testing the branch logic without
// Postgres. It records the settings JSON written and the audit row appended.
type fakeDB struct {
	current       []byte // tenants.settings as stored
	wroteSettings []byte // last settings written by an update
	auditRows     []auditRow
	updateRows    int64 // RowsAffected returned by the settings update
	failUpdate    error
}

type auditRow struct {
	tenantID string
	action   string
	target   string
	details  []byte
}

type fakeTag struct{ n int64 }

func (t fakeTag) RowsAffected() int64 { return t.n }

func (f *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) rowScanner {
	return fakeRow{db: f, sql: sql}
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (commandTag, error) {
	switch {
	case containsInsertAudit(sql):
		r := auditRow{}
		if len(args) >= 1 {
			r.tenantID, _ = args[0].(string)
		}
		if len(args) >= 2 {
			r.action, _ = args[1].(string)
		}
		if len(args) >= 4 {
			r.target, _ = args[3].(string)
		}
		if len(args) >= 5 {
			r.details, _ = args[4].([]byte)
		}
		f.auditRows = append(f.auditRows, r)
		return fakeTag{n: 1}, nil
	default:
		if f.failUpdate != nil {
			return fakeTag{}, f.failUpdate
		}
		// The settings update: `update tenants set settings = $1 where id = $2`.
		if len(args) >= 1 {
			if b, ok := args[0].([]byte); ok {
				f.wroteSettings = b
			}
		}
		return fakeTag{n: f.updateRows}, nil
	}
}

type fakeRow struct {
	db  *fakeDB
	sql string
}

func (r fakeRow) Scan(dest ...any) error {
	// The service reads the current settings document as a single json column.
	if len(dest) == 1 {
		if p, ok := dest[0].(*[]byte); ok {
			*p = r.db.current
			return nil
		}
	}
	return errors.New("unexpected scan")
}

func containsInsertAudit(sql string) bool {
	return len(sql) >= 6 && (sql[:6] == "insert" || sql[:6] == "INSERT") && contains(sql, "audit_log")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func provisionedSettings(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"embedding_dim": 1024,
		"embedding":     map[string]any{"provider": "voyage", "model": "voyage-3", "dim": 1024},
		"retrieval":     map[string]any{"k_vector": 40, "k_text": 40, "final_k": 8, "min_score": 0.02},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestService(db SettingsDB) *SettingsService {
	return &SettingsService{DB: db}
}

// Get returns the stored document with embedding.dim projected from the immutable
// flat embedding_dim when the nested value is absent.
func TestGetProjectsEmbeddingDim(t *testing.T) {
	db := &fakeDB{current: []byte(`{"embedding_dim":1536}`)}
	svc := newTestService(db)

	doc, err := svc.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	emb, ok := doc["embedding"].(map[string]any)
	if !ok {
		t.Fatalf("embedding missing/mistyped: %#v", doc["embedding"])
	}
	if got := jsonNum(emb["dim"]); got != 1536 {
		t.Fatalf("embedding.dim = %v, want 1536", got)
	}
	// The flat mirror is not leaked in the spec-shaped view.
	if _, leaked := doc["embedding_dim"]; leaked {
		t.Errorf("flat embedding_dim leaked into settings view")
	}
}

// A tenant provisioned with only the flat embedding_dim mirror (what the
// provisioner writes) yields a complete, schema-valid SPEC-02 §5 document from
// Get: defaults fill every unset field and embedding.dim reflects the provisioned
// dimension.
func TestGetFillsDefaultsForFreshlyProvisioned(t *testing.T) {
	db := &fakeDB{current: []byte(`{"embedding_dim":1024}`)}
	svc := newTestService(db)

	doc, err := svc.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := validateSettings(doc); err != nil {
		t.Fatalf("defaulted document does not validate: %v", err)
	}
	emb := doc["embedding"].(map[string]any)
	if jsonNum(emb["dim"]) != 1024 {
		t.Fatalf("embedding.dim = %v, want 1024", emb["dim"])
	}
	if emb["provider"] == nil || emb["model"] == nil {
		t.Fatalf("embedding provider/model not defaulted: %#v", emb)
	}
	if doc["retrieval"] == nil || doc["limits"] == nil {
		t.Fatalf("defaults not applied: %#v", doc)
	}
}

// Patching one field on a freshly provisioned tenant succeeds: defaults make the
// merged document complete and valid.
func TestPatchOnFreshlyProvisionedFillsDefaults(t *testing.T) {
	db := &fakeDB{current: []byte(`{"embedding_dim":1024}`), updateRows: 1}
	svc := newTestService(db)

	patch := map[string]any{"llm": map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6", "max_tokens": 2048}}
	got, err := svc.Patch(context.Background(), PatchParams{TenantID: "t1", Patch: patch})
	if err != nil {
		t.Fatalf("patch on fresh tenant: %v", err)
	}
	llm := got["llm"].(map[string]any)
	if jsonNum(llm["max_tokens"]) != 2048 {
		t.Fatalf("max_tokens = %v, want 2048", llm["max_tokens"])
	}
	if got["retrieval"] == nil {
		t.Fatalf("defaults not present in merged doc")
	}
}

// A valid partial patch is merged, persisted, and audited.
func TestPatchMergesValidatesAndAudits(t *testing.T) {
	db := &fakeDB{current: provisionedSettings(t), updateRows: 1}
	svc := newTestService(db)

	patch := map[string]any{"retrieval": map[string]any{"k_vector": 40, "k_text": 40, "final_k": 12, "min_score": 0.05}}
	_, err := svc.Patch(context.Background(), PatchParams{TenantID: "t1", Actor: Actor{UserID: strp("u1")}, Patch: patch})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if db.wroteSettings == nil {
		t.Fatal("no settings written")
	}
	var stored map[string]any
	if err := json.Unmarshal(db.wroteSettings, &stored); err != nil {
		t.Fatal(err)
	}
	ret := stored["retrieval"].(map[string]any)
	if jsonNum(ret["final_k"]) != 12 {
		t.Errorf("final_k not merged: %v", ret["final_k"])
	}
	// The flat mirror is preserved so provisioning/migration keep working.
	if jsonNum(stored["embedding_dim"]) != 1024 {
		t.Errorf("flat embedding_dim not preserved: %v", stored["embedding_dim"])
	}
	if len(db.auditRows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(db.auditRows))
	}
	if db.auditRows[0].action != "settings.update" {
		t.Errorf("audit action = %q, want settings.update", db.auditRows[0].action)
	}
	if db.auditRows[0].tenantID != "t1" {
		t.Errorf("audit tenant = %q", db.auditRows[0].tenantID)
	}
}

// An invalid patch is rejected before any write or audit.
func TestPatchRejectsInvalidWithoutWriting(t *testing.T) {
	db := &fakeDB{current: provisionedSettings(t), updateRows: 1}
	svc := newTestService(db)

	patch := map[string]any{"retrieval": map[string]any{"k_vector": 40, "k_text": 40, "final_k": 8, "min_score": 9}}
	_, err := svc.Patch(context.Background(), PatchParams{TenantID: "t1", Patch: patch})
	var ve *ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v (%T), want *ValidationErrors", err, err)
	}
	if db.wroteSettings != nil {
		t.Error("settings written despite invalid patch")
	}
	if len(db.auditRows) != 0 {
		t.Error("audit written despite invalid patch")
	}
}

// Changing embedding.dim is refused with ErrImmutableField and no write.
func TestPatchRefusesEmbeddingDimChange(t *testing.T) {
	db := &fakeDB{current: provisionedSettings(t), updateRows: 1}
	svc := newTestService(db)

	patch := map[string]any{"embedding": map[string]any{"provider": "voyage", "model": "voyage-3", "dim": 2048}}
	_, err := svc.Patch(context.Background(), PatchParams{TenantID: "t1", Patch: patch})
	if !errors.Is(err, ErrImmutableField) {
		t.Fatalf("error = %v, want ErrImmutableField", err)
	}
	if db.wroteSettings != nil {
		t.Error("settings written despite immutable-field change")
	}
	if len(db.auditRows) != 0 {
		t.Error("audit written despite rejected change")
	}
}

// Re-sending the same embedding.dim (no change) is allowed.
func TestPatchAllowsSameEmbeddingDim(t *testing.T) {
	db := &fakeDB{current: provisionedSettings(t), updateRows: 1}
	svc := newTestService(db)

	patch := map[string]any{"embedding": map[string]any{"provider": "voyage", "model": "voyage-3-large", "dim": 1024}}
	if _, err := svc.Patch(context.Background(), PatchParams{TenantID: "t1", Patch: patch}); err != nil {
		t.Fatalf("patch with same dim rejected: %v", err)
	}
	var stored map[string]any
	_ = json.Unmarshal(db.wroteSettings, &stored)
	emb := stored["embedding"].(map[string]any)
	if emb["model"] != "voyage-3-large" {
		t.Errorf("model not updated: %v", emb["model"])
	}
}

func jsonNum(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return -1
	}
}

func strp(s string) *string { return &s }
