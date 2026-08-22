//go:build e2e

// STORY-03.5 golden path: tenant settings with JSON-schema validation against the
// REAL control-plane Postgres (up via `mise run up`), no mocks. It drives the real
// tenants.SettingsService and SettingsHandlers through:
//   - GET the default settings of a freshly provisioned tenant (embedding.dim
//     projected from the flat embedding_dim mirror the provisioner writes),
//   - PATCH a valid change (persisted to tenants.settings + a settings.update
//     audit row written),
//   - reject an invalid document with field-level errors (nothing persisted),
//   - reject an attempt to change embedding.dim (409, nothing persisted).
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestTenantSettingsGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)
	svc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	handlers := tenants.NewSettingsHandlers(svc)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "set-" + suffix

	// Seed a tenant provisioned at dim 1024, mirroring what the provisioner
	// writes today: the flat settings.embedding_dim key (SPEC-01 §6/§7).
	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region, settings)
		 values ($1, $2, 'active', 'eu-central', jsonb_build_object('embedding_dim', 1024::int))
		 returning id::text`,
		slug, "Settings Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	// Seed an owner user for the audit actor.
	var userID string
	if err := pool.QueryRow(ctx,
		`insert into users (email) values ($1) returning id::text`,
		fmt.Sprintf("set-owner-%s@example.test", suffix)).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM users WHERE id = '%s'", userID))
	})

	tid := tenant.ID(uuid.MustParse(tenantID))
	reqCtx := func(method, body string) *http.Request {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, "/v1/settings", nil)
		} else {
			r = httptest.NewRequest(method, "/v1/settings", strings.NewReader(body))
		}
		c := tenant.WithTenantID(r.Context(), tid)
		c = auth.ContextWithSession(c, auth.Session{UserID: userID})
		return r.WithContext(c)
	}

	// --- GET default settings: embedding.dim projected from the flat mirror. ---
	{
		rr := httptest.NewRecorder()
		handlers.Get(rr, reqCtx(http.MethodGet, ""))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatalf("GET body: %v", err)
		}
		emb, ok := doc["embedding"].(map[string]any)
		if !ok {
			t.Fatalf("embedding missing from GET body: %s", rr.Body.String())
		}
		if emb["dim"].(float64) != 1024 {
			t.Fatalf("embedding.dim = %v, want 1024", emb["dim"])
		}
		if _, leaked := doc["embedding_dim"]; leaked {
			t.Fatalf("flat embedding_dim leaked in GET body: %s", rr.Body.String())
		}
	}

	// --- PATCH a valid change: persisted + audited. ---
	{
		body := `{"retrieval":{"k_vector":40,"k_text":40,"final_k":12,"min_score":0.05},"llm":{"provider":"anthropic","model":"claude-sonnet-4-6","max_tokens":2048}}`
		rr := httptest.NewRecorder()
		handlers.Patch(rr, reqCtx(http.MethodPatch, body))
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH valid status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		// Persisted: read back the raw column.
		got := psqlScalar(t, fmt.Sprintf(
			"select settings->'retrieval'->>'final_k' from tenants where id = '%s'", tenantID))
		if got != "12" {
			t.Fatalf("persisted final_k = %q, want 12", got)
		}
		// The flat mirror is preserved so the migrator/provisioner keep working.
		gotDim := psqlScalar(t, fmt.Sprintf(
			"select settings->>'embedding_dim' from tenants where id = '%s'", tenantID))
		if gotDim != "1024" {
			t.Fatalf("flat embedding_dim after patch = %q, want 1024", gotDim)
		}
		// Audited.
		n := psqlScalar(t, fmt.Sprintf(
			"select count(*) from audit_log where tenant_id = '%s' and action = 'settings.update'", tenantID))
		if n != "1" {
			t.Fatalf("settings.update audit rows = %q, want 1", n)
		}
	}

	// --- Reject an invalid document with field-level errors. ---
	{
		body := `{"retrieval":{"k_vector":40,"k_text":40,"final_k":8,"min_score":9}}`
		rr := httptest.NewRecorder()
		handlers.Patch(rr, reqCtx(http.MethodPatch, body))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("PATCH invalid status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Fields []tenants.FieldError `json:"fields"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid body: %v", err)
		}
		found := false
		for _, f := range resp.Fields {
			if f.Field == "retrieval.min_score" {
				found = true
			}
		}
		if !found {
			t.Fatalf("retrieval.min_score not in field errors: %s", rr.Body.String())
		}
		// Nothing persisted: final_k still 12, no new audit row.
		got := psqlScalar(t, fmt.Sprintf(
			"select settings->'retrieval'->>'final_k' from tenants where id = '%s'", tenantID))
		if got != "12" {
			t.Fatalf("invalid patch mutated final_k to %q", got)
		}
		n := psqlScalar(t, fmt.Sprintf(
			"select count(*) from audit_log where tenant_id = '%s' and action = 'settings.update'", tenantID))
		if n != "1" {
			t.Fatalf("invalid patch wrote an audit row (count %q)", n)
		}
	}

	// --- Reject an attempt to change embedding.dim (409, nothing persisted). ---
	{
		body := `{"embedding":{"provider":"voyage","model":"voyage-3","dim":2048}}`
		rr := httptest.NewRecorder()
		handlers.Patch(rr, reqCtx(http.MethodPatch, body))
		if rr.Code != http.StatusConflict {
			t.Fatalf("PATCH embedding.dim status = %d, want 409; body=%s", rr.Code, rr.Body.String())
		}
		gotDim := psqlScalar(t, fmt.Sprintf(
			"select settings->>'embedding_dim' from tenants where id = '%s'", tenantID))
		if gotDim != "1024" {
			t.Fatalf("embedding_dim changed to %q despite immutability guard", gotDim)
		}
		n := psqlScalar(t, fmt.Sprintf(
			"select count(*) from audit_log where tenant_id = '%s' and action = 'settings.update'", tenantID))
		if n != "1" {
			t.Fatalf("rejected immutable change wrote an audit row (count %q)", n)
		}
	}
}
