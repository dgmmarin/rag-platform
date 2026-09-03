//go:build e2e

// STORY-04.3 golden path: the REAL public HTTP router (internal/api) served over a
// real net/http listener against the REAL control-plane Postgres (up via
// `mise run up`), no mocks. It drives the full sources lifecycle through the
// assembled tenant-scoped chain (API-key admin scope -> rate limit -> handler),
// proving:
//   - create returns 201 and never echoes credentials (FR-SRC-10),
//   - list returns the {items,next_cursor} envelope containing the new source,
//   - get returns it; a bad id is 404,
//   - patch pauses the source (pause/resume, FR-SRC-01),
//   - a manual sync enqueues a real sync_source job (202); the Idempotency-Key
//     replays the same job; a second distinct sync is 409 (the partial unique
//     index jobs_one_active_sync_per_source, SPEC-07 §2),
//   - delete marks the source deleting and enqueues delete_source (202),
//   - test connection returns the not_found seam (connector framework is EPIC-06),
//   - the tenant is derived from the API key, never a parameter (FR-ACC-03).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/ratelimit"
	"github.com/rag-platform/ragctl/internal/cp/sources"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/obs"
)

func TestSourcesGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "sources-" + suffix

	// --- Seed a tenant and an admin-scoped API key that owns the sources. ---
	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region) values ($1, $2, 'active', 'eu-central') returning id::text`,
		slug, "Sources Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	keySvc := auth.NewAPIKeyService(auth.MembershipFromPool(pool))
	_, secret, err := keySvc.Create(ctx, auth.CreateKeyParams{TenantID: tenantID, Name: "admin-token", Scopes: []string{"admin"}})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// --- Build the SAME router serve builds, from the real control-plane pool. ---
	verifier := auth.NewAPIKeyVerifier(auth.FromPool(pool))
	limiter := ratelimit.New(nil)
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	rl := &ratelimit.Middleware{
		Limiter:     limiter,
		Limit:       ratelimit.LimitFromSettings(settingsSvc, 1000),
		Burst:       1000,
		TenantBurst: 1000,
	}
	sh := sources.NewHandlers(sources.NewService(sources.FromPool(pool)))
	deps := api.Deps{
		Log:               obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:           obs.NewMetrics(),
		RequireScopeAdmin: verifier.RequireScope(auth.ScopeAdmin),
		RateLimit:         rl.Handler,
		SourceList:        http.HandlerFunc(sh.List),
		SourceCreate:      http.HandlerFunc(sh.Create),
		SourceGet:         http.HandlerFunc(sh.Get),
		SourceUpdate:      http.HandlerFunc(sh.Update),
		SourceDelete:      http.HandlerFunc(sh.Delete),
		SourceSync:        http.HandlerFunc(sh.Sync),
		SourceTest:        http.HandlerFunc(sh.Test),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()

	bearer := "Bearer " + secret
	call := func(method, path, body string, headers map[string]string) (int, []byte) {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, r)
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", bearer)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	// --- Unauthenticated is refused (no bearer) with the spec envelope. ---
	{
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/sources", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("anon list: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anon list = %d, want 401", resp.StatusCode)
		}
	}

	// --- Create (201); credentials must never be echoed. ---
	createBody := `{"kind":"web_crawl","name":"docs-` + suffix + `","config":{"start_urls":["https://docs.example/"]}}`
	code, body := call(http.MethodPost, "/v1/sources", createBody, nil)
	if code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", code, body)
	}
	var created sources.Source
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.ID == "" || created.TenantID != tenantID || created.Status != "active" {
		t.Fatalf("unexpected created source: %+v", created)
	}
	if strings.Contains(string(body), "credential") {
		t.Fatalf("create response leaks credentials: %s", body)
	}

	// --- Duplicate name -> 409 conflict. ---
	if code, _ := call(http.MethodPost, "/v1/sources", createBody, nil); code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", code)
	}

	// --- Credentials in the body are rejected (STORY-06.2 defer) -> 400. ---
	if code, _ := call(http.MethodPost, "/v1/sources",
		`{"kind":"api","name":"x-`+suffix+`","credentials":{"token":"s"}}`, nil); code != http.StatusBadRequest {
		t.Fatalf("create with credentials = %d, want 400", code)
	}

	// --- List returns the {items,next_cursor} envelope containing the source. ---
	code, body = call(http.MethodGet, "/v1/sources", "", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body=%s", code, body)
	}
	var page sources.Page
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, s := range page.Items {
		if s.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created source not in list; items=%d", len(page.Items))
	}

	// --- Get one (200); an unknown id is 404. ---
	if code, _ := call(http.MethodGet, "/v1/sources/"+created.ID, "", nil); code != http.StatusOK {
		t.Fatalf("get = %d, want 200", code)
	}
	if code, _ := call(http.MethodGet, "/v1/sources/00000000-0000-0000-0000-000000000000", "", nil); code != http.StatusNotFound {
		t.Fatalf("get missing = %d, want 404", code)
	}

	// --- Patch: pause the source (FR-SRC-01). ---
	code, body = call(http.MethodPatch, "/v1/sources/"+created.ID, `{"status":"paused"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("patch = %d, want 200; body=%s", code, body)
	}
	var patched sources.Source
	_ = json.Unmarshal(body, &patched)
	if patched.Status != "paused" {
		t.Fatalf("patch status = %q, want paused", patched.Status)
	}

	// --- Sync with an Idempotency-Key enqueues a real sync_source job (202). ---
	idem := map[string]string{"Idempotency-Key": "k-" + suffix}
	code, body = call(http.MethodPost, "/v1/sources/"+created.ID+"/sync", `{"full":true}`, idem)
	if code != http.StatusAccepted {
		t.Fatalf("sync = %d, want 202; body=%s", code, body)
	}
	var job1 sources.Job
	if err := json.Unmarshal(body, &job1); err != nil {
		t.Fatalf("decode sync: %v", err)
	}
	if job1.Kind != "sync_source" || job1.Status != "queued" {
		t.Fatalf("unexpected sync job: %+v", job1)
	}
	// The job is really in the control-plane jobs table.
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from jobs where id = '%s' and kind = 'sync_source' and status = 'queued'", job1.ID)); n != "1" {
		t.Fatalf("sync job not persisted in jobs table (count=%s)", n)
	}

	// --- Same Idempotency-Key replays the same job (not a 409). ---
	code, body = call(http.MethodPost, "/v1/sources/"+created.ID+"/sync", `{"full":true}`, idem)
	if code != http.StatusAccepted {
		t.Fatalf("idempotent sync = %d, want 202", code)
	}
	var job2 sources.Job
	_ = json.Unmarshal(body, &job2)
	if job2.ID != job1.ID {
		t.Fatalf("idempotency-key did not replay: %s vs %s", job2.ID, job1.ID)
	}

	// --- A distinct sync while one is active -> 409 (SPEC-07 §2). ---
	if code, _ := call(http.MethodPost, "/v1/sources/"+created.ID+"/sync", `{}`, nil); code != http.StatusConflict {
		t.Fatalf("concurrent sync = %d, want 409", code)
	}

	// --- Test connection: connector framework is EPIC-06 -> not_found seam. ---
	if code, _ := call(http.MethodPost, "/v1/sources/"+created.ID+"/test", "", nil); code != http.StatusNotFound {
		t.Fatalf("test = %d, want 404 (seam)", code)
	}

	// --- Delete: marks deleting + enqueues delete_source (202). ---
	code, body = call(http.MethodDelete, "/v1/sources/"+created.ID, "", nil)
	if code != http.StatusAccepted {
		t.Fatalf("delete = %d, want 202; body=%s", code, body)
	}
	if s := psqlScalar(t, fmt.Sprintf("select status from sources where id = '%s'", created.ID)); s != "deleting" {
		t.Fatalf("source status after delete = %q, want deleting", s)
	}
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from jobs where source_id = '%s' and kind = 'delete_source'", created.ID)); n != "1" {
		t.Fatalf("delete_source job not enqueued (count=%s)", n)
	}
}
