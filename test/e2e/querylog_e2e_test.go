//go:build e2e

// STORY-08.8 golden path: async query logging + feedback + admin visibility
// (FR-RET-09/10, SPEC-06 §6, SPEC-07 §2g) driven over the REAL public router
// (api.New) against a REAL enrolled tenant database (up via `mise run up`), reached
// ONLY through a resolver + *tenant.DB (ADR-0003, C-1, C-3). query_log and
// query_feedback are tenant content: no control-plane pool ever touches them.
//
// The two external providers are stubbed (embedding + LLM); everything else is
// real. It proves:
//   - POST /v1/query persists a query_log row ASYNCHRONOUSLY (awaited with a
//     bounded poll of the tenant DB) with the question, grounded flag, retrieved
//     chunk ids + scores and the model,
//   - a refusal query (no-match filter) is also logged with grounded=false,
//   - POST /v1/feedback writes a query_feedback row keyed by the returned query id,
//   - GET /v1/queries (admin scope) lists BOTH queries with the feedback joined.
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

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/cp/usage"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/query"
	"github.com/rag-platform/ragctl/internal/querylog"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestQueryLogAndFeedbackGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const dim = 8
	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "querylog-" + suffix
	tenantDisplay := "Query Log " + suffix
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		var dbName, role string
		_ = pool.QueryRow(cctx,
			`select d.database_name, d.username from tenant_databases d
			 join tenants t on t.id = d.tenant_id where t.slug = $1`, slug).Scan(&dbName, &role)
		if dbName != "" {
			_, _ = pool.Exec(cctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		}
		if role != "" {
			_, _ = pool.Exec(cctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
		}
		_, _ = pool.Exec(cctx, `DELETE FROM tenants WHERE slug = $1`, slug)
	})
	if out, exit := runEnroll(t, ageKey, blob, slug, tenantDisplay, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `select t.id::text from tenants t where t.slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}

	sourceDocs := seedSource(ctx, t, pool, tenantID, "docs")

	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher, CacheTTL: 50 * time.Millisecond})
	db, err := resolver.Open(ctx, tenant.ID(uuid.MustParse(tenantID)))
	if err != nil {
		t.Fatalf("resolver.Open: %v", err)
	}
	store := documents.NewTenantStore()

	vec := func(v ...float32) []float32 {
		out := make([]float32, dim)
		copy(out, v)
		return out
	}
	const model = "text-embedding-3-small"
	putDoc(ctx, t, store, db, putSpec{
		src: sourceDocs, ext: "a", title: "X200 Reset Guide",
		uri: "https://docs.acme.com/x200", meta: `{"team":"support"}`,
		content: "How to reset the X200 device", emb: vec(1, 0, 0, 0, 0, 0, 0, 0), model: model,
	})

	// --- Real router with the REAL query + querylog wiring; only embedding + LLM
	// are stubbed. The Logger fills the answer stage's QueryLogger seam. ---
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	retrieveSvc := retrieve.NewService(resolver, settingsSvc, retrieveStubFactory{vec: vec(1, 0, 0, 0, 0, 0, 0, 0)})
	counter := usage.NewCounter(nil)
	qlStore := querylog.NewTenantStore()
	logger := &querylog.Logger{Resolver: resolver, Store: qlStore, Slog: obs.Logger("e2e", 0, io.Discard)}
	qlSvc := &querylog.Service{Resolver: resolver, Store: qlStore}
	qlHandlers := querylog.NewHandlers(qlSvc)

	querySvc := &query.Service{
		Retrieve: retrieveSvc,
		Answer:   &answer.Service{Providers: queryStubLLMFactory{}, Usage: counter, Logger: logger},
		Settings: settingsSvc,
		Names:    tenants.NewNameService(tenants.SettingsFromPool(pool)),
		Usage:    counter,
	}
	qh := query.NewHandlers(querySvc)

	deps := api.Deps{
		Log:               obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:           obs.NewMetrics(),
		RequireScopeQuery: injectTenantMW(tenantID),
		RequireScopeAdmin: injectTenantMW(tenantID),
		Query:             http.HandlerFunc(qh.Query),
		Feedback:          http.HandlerFunc(qlHandlers.Feedback),
		QueryList:         http.HandlerFunc(qlHandlers.List),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()
	client := srv.Client()

	// --- 1. A grounded query is logged asynchronously (FR-RET-09). ---
	grounded := postQueryJSON(t, client, srv.URL, `{"question":"reset the X200 device","stream":false}`)
	if !grounded.Grounded || grounded.ID == "" {
		t.Fatalf("expected a grounded answer with an id, got %+v", grounded)
	}
	waitForQueryLogCount(ctx, t, db, 1)

	// The stored row carries the question, grounded flag, retrieved chunk(s) with
	// scores, and the model. It keys on the response id with the "q_" prefix stripped.
	var (
		gotQuestion string
		gotGrounded bool
		gotModel    string
		retrieved   []byte
	)
	storedID := strings.TrimPrefix(grounded.ID, "q_")
	if err := db.QueryRow(ctx,
		`select question, grounded, coalesce(llm_model,''), retrieved from query_log where id = $1`, storedID).
		Scan(&gotQuestion, &gotGrounded, &gotModel, &retrieved); err != nil {
		t.Fatalf("read query_log row: %v", err)
	}
	if gotQuestion != "reset the X200 device" || !gotGrounded || gotModel != "claude-sonnet-5" {
		t.Fatalf("query_log row = {q:%q grounded:%v model:%q}", gotQuestion, gotGrounded, gotModel)
	}
	var rc []struct {
		ChunkID string  `json:"chunk_id"`
		Score   float64 `json:"score"`
		Rank    int     `json:"rank"`
	}
	if err := json.Unmarshal(retrieved, &rc); err != nil {
		t.Fatalf("decode retrieved jsonb: %v", err)
	}
	if len(rc) == 0 || rc[0].Rank != 1 || rc[0].Score <= 0 || rc[0].ChunkID == "" {
		t.Fatalf("retrieved = %+v, want at least a rank-1 chunk with a positive score", rc)
	}

	// --- 2. A refusal query (no-match filter) is also logged, grounded=false. ---
	noSource := uuid.NewString()
	refusal := postQueryJSON(t, client, srv.URL,
		fmt.Sprintf(`{"question":"reset the X200 device","filters":{"source_ids":[%q]}}`, noSource))
	if refusal.Grounded {
		t.Fatalf("expected a refusal, got %+v", refusal)
	}
	waitForQueryLogCount(ctx, t, db, 2)

	// --- 3. Feedback on the grounded query (FR-RET-10). ---
	fbBody := fmt.Sprintf(`{"query_id":%q,"rating":1,"comment":"helpful"}`, grounded.ID)
	postFeedback(t, client, srv.URL, fbBody, http.StatusOK)

	// Idempotent: a second feedback for the same query replaces it (last write wins).
	postFeedback(t, client, srv.URL, fmt.Sprintf(`{"query_id":%q,"rating":-1}`, grounded.ID), http.StatusOK)

	// Feedback for an unknown query id is a 404 (ownership is structural — the write
	// targets this tenant's own database).
	postFeedback(t, client, srv.URL, fmt.Sprintf(`{"query_id":"q_%s","rating":1}`, uuid.NewString()), http.StatusNotFound)

	// --- 4. Admin visibility: GET /v1/queries lists BOTH queries with feedback. ---
	page := getQueryList(t, client, srv.URL)
	if len(page.Items) != 2 {
		t.Fatalf("admin list returned %d entries, want 2: %+v", len(page.Items), page.Items)
	}
	var groundedEntry, refusalEntry *querylog.Entry
	for i := range page.Items {
		e := &page.Items[i]
		switch e.ID {
		case grounded.ID:
			groundedEntry = e
		case refusal.ID:
			refusalEntry = e
		}
	}
	if groundedEntry == nil || refusalEntry == nil {
		t.Fatalf("admin list missing one of the queries: grounded=%v refusal=%v", groundedEntry, refusalEntry)
	}
	if !groundedEntry.Grounded || len(groundedEntry.Retrieved) == 0 {
		t.Fatalf("grounded entry = %+v, want grounded with retrieved chunks", groundedEntry)
	}
	if groundedEntry.Feedback == nil || groundedEntry.Feedback.Rating != -1 {
		t.Fatalf("grounded entry feedback = %+v, want the last-written rating -1", groundedEntry.Feedback)
	}
	if refusalEntry.Grounded {
		t.Fatalf("refusal entry should have grounded=false, got %+v", refusalEntry)
	}
	if refusalEntry.Feedback != nil {
		t.Fatalf("refusal entry should have no feedback, got %+v", refusalEntry.Feedback)
	}
	// Admin list must never leak the answer text or another tenant's data; the id
	// round-trips the q_ prefix so a client can feed it back to /v1/feedback.
	if !strings.HasPrefix(groundedEntry.ID, "q_") {
		t.Fatalf("entry id %q should carry the q_ prefix", groundedEntry.ID)
	}
}

// waitForQueryLogCount polls the tenant's query_log until it holds want rows or a
// bounded deadline elapses — proving the async write lands without coupling the
// test to the logger's internals.
func waitForQueryLogCount(ctx context.Context, t *testing.T, db *tenant.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := db.QueryRow(ctx, `select count(*) from query_log`).Scan(&n); err != nil {
			t.Fatalf("count query_log: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("query_log did not reach %d rows in time (have %d)", want, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func postFeedback(t *testing.T, client *http.Client, base, body string, wantStatus int) {
	t.Helper()
	resp, err := client.Post(base+"/v1/feedback", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/feedback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantStatus {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/feedback = %d, want %d; body=%s", resp.StatusCode, wantStatus, b)
	}
}

func getQueryList(t *testing.T, client *http.Client, base string) querylog.Page {
	t.Helper()
	resp, err := client.Get(base + "/v1/queries?limit=50")
	if err != nil {
		t.Fatalf("GET /v1/queries: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/queries = %d, want 200; body=%s", resp.StatusCode, b)
	}
	var page querylog.Page
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode /v1/queries: %v", err)
	}
	return page
}
