//go:build e2e

// STORY-04.6 golden path: the REAL public HTTP router (internal/api) served over a
// real net/http listener against the REAL control-plane Postgres (up via
// `mise run up`), no mocks. It drives the platform-admin tenant surface through
// the assembled chain (session -> RequirePlatformAdmin -> CSRF -> handler) and the
// REAL provisioner/lifecycle (STORY-02.3/02.4/02.5), proving (FR-TEN-01/05/07,
// SPEC-07 §2):
//   - POST /admin/tenants provisions a real tenant database + role and returns the
//     tenant + a provision_tenant job id (201); the tenant is active,
//   - a CSRF-less mutation is refused (403) — the mutations are CSRF-guarded,
//   - GET /admin/tenants lists it ({items,next_cursor}),
//   - PATCH /admin/tenants/{id} suspends (status), updates settings, then resumes,
//     each routed to the existing lifecycle/settings service (DB reflects it),
//   - DELETE /admin/tenants/{id}?grace schedules deletion (202) — status becomes
//     deleting with delete_after set,
//   - a non-platform-admin session is refused (403) by the platform gate.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/provision"
)

func TestAdminTenantsGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "adm" + suffix

	// Teardown: drop the provisioned DB + role and the control-plane rows, wherever
	// the test fails.
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

	// --- Build the SAME router serve builds, from the real control-plane pool and
	// the real provisioner/lifecycle (privileged connection = control-plane URL,
	// superuser locally). The cipher shares the migrate DEK so recorded passwords
	// round-trip. ---
	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	port, err := strconv.Atoi(hostPort("POSTGRES_PORT", "5432"))
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	authSvc := auth.NewService(auth.FromPool(pool))
	authHandlers := &auth.Handlers{Service: authSvc, Secure: false}
	authz := auth.NewAuthzService(auth.MembershipFromPool(pool))
	adminSvc := &tenants.AdminService{
		Store:    tenants.AdminStoreFromPool(pool),
		Prov:     &provision.Provisioner{PrivilegedURL: controlPlaneURL(), Encrypter: cipher, Decrypter: cipher, TenantHost: "localhost", TenantPort: port},
		Life:     &provision.Lifecycle{PrivilegedURL: controlPlaneURL(), Encrypter: cipher},
		Settings: tenants.NewSettingsService(tenants.SettingsFromPool(pool)),
		SSLMode:  "disable",
	}
	th := tenants.NewAdminHandlers(adminSvc)
	deps := api.Deps{
		Log:                  obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:              obs.NewMetrics(),
		RequireSession:       authHandlers.RequireSession,
		CSRF:                 authHandlers.CSRF,
		RequirePlatformAdmin: authz.RequirePlatformAdmin(),
		Signup:               http.HandlerFunc(authHandlers.Signup),
		Login:                http.HandlerFunc(authHandlers.Login),
		Logout:               http.HandlerFunc(authHandlers.Logout),
		TenantCreate:         http.HandlerFunc(th.Create),
		TenantList:           http.HandlerFunc(th.List),
		TenantUpdate:         http.HandlerFunc(th.Update),
		TenantDelete:         http.HandlerFunc(th.Delete),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()

	client := srv.Client()
	jar, _ := cookiejar.New(nil)
	client.Jar = jar

	// --- A platform admin with a password, logged in through the real route. ---
	adminEmail := fmt.Sprintf("adm-admin-%s@example.test", suffix)
	const password = "a-sufficiently-strong-password"
	adminID := seedUser(t, ctx, pool, adminEmail, true)
	setPassword(ctx, t, pool, adminID, password)
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM users WHERE id = '%s'", adminID))
	})
	csrf := login(t, client, srv.URL, adminEmail, password)
	if csrf == "" {
		t.Fatal("login returned empty csrf token")
	}

	call := func(method, path, body, csrfTok string) (int, []byte) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, rdr)
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if csrfTok != "" {
			req.Header.Set(auth.CSRFHeaderName, csrfTok)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	// --- A CSRF-less mutation is refused (proves the mutations are CSRF-guarded). ---
	if code, _ := call(http.MethodPost, "/admin/tenants",
		`{"slug":"`+slug+`","name":"No CSRF"}`, ""); code != http.StatusForbidden {
		t.Fatalf("create without CSRF = %d, want 403", code)
	}

	// --- POST /admin/tenants provisions a real tenant + returns tenant + job id. ---
	code, body := call(http.MethodPost, "/admin/tenants",
		`{"slug":"`+slug+`","name":"Admin Tenants Co","region":"eu-central","embedding_dim":768}`, csrf)
	if code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", code, body)
	}
	var created struct {
		Tenant tenants.Tenant `json:"tenant"`
		JobID  string         `json:"job_id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v; body=%s", err, body)
	}
	if created.Tenant.ID == "" || created.Tenant.Status != "active" {
		t.Fatalf("created tenant = %+v, want active with id", created.Tenant)
	}
	if created.JobID == "" {
		t.Fatal("create returned empty job_id")
	}
	tenantID := created.Tenant.ID

	// The tenant database + role were really provisioned, and a provision_tenant
	// mirror row exists (ADR-0005 history view).
	if db := tenantScalar(t, slug, "d.database_name"); db == "" {
		t.Fatal("tenant_databases row missing after create")
	}
	if k := psqlScalar(t, fmt.Sprintf("select kind from jobs where id = '%s'", created.JobID)); k != "provision_tenant" {
		t.Fatalf("provision job kind = %q, want provision_tenant", k)
	}

	// --- GET /admin/tenants lists it. ---
	code, body = call(http.MethodGet, "/admin/tenants?limit=200", "", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body=%s", code, body)
	}
	var page tenants.TenantPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, tn := range page.Items {
		if tn.ID == tenantID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created tenant %s not in list", tenantID)
	}

	// --- PATCH suspend + settings update, then verify the DB reflects both. ---
	code, body = call(http.MethodPatch, "/admin/tenants/"+tenantID,
		`{"status":"suspended","settings":{"retrieval":{"final_k":15}}}`, csrf)
	if code != http.StatusOK {
		t.Fatalf("patch suspend = %d, want 200; body=%s", code, body)
	}
	var patched tenants.Tenant
	_ = json.Unmarshal(body, &patched)
	if patched.Status != "suspended" {
		t.Fatalf("patched status = %q, want suspended", patched.Status)
	}
	if s := psqlScalar(t, fmt.Sprintf("select status from tenants where id = '%s'", tenantID)); s != "suspended" {
		t.Fatalf("db status = %q, want suspended", s)
	}
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from audit_log where tenant_id = '%s' and action = 'tenant.suspend'", tenantID)); n == "0" {
		t.Fatal("no tenant.suspend audit row written")
	}
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from audit_log where tenant_id = '%s' and action = 'settings.update'", tenantID)); n == "0" {
		t.Fatal("no settings.update audit row written")
	}

	// --- PATCH resume. ---
	code, body = call(http.MethodPatch, "/admin/tenants/"+tenantID, `{"status":"active"}`, csrf)
	if code != http.StatusOK {
		t.Fatalf("patch resume = %d, want 200; body=%s", code, body)
	}

	// --- Unknown tenant PATCH is 404. ---
	if code, _ := call(http.MethodPatch, "/admin/tenants/00000000-0000-0000-0000-000000000000",
		`{"status":"active"}`, csrf); code != http.StatusNotFound {
		t.Fatalf("patch unknown = %d, want 404", code)
	}

	// --- DELETE schedules deletion with grace (202); status -> deleting. ---
	code, body = call(http.MethodDelete, "/admin/tenants/"+tenantID+"?grace=168h", "", csrf)
	if code != http.StatusAccepted {
		t.Fatalf("delete = %d, want 202; body=%s", code, body)
	}
	var deleted tenants.Tenant
	_ = json.Unmarshal(body, &deleted)
	if deleted.Status != "deleting" {
		t.Fatalf("deleted status = %q, want deleting", deleted.Status)
	}
	if deleted.DeleteAfter == nil {
		t.Fatal("delete response missing delete_after")
	}
	if s := psqlScalar(t, fmt.Sprintf("select status from tenants where id = '%s'", tenantID)); s != "deleting" {
		t.Fatalf("db status after delete = %q, want deleting", s)
	}

	// --- A non-platform-admin session is refused by the platform gate. ---
	memberEmail := fmt.Sprintf("adm-member-%s@example.test", suffix)
	memberID := seedUser(t, ctx, pool, memberEmail, false)
	setPassword(ctx, t, pool, memberID, password)
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM users WHERE id = '%s'", memberID))
	})
	mjar, _ := cookiejar.New(nil)
	mclient := &http.Client{Jar: mjar}
	_ = login(t, mclient, srv.URL, memberEmail, password)
	{
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/admin/tenants", nil)
		resp, err := mclient.Do(req)
		if err != nil {
			t.Fatalf("member list: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("member list = %d, want 403", resp.StatusCode)
		}
	}
}
