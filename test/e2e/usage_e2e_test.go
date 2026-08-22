//go:build e2e

// STORY-03.7 golden path: usage accounting against the REAL control-plane Postgres
// (up via `mise run up`), no mocks. It proves:
//   - the sanctioned usage.Counter buffers increments and flushes them into
//     usage_daily with an accumulating upsert (SPEC-10 §6): two flushes for the
//     same tenant/day sum rather than overwrite,
//   - GET /v1/usage returns the resolved tenant's daily rows (FR-ADM-06, SPEC-07),
//     scoped to the tenant from context (FR-ACC-03),
//   - the read is tenant-scoped: a request with no resolved tenant is refused 401.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/cp/usage"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestUsageCountersGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "usg-" + suffix

	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region, settings)
		 values ($1, $2, 'active', 'eu-central', jsonb_build_object('embedding_dim', 1024::int))
		 returning id::text`,
		slug, "Usage Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	day := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	wantDay := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

	// --- Two flush cycles must accumulate on the same (tenant, day) row. ---
	counter := usage.NewCounter(usage.FromPool(pool))

	counter.AddAt(tenantID, usage.Delta{Queries: 3, EmbedTokens: 1000, ChunksEmbedded: 12}, day)
	counter.AddAt(tenantID, usage.Delta{Queries: 2, DocsIngested: 4}, day.Add(time.Hour))
	if err := counter.Flush(ctx); err != nil {
		t.Fatalf("first flush: %v", err)
	}

	counter.AddAt(tenantID, usage.Delta{Queries: 5, LLMInTokens: 800, LLMOutTokens: 600}, day.Add(2*time.Hour))
	if err := counter.Flush(ctx); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	// Verify the raw row accumulated across both flushes (not overwritten).
	var q, docs, chunks, embed, llmIn, llmOut int64
	if err := pool.QueryRow(ctx,
		`select queries, docs_ingested, chunks_embedded, embed_tokens, llm_in_tokens, llm_out_tokens
		 from usage_daily where tenant_id = $1 and day = $2`,
		tenantID, wantDay).Scan(&q, &docs, &chunks, &embed, &llmIn, &llmOut); err != nil {
		t.Fatalf("read usage_daily: %v", err)
	}
	if q != 10 || docs != 4 || chunks != 12 || embed != 1000 || llmIn != 800 || llmOut != 600 {
		t.Fatalf("counts did not accumulate: queries=%d docs=%d chunks=%d embed=%d llmIn=%d llmOut=%d",
			q, docs, chunks, embed, llmIn, llmOut)
	}

	// --- GET /v1/usage returns the resolved tenant's rows. ---
	handlers := usage.NewHandlers(usage.NewService(usage.FromPool(pool)))
	tid := tenant.ID(uuid.MustParse(tenantID))

	call := func(withTenant bool, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/usage"+query, nil)
		if withTenant {
			r = r.WithContext(tenant.WithTenantID(r.Context(), tid))
		}
		rr := httptest.NewRecorder()
		handlers.List(rr, r)
		return rr
	}

	{
		rr := call(true, "?from=2026-08-01&to=2026-08-31")
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /v1/usage = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		var body struct {
			Usage []usage.Row `json:"usage"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var got *usage.Row
		for i := range body.Usage {
			if body.Usage[i].Day.Equal(wantDay) {
				got = &body.Usage[i]
			}
		}
		if got == nil {
			t.Fatalf("day row not returned: %s", rr.Body.String())
		}
		if got.TenantID != tenantID {
			t.Fatalf("row tenant = %s, want %s", got.TenantID, tenantID)
		}
		if got.Queries != 10 || got.EmbedTokens != 1000 || got.LLMOutTokens != 600 {
			t.Fatalf("row counts wrong: %#v", *got)
		}
	}

	// --- No resolved tenant is refused (fail closed, FR-ACC-03). ---
	{
		rr := call(false, "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("no-tenant GET = %d, want 401; body=%s", rr.Code, rr.Body.String())
		}
	}
}
