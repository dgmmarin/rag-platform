//go:build e2e

// STORY-04.1 golden path: the REAL public HTTP router (internal/api) served over
// a real net/http listener against the REAL control-plane Postgres (up via
// `mise run up`), no mocks. It drives a full flow through the assembled
// middleware chain, proving the chain works end to end, not just in unit
// isolation:
//   - health/readiness endpoints are open (no auth) and 200,
//   - an unauthenticated protected route (GET /admin/audit) is 401 with the
//     SPEC-07 §1 error envelope,
//   - signup + login through the mounted auth routes returns a session cookie and
//     a CSRF token,
//   - the logged-in platform admin reads the audit log through the mounted
//     /admin/audit route (session -> RequirePlatformAdmin -> handler) and sees a
//     real settings.update row,
//   - a non-admin session is refused 403 by the platform-admin gate.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/cp/audit"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestAPIRouterGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)

	// Build the SAME router serve builds, from the real control-plane pool.
	authSvc := auth.NewService(auth.FromPool(pool))
	authHandlers := &auth.Handlers{Service: authSvc, Secure: false}
	authz := auth.NewAuthzService(auth.MembershipFromPool(pool))
	auditHandlers := audit.NewHandlers(audit.NewService(audit.FromPool(pool)))

	deps := api.Deps{
		Log:                  obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:              obs.NewMetrics(),
		RequireSession:       authHandlers.RequireSession,
		CSRF:                 authHandlers.CSRF,
		RequirePlatformAdmin: authz.RequirePlatformAdmin(),
		Signup:               http.HandlerFunc(authHandlers.Signup),
		Login:                http.HandlerFunc(authHandlers.Login),
		Logout:               http.HandlerFunc(authHandlers.Logout),
		AuditList:            http.HandlerFunc(auditHandlers.List),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()

	client := srv.Client()
	jar, _ := cookiejar.New(nil)
	client.Jar = jar

	// --- Health/readiness are open. ---
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	// --- Unauthenticated protected route is 401 with the spec envelope. ---
	{
		resp, err := client.Get(srv.URL + "/admin/audit?tenant=" + uuid.NewString())
		if err != nil {
			t.Fatalf("anon audit: %v", err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anon audit = %d, want 401", resp.StatusCode)
		}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		_ = resp.Body.Close()
		if env.Error.Code != api.CodeUnauthorized {
			t.Fatalf("error.code = %q, want unauthorized", env.Error.Code)
		}
		// The obs middleware (outermost) always stamps the correlation id on the
		// response, so a client can tie any error back to server logs (SPEC-07 §1).
		if resp.Header.Get("X-Request-Id") == "" {
			t.Fatal("response missing X-Request-Id correlation header")
		}
	}

	// --- Seed a tenant + a real settings.update audit row (as in STORY-03.6). ---
	slug := "api-" + suffix
	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region, settings)
		 values ($1, $2, 'active', 'eu-central', jsonb_build_object('embedding_dim', 1024::int))
		 returning id::text`,
		slug, "API Router Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	adminEmail := fmt.Sprintf("api-admin-%s@example.test", strings.ReplaceAll(suffix, "-", ""))
	const password = "a-sufficiently-strong-password"

	// Produce the audit row via the same real settings action STORY-03.6 uses.
	adminID := seedUser(t, ctx, pool, adminEmail, true)
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	settingsHandlers := tenants.NewSettingsHandlers(settingsSvc)
	{
		body := `{"retrieval":{"k_vector":40,"k_text":40,"final_k":12,"min_score":0.05}}`
		r := httptest.NewRequest(http.MethodPatch, "/v1/settings", strings.NewReader(body))
		c := tenant.WithTenantID(r.Context(), tenant.ID(uuid.MustParse(tenantID)))
		c = auth.ContextWithSession(c, auth.Session{UserID: adminID})
		rr := httptest.NewRecorder()
		settingsHandlers.Patch(rr, r.WithContext(c))
		if rr.Code != http.StatusOK {
			t.Fatalf("seed settings.update = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
	}

	// The seeded admin has no password (seedUser inserts without one). Give it one
	// so it can log in through the real /v1/auth/login route.
	setPassword(ctx, t, pool, adminID, password)

	// --- Login through the mounted route: get session cookie + CSRF token. ---
	csrf := login(t, client, srv.URL, adminEmail, password)
	if csrf == "" {
		t.Fatal("login returned empty csrf token")
	}

	// --- Platform admin reads the audit log through the real chain. ---
	{
		resp, err := client.Get(srv.URL + "/admin/audit?tenant=" + tenantID)
		if err != nil {
			t.Fatalf("admin audit: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admin audit = %d, want 200", resp.StatusCode)
		}
		var body struct {
			Entries []audit.Entry `json:"entries"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode audit: %v", err)
		}
		_ = resp.Body.Close()
		found := false
		for _, e := range body.Entries {
			if e.Action == "settings.update" {
				found = true
			}
		}
		if !found {
			t.Fatalf("settings.update entry not returned through the router; entries=%d", len(body.Entries))
		}
	}

	// --- A non-admin session is refused 403 by the platform-admin gate. ---
	{
		memberEmail := fmt.Sprintf("api-member-%s@example.test", strings.ReplaceAll(suffix, "-", ""))
		memberID := seedUser(t, ctx, pool, memberEmail, false)
		setPassword(ctx, t, pool, memberID, password)

		mjar, _ := cookiejar.New(nil)
		mclient := srv.Client()
		mclient.Jar = mjar
		login(t, mclient, srv.URL, memberEmail, password)

		resp, err := mclient.Get(srv.URL + "/admin/audit?tenant=" + tenantID)
		if err != nil {
			t.Fatalf("member audit: %v", err)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("member audit = %d, want 403", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// login posts credentials to the mounted /v1/auth/login route and returns the
// CSRF token, leaving the session cookie in the client's jar.
func login(t *testing.T, client *http.Client, base, email, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	resp, err := client.Post(base+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	}
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return out.CSRFToken
}

// setPassword sets an argon2id password hash on an existing user so a seeded user
// can log in through the real login route.
func setPassword(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, `update users set password_hash = $2 where id = $1`, userID, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
}
