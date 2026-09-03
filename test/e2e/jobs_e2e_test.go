//go:build e2e

// STORY-04.5 golden path: the REAL public HTTP router (internal/api) served over a
// real net/http listener against the REAL control-plane Postgres (up via
// `mise run up`), no mocks. It drives the jobs surface through the assembled
// tenant-scoped chain (API-key admin scope -> rate limit -> handler), proving:
//   - list returns the {items,next_cursor} envelope with the tenant's jobs, and
//     the ?status/?kind filters narrow it (FR-ADM-02),
//   - get returns one job with status/timing/stats; a bad id is 404,
//   - cancel on a QUEUED job is fully effective now: 200 + the row flips to
//     cancelled in the real jobs table (SPEC-08 §4),
//   - cancel on a TERMINAL job is 409 (not cancellable),
//   - cancel on a RUNNING job is the not_found seam (the River worker is EPIC-09;
//     ADR-0031),
//   - the tenant is derived from the API key, never a parameter (FR-ACC-03):
//     another tenant's job is neither listed nor gettable.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/jobs"
	"github.com/rag-platform/ragctl/internal/cp/ratelimit"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/obs"
)

func TestJobsGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)

	// --- Seed two tenants: t owns the jobs, other is the isolation control. ---
	var tenantID, otherID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region) values ($1, $2, 'active', 'eu-central') returning id::text`,
		"jobs-"+suffix, "Jobs Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region) values ($1, $2, 'active', 'eu-central') returning id::text`,
		"jobs-other-"+suffix, "Jobs Other "+suffix).Scan(&otherID); err != nil {
		t.Fatalf("seed other tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id in ('%s','%s')", tenantID, otherID))
	})

	// --- Seed jobs directly in the control-plane jobs table (the mirror rows the
	// EPIC-09 worker would otherwise write). ---
	seed := func(sql string, args ...any) string {
		var id string
		if err := pool.QueryRow(ctx, sql+" returning id::text", args...).Scan(&id); err != nil {
			t.Fatalf("seed job: %v", err)
		}
		return id
	}
	queuedID := seed(
		`insert into jobs (tenant_id, kind, status) values ($1, 'sync_source', 'queued')`, tenantID)
	runningID := seed(
		`insert into jobs (tenant_id, kind, status, started_at, worker_id)
		 values ($1, 'ingest_document', 'running', now() - interval '5 seconds', 'w-1')`, tenantID)
	succeededID := seed(
		`insert into jobs (tenant_id, kind, status, started_at, finished_at, stats)
		 values ($1, 'gc_tenant', 'succeeded', now() - interval '10 seconds', now(), '{"docs":3}'::jsonb)`, tenantID)
	otherJobID := seed(
		`insert into jobs (tenant_id, kind, status) values ($1, 'sync_source', 'queued')`, otherID)

	// --- An admin-scoped API key owning tenantID's jobs. ---
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
	jh := jobs.NewHandlers(jobs.NewService(jobs.FromPool(pool))) // Canceller nil (EPIC-09 seam)
	deps := api.Deps{
		Log:               obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:           obs.NewMetrics(),
		RequireScopeAdmin: verifier.RequireScope(auth.ScopeAdmin),
		RateLimit:         rl.Handler,
		JobList:           http.HandlerFunc(jh.List),
		JobGet:            http.HandlerFunc(jh.Get),
		JobCancel:         http.HandlerFunc(jh.Cancel),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()

	bearer := "Bearer " + secret
	call := func(method, path string) (int, []byte) {
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", bearer)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	// --- Unauthenticated is refused (no bearer). ---
	{
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/jobs", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("anon list: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anon list = %d, want 401", resp.StatusCode)
		}
	}

	// --- List returns the {items,next_cursor} envelope with this tenant's jobs. ---
	code, body := call(http.MethodGet, "/v1/jobs")
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body=%s", code, body)
	}
	var page jobs.Page
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	ids := map[string]jobs.Job{}
	for _, j := range page.Items {
		ids[j.ID] = j
		if j.TenantID != tenantID {
			t.Fatalf("list leaked a foreign tenant's job: %+v", j)
		}
	}
	for _, want := range []string{queuedID, runningID, succeededID} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("job %s missing from list", want)
		}
	}
	if _, leaked := ids[otherJobID]; leaked {
		t.Fatalf("other tenant's job %s appeared in list (FR-ACC-03)", otherJobID)
	}
	// The succeeded job carries computed duration + stats (FR-ADM-02).
	if sj := ids[succeededID]; sj.DurationMS == nil || *sj.DurationMS <= 0 || len(sj.Stats) == 0 {
		t.Fatalf("succeeded job missing duration/stats: %+v", sj)
	}

	// --- Filter by status narrows the list. ---
	code, body = call(http.MethodGet, "/v1/jobs?status=queued")
	if code != http.StatusOK {
		t.Fatalf("list?status=queued = %d; body=%s", code, body)
	}
	_ = json.Unmarshal(body, &page)
	for _, j := range page.Items {
		if j.Status != "queued" {
			t.Fatalf("status filter returned a %q job", j.Status)
		}
	}
	// An invalid status filter is a 400.
	if code, _ := call(http.MethodGet, "/v1/jobs?status=bogus"); code != http.StatusBadRequest {
		t.Fatalf("list?status=bogus = %d, want 400", code)
	}

	// --- Get one (200); an unknown id is 404; another tenant's job is 404. ---
	if code, _ := call(http.MethodGet, "/v1/jobs/"+queuedID); code != http.StatusOK {
		t.Fatalf("get = %d, want 200", code)
	}
	if code, _ := call(http.MethodGet, "/v1/jobs/00000000-0000-0000-0000-000000000000"); code != http.StatusNotFound {
		t.Fatalf("get missing = %d, want 404", code)
	}
	if code, _ := call(http.MethodGet, "/v1/jobs/"+otherJobID); code != http.StatusNotFound {
		t.Fatalf("get foreign-tenant job = %d, want 404 (FR-ACC-03)", code)
	}

	// --- Cancel a QUEUED job: 200 + the row flips to cancelled (SPEC-08 §4). ---
	code, body = call(http.MethodPost, "/v1/jobs/"+queuedID+"/cancel")
	if code != http.StatusOK {
		t.Fatalf("cancel queued = %d, want 200; body=%s", code, body)
	}
	var cancelled jobs.Job
	_ = json.Unmarshal(body, &cancelled)
	if cancelled.Status != "cancelled" {
		t.Fatalf("cancelled status = %q, want cancelled", cancelled.Status)
	}
	if s := psqlScalar(t, fmt.Sprintf("select status from jobs where id = '%s'", queuedID)); s != "cancelled" {
		t.Fatalf("queued job status in db = %q, want cancelled", s)
	}

	// --- Cancel a TERMINAL job: 409. ---
	if code, _ := call(http.MethodPost, "/v1/jobs/"+succeededID+"/cancel"); code != http.StatusConflict {
		t.Fatalf("cancel succeeded = %d, want 409", code)
	}

	// --- Cancel a RUNNING job: not_found seam (River worker is EPIC-09). ---
	code, body = call(http.MethodPost, "/v1/jobs/"+runningID+"/cancel")
	if code != http.StatusNotFound {
		t.Fatalf("cancel running = %d, want 404 (seam); body=%s", code, body)
	}
	// The running row is untouched (the API never flips running->cancelled itself).
	if s := psqlScalar(t, fmt.Sprintf("select status from jobs where id = '%s'", runningID)); s != "running" {
		t.Fatalf("running job status in db = %q, want running (unchanged)", s)
	}
}
