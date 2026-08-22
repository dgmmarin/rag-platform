//go:build e2e

// STORY-03.6 golden path: the audit log read API against the REAL control-plane
// Postgres (up via `mise run up`), no mocks. It proves:
//   - a real privileged action (settings.update) writes an audit row with actor,
//     tenant and target,
//   - GET /admin/audit?tenant= behind RequirePlatformAdmin returns that tenant's
//     entries to a platform admin,
//   - a non-platform-admin is refused 403 and a missing tenant param is 400.
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rag-platform/ragctl/internal/cp/audit"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestAuditLogGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "aud-" + suffix

	// Seed a tenant provisioned at dim 1024 (the flat mirror the provisioner writes).
	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region, settings)
		 values ($1, $2, 'active', 'eu-central', jsonb_build_object('embedding_dim', 1024::int))
		 returning id::text`,
		slug, "Audit Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	// Seed two users: a platform admin (may read any tenant's audit) and a plain
	// member (may not).
	adminID := seedUser(t, ctx, pool, fmt.Sprintf("aud-admin-%s@example.test", suffix), true)
	memberID := seedUser(t, ctx, pool, fmt.Sprintf("aud-member-%s@example.test", suffix), false)

	// --- Produce a real audit row via a genuine privileged action. ---
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	settingsHandlers := tenants.NewSettingsHandlers(settingsSvc)
	tid := tenant.ID(uuid.MustParse(tenantID))
	{
		body := `{"retrieval":{"k_vector":40,"k_text":40,"final_k":12,"min_score":0.05}}`
		r := httptest.NewRequest(http.MethodPatch, "/v1/settings", strings.NewReader(body))
		c := tenant.WithTenantID(r.Context(), tid)
		c = auth.ContextWithSession(c, auth.Session{UserID: adminID})
		rr := httptest.NewRecorder()
		settingsHandlers.Patch(rr, r.WithContext(c))
		if rr.Code != http.StatusOK {
			t.Fatalf("seed settings.update = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
	}

	// The audit endpoint behind the real platform-admin middleware.
	authz := auth.NewAuthzService(auth.MembershipFromPool(pool))
	handlers := audit.NewHandlers(audit.NewService(audit.FromPool(pool)))
	endpoint := authz.RequirePlatformAdmin()(http.HandlerFunc(handlers.List))

	call := func(userID, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/admin/audit"+query, nil)
		if userID != "" {
			r = r.WithContext(auth.ContextWithSession(r.Context(), auth.Session{UserID: userID}))
		}
		rr := httptest.NewRecorder()
		endpoint.ServeHTTP(rr, r)
		return rr
	}

	// --- Platform admin reads the tenant's audit entries. ---
	{
		rr := call(adminID, "?tenant="+tenantID)
		if rr.Code != http.StatusOK {
			t.Fatalf("admin GET = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		var body struct {
			Entries []audit.Entry `json:"entries"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var got *audit.Entry
		for i := range body.Entries {
			if body.Entries[i].Action == "settings.update" {
				got = &body.Entries[i]
			}
		}
		if got == nil {
			t.Fatalf("settings.update entry not returned: %s", rr.Body.String())
		}
		if got.TenantID == nil || *got.TenantID != tenantID {
			t.Fatalf("entry tenant = %v, want %s", got.TenantID, tenantID)
		}
		if got.ActorUserID == nil || *got.ActorUserID != adminID {
			t.Fatalf("entry actor = %v, want %s", got.ActorUserID, adminID)
		}
		if got.TargetType == nil || *got.TargetType != "tenant" {
			t.Fatalf("entry target_type = %v, want tenant", got.TargetType)
		}
	}

	// --- A non-platform-admin is refused. ---
	{
		rr := call(memberID, "?tenant="+tenantID)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("member GET = %d, want 403; body=%s", rr.Code, rr.Body.String())
		}
	}

	// --- No session is refused. ---
	{
		rr := call("", "?tenant="+tenantID)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("anon GET = %d, want 401; body=%s", rr.Code, rr.Body.String())
		}
	}

	// --- Missing tenant param is a 400 (even for an admin). ---
	{
		rr := call(adminID, "")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("admin GET no tenant = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
	}
}

func seedUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string, platformAdmin bool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`insert into users (email, is_platform_admin) values ($1, $2) returning id::text`,
		email, platformAdmin).Scan(&id); err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM users WHERE id = '%s'", id))
	})
	return id
}
