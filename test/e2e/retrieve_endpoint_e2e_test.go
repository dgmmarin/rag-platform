//go:build e2e

// STORY-08.2 golden path: the /v1/retrieve endpoint (FR-RET-08, SPEC-06 §2,
// SPEC-07 §2) driven over the REAL public router (api.New) against a REAL enrolled
// tenant database (up via `mise run up`), reached ONLY through a resolver +
// *tenant.DB (ADR-0003, C-1, C-3). Content is seeded through the real write store
// exactly as ingestion would; the embedding PROVIDER is external, so it is stubbed
// with a deterministic Embedder (as the ingest e2e stubs it) — everything else is
// real: the router, the query-scope chain that resolves the tenant (FR-ACC-03),
// the settings load, and the hybrid SQL. It proves the endpoint:
//   - embeds the query and returns ranked chunks with score + citation metadata,
//   - respects top_k, and
//   - respects the source filter (FR-RET-02).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// retrieveStubEmbedder returns a fixed dim-8 vector for every text, so the query
// embeds to the same space as the seeded corpus (the provider is external).
type retrieveStubEmbedder struct{ vec []float32 }

func (e retrieveStubEmbedder) Embed(_ context.Context, texts []string) (embed.Result, error) {
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		vecs[i] = e.vec
	}
	return embed.Result{Vectors: vecs, Tokens: len(texts)}, nil
}

// retrieveStubFactory builds the stub embedder regardless of settings (matching
// the ingest e2e's stub-factory approach).
type retrieveStubFactory struct{ vec []float32 }

func (f retrieveStubFactory) Embedder(context.Context, retrieve.Settings) (embed.Embedder, error) {
	return retrieveStubEmbedder{vec: f.vec}, nil
}

func TestRetrieveEndpointGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const dim = 8
	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "retrieveapi-" + suffix
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
	if out, exit := runEnroll(t, ageKey, blob, slug, "Retrieve API "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `select t.id::text from tenants t where t.slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}

	sourceDocs := seedSource(ctx, t, pool, tenantID, "docs")
	sourceShop := seedSource(ctx, t, pool, tenantID, "shop")

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

	// docA tops both lists (its embedding equals the stubbed query vector and its
	// text carries both query terms). docC is vector-near, docB (shop) is unrelated.
	docA := putDoc(ctx, t, store, db, putSpec{
		src: sourceDocs, ext: "a", title: "X200 Reset Guide",
		uri: "https://docs.acme.com/x200", meta: `{"team":"support"}`,
		content: "How to reset the X200 device", emb: vec(1, 0, 0, 0, 0, 0, 0, 0), model: model,
	})
	_ = putDoc(ctx, t, store, db, putSpec{
		src: sourceDocs, ext: "c", title: "X200 Error Codes",
		uri: "https://docs.acme.com/errors", meta: `{"team":"engineering"}`,
		content: "X200 error codes reference", emb: vec(0.9, 0.1, 0, 0, 0, 0, 0, 0), model: model,
	})
	docB := putDoc(ctx, t, store, db, putSpec{
		src: sourceShop, ext: "b", title: "Pasta Recipe",
		uri: "https://shop.acme.com/pasta", meta: `{"team":"marketing"}`,
		content: "Pasta carbonara cooking recipe", emb: vec(0, 0, 0, 0, 0, 0, 0, 1), model: model,
	})

	// --- Assemble the REAL router: query scope resolves the tenant into context
	// (FR-ACC-03), settings come from the real control plane, only the embedder is
	// stubbed. RateLimit is left nil (pass-through). ---
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	svc := retrieve.NewService(resolver, settingsSvc, retrieveStubFactory{vec: vec(1, 0, 0, 0, 0, 0, 0, 0)})
	h := retrieve.NewHandlers(svc)

	deps := api.Deps{
		Log:               obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:           obs.NewMetrics(),
		RequireScopeQuery: injectTenantMW(tenantID),
		Retrieve:          http.HandlerFunc(h.Retrieve),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()
	client := srv.Client()

	// --- Golden path: docA ranks first, with score + citation metadata. ---
	chunks := postRetrieve(t, client, srv.URL, `{"query":"X200 reset","top_k":8}`)
	if len(chunks) == 0 {
		t.Fatal("no chunks returned")
	}
	if chunks[0].DocumentID != docA {
		t.Fatalf("top chunk document = %s, want docA %s", chunks[0].DocumentID, docA)
	}
	if chunks[0].URI != "https://docs.acme.com/x200" || chunks[0].Title != "X200 Reset Guide" ||
		chunks[0].SourceID != sourceDocs || chunks[0].Content == "" || chunks[0].Score <= 0 {
		t.Fatalf("top chunk metadata mismapped: %+v", chunks[0])
	}
	// The returned metadata is the CHUNK's metadata column (c.metadata); the shared
	// seed sets document metadata, not chunk metadata, so it round-trips as the empty
	// object here. (Document metadata drives the metadata FILTER — proven in the
	// STORY-08.1 retrieval e2e.) It must still be valid JSON, never null/garbage.
	if !json.Valid(chunks[0].Metadata) {
		t.Fatalf("top chunk metadata is not valid JSON: %s", chunks[0].Metadata)
	}

	// --- top_k is respected: a limit of 1 returns exactly one chunk. ---
	if got := postRetrieve(t, client, srv.URL, `{"query":"X200 reset","top_k":1}`); len(got) != 1 {
		t.Fatalf("top_k=1 returned %d chunks, want 1", len(got))
	}

	// --- Source filter (FR-RET-02): only the shop source's doc is returned. ---
	shopBody := fmt.Sprintf(`{"query":"X200 reset","top_k":8,"filters":{"source_ids":[%q]}}`, sourceShop)
	shop := postRetrieve(t, client, srv.URL, shopBody)
	if len(shop) != 1 || shop[0].DocumentID != docB {
		t.Fatalf("source=shop returned %d chunks (%+v), want only docB %s", len(shop), shop, docB)
	}

	// --- An empty query is a 400 validation envelope. ---
	resp, err := client.Post(srv.URL+"/v1/retrieve", "application/json", strings.NewReader(`{"query":"  "}`))
	if err != nil {
		t.Fatalf("POST empty query: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty query = %d, want 400", resp.StatusCode)
	}
}

// endpointChunk mirrors the response chunkView (SPEC-06/07 citation metadata).
type endpointChunk struct {
	ID          string          `json:"id"`
	DocumentID  string          `json:"document_id"`
	SourceID    string          `json:"source_id"`
	Content     string          `json:"content"`
	URI         string          `json:"uri"`
	Title       string          `json:"title"`
	HeadingPath []string        `json:"heading_path"`
	Metadata    json.RawMessage `json:"metadata"`
	Score       float64         `json:"score"`
}

func postRetrieve(t *testing.T, client *http.Client, base, body string) []endpointChunk {
	t.Helper()
	resp, err := client.Post(base+"/v1/retrieve", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/retrieve: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/retrieve = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Chunks []endpointChunk `json:"chunks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out.Chunks
}

// injectTenantMW is a stand-in for the query-scope middleware: it resolves the
// tenant into context (FR-ACC-03) so the handler reads it from context, never a
// body/param. The real gate additionally verifies the API key + scope (STORY-03.x).
func injectTenantMW(tenantID string) api.Middleware {
	id := tenant.ID(uuid.MustParse(tenantID))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(tenant.WithTenantID(r.Context(), id)))
		})
	}
}
