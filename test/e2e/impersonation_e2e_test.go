//go:build e2e

// STORY-03.8 golden path: platform-admin impersonation against the REAL
// control-plane Postgres (up via `mise run up`), no mocks. It proves:
//   - only a platform admin can start impersonation: a non-admin is refused 403 by
//     the real RequirePlatformAdmin middleware and no grant/audit row is written,
//   - the resulting grant carries BOTH the real admin and the impersonated
//     identity (never a silent identity swap, FR-ACC-07),
//   - starting writes an admin.impersonate audit row (actor=admin, target=user,
//     details.impersonation=true, SPEC-02 §4/§6),
//   - ending impersonation stamps the grant and writes admin.impersonate.end.
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rag-platform/ragctl/internal/cp/audit"
	"github.com/rag-platform/ragctl/internal/cp/auth"
)

func TestImpersonationGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "imp-" + suffix

	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region, settings)
		 values ($1, $2, 'active', 'eu-central', jsonb_build_object('embedding_dim', 1024::int))
		 returning id::text`,
		slug, "Impersonation Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	adminID := seedUser(t, ctx, pool, fmt.Sprintf("imp-admin-%s@example.test", suffix), true)
	memberID := seedUser(t, ctx, pool, fmt.Sprintf("imp-member-%s@example.test", suffix), false)
	targetID := seedUser(t, ctx, pool, fmt.Sprintf("imp-target-%s@example.test", suffix), false)

	svc := auth.NewImpersonationService(auth.FromPool(pool), audit.FromPool(pool))
	handlers := auth.NewImpersonationHandlers(svc)
	authz := auth.NewAuthzService(auth.MembershipFromPool(pool))

	// The Start endpoint behind the real platform-admin middleware.
	startEndpoint := authz.RequirePlatformAdmin()(http.HandlerFunc(handlers.Start))
	callStart := func(userID string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"tenant_id":%q,"user_id":%q}`, tenantID, targetID)
		r := httptest.NewRequest(http.MethodPost, "/admin/impersonations", strings.NewReader(body))
		if userID != "" {
			r = r.WithContext(auth.ContextWithSession(r.Context(), auth.Session{UserID: userID}))
		}
		rr := httptest.NewRecorder()
		startEndpoint.ServeHTTP(rr, r)
		return rr
	}

	// --- A non-platform-admin is refused; no grant is created. ---
	{
		rr := callStart(memberID)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("member start = %d, want 403; body=%s", rr.Code, rr.Body.String())
		}
		var n int
		if err := pool.QueryRow(ctx,
			`select count(*) from impersonation_sessions where admin_user_id = $1`, memberID).Scan(&n); err != nil {
			t.Fatalf("count grants: %v", err)
		}
		if n != 0 {
			t.Fatalf("non-admin created %d grants, want 0", n)
		}
	}

	// --- A platform admin starts impersonation; the grant carries both identities. ---
	var grantID string
	{
		rr := callStart(adminID)
		if rr.Code != http.StatusCreated {
			t.Fatalf("admin start = %d, want 201; body=%s", rr.Code, rr.Body.String())
		}
		var resp struct {
			ID                 string `json:"id"`
			AdminUserID        string `json:"admin_user_id"`
			TenantID           string `json:"tenant_id"`
			ImpersonatedUserID string `json:"impersonated_user_id"`
			ExpiresAt          string `json:"expires_at"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode start: %v", err)
		}
		if resp.AdminUserID != adminID || resp.TenantID != tenantID || resp.ImpersonatedUserID != targetID {
			t.Fatalf("grant does not carry both identities: %+v", resp)
		}
		if resp.ExpiresAt == "" {
			t.Fatalf("grant is not time-bounded: %+v", resp)
		}
		grantID = resp.ID
	}

	// --- The persisted grant records both the admin and the impersonated user. ---
	{
		var admin, ten, target string
		var ended *time.Time
		if err := pool.QueryRow(ctx,
			`select admin_user_id::text, tenant_id::text, impersonated_user_id::text, ended_at
			 from impersonation_sessions where id = $1`, grantID).Scan(&admin, &ten, &target, &ended); err != nil {
			t.Fatalf("read grant: %v", err)
		}
		if admin != adminID || ten != tenantID || target != targetID {
			t.Fatalf("persisted grant mismatch: admin=%s tenant=%s target=%s", admin, ten, target)
		}
		if ended != nil {
			t.Fatalf("fresh grant already ended: %v", ended)
		}
	}

	// --- Starting wrote an admin.impersonate audit row attributed to the admin. ---
	assertImpersonationAudit(ctx, t, pool, tenantID, "admin.impersonate", adminID, targetID)

	// --- Ending impersonation stamps the grant and audits the end. ---
	{
		r := httptest.NewRequest(http.MethodDelete, "/admin/impersonations/"+grantID, nil)
		r = r.WithContext(auth.ContextWithSession(r.Context(), auth.Session{UserID: adminID}))
		r.SetPathValue("id", grantID)
		endEndpoint := authz.RequirePlatformAdmin()(http.HandlerFunc(handlers.End))
		rr := httptest.NewRecorder()
		endEndpoint.ServeHTTP(rr, r)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("end = %d, want 204; body=%s", rr.Code, rr.Body.String())
		}
		var ended *time.Time
		if err := pool.QueryRow(ctx,
			`select ended_at from impersonation_sessions where id = $1`, grantID).Scan(&ended); err != nil {
			t.Fatalf("read ended grant: %v", err)
		}
		if ended == nil {
			t.Fatalf("End did not stamp ended_at")
		}
	}
	assertImpersonationAudit(ctx, t, pool, tenantID, "admin.impersonate.end", adminID, targetID)
}

// assertImpersonationAudit asserts an audit row exists for the action, attributed
// to the real admin (never the impersonated user), targeting the impersonated
// user, scoped to the tenant, with details.impersonation=true (SPEC-02 §4/§6).
func assertImpersonationAudit(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tenantID, action, adminID, targetID string) {
	t.Helper()
	var actor, target *string
	var details []byte
	err := pool.QueryRow(ctx,
		`select actor_user_id::text, target_id::text, details
		 from audit_log
		 where tenant_id = $1 and action = $2
		 order by id desc limit 1`, tenantID, action).Scan(&actor, &target, &details)
	if err != nil {
		t.Fatalf("read %s audit row: %v", action, err)
	}
	if actor == nil || *actor != adminID {
		t.Fatalf("%s actor = %v, want the real admin %s", action, actor, adminID)
	}
	if target == nil || *target != targetID {
		t.Fatalf("%s target = %v, want the impersonated user %s", action, target, targetID)
	}
	var d map[string]any
	if err := json.Unmarshal(details, &d); err != nil {
		t.Fatalf("%s details decode: %v", action, err)
	}
	if v, _ := d["impersonation"].(bool); !v {
		t.Fatalf("%s details.impersonation != true: %v", action, d)
	}
}
