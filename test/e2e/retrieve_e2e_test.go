//go:build e2e

// STORY-08.1 golden path: the hybrid retrieval query (internal/retrieve.Retrieve,
// SPEC-06 §2, FR-RET-01/02/08, ADR-0007) against a REAL enrolled tenant database
// (up via `mise run up`), reached ONLY through a resolver + *tenant.DB
// (ADR-0003, C-1, C-3) — no HTTP, no mocks. Content is seeded through the real
// write store (documents.TenantStore.Put) so live_chunks/documents are populated
// exactly as ingestion would. It proves:
//   - vector + full-text results are fused with RRF (k=60) in one round trip, and
//     the chunk that tops BOTH lists ranks first with the exact spec score,
//   - the source, uri-prefix, date-range and metadata-tag filters each narrow
//     the candidate set correctly (FR-RET-02),
//   - the uri-prefix filter escapes LIKE metacharacters, so a client-supplied '%'
//     is a literal, not a wildcard (injection safety),
//   - retrieval reads only live_chunks (a soft-deleted document disappears).
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestHybridRetrievalGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const dim = 8
	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "retrieve-" + suffix
	// Teardown talks to the control-plane over the same pgxpool the resolver uses
	// (localhost:5432), never `docker compose exec`.
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
	if out, exit := runEnroll(t, ageKey, blob, slug, "Retrieve "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `select t.id::text from tenants t where t.slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}

	// Two informational control-plane sources (Invariant 4: no cross-DB FK).
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

	// oneHot builds a unit-ish vector so cosine distance is controllable.
	vec := func(v ...float32) []float32 {
		out := make([]float32, dim)
		copy(out, v)
		return out
	}
	const model = "text-embedding-3-small"

	// A tops both lists: its embedding equals the query and its text has both query
	// terms. C is vector-near but its text lacks "reset". B is unrelated.
	docA := putDoc(ctx, t, store, db, putSpec{
		src: sourceDocs, ext: "a", title: "X200 Reset Guide",
		uri: "https://docs.acme.com/x200", meta: `{"team":"support"}`,
		content: "How to reset the X200 device", emb: vec(1, 0, 0, 0, 0, 0, 0, 0), model: model,
	})
	docC := putDoc(ctx, t, store, db, putSpec{
		src: sourceDocs, ext: "c", title: "X200 Error Codes",
		uri: "https://docs.acme.com/errors", meta: `{"team":"engineering"}`,
		content: "X200 error codes reference", emb: vec(0.9, 0.1, 0, 0, 0, 0, 0, 0), model: model,
	})
	docB := putDoc(ctx, t, store, db, putSpec{
		src: sourceShop, ext: "b", title: "Pasta Recipe",
		uri: "https://shop.acme.com/pasta", meta: `{"team":"marketing"}`,
		content: "Pasta carbonara cooking recipe", emb: vec(0, 0, 0, 0, 0, 0, 0, 1), model: model,
	})

	query := retrieve.Params{Embedding: vec(1, 0, 0, 0, 0, 0, 0, 0), QueryText: "X200 reset"}

	// --- Fusion: A tops both lists and ranks first with the exact RRF score. ---
	res, err := retrieve.Retrieve(ctx, db, query)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("Retrieve returned no results")
	}
	if res[0].DocumentID != docA {
		t.Fatalf("top result = %s, want docA %s (order: %s)", res[0].DocumentID, docA, ids(res))
	}
	if res[0].URI != "https://docs.acme.com/x200" || res[0].Title != "X200 Reset Guide" {
		t.Fatalf("top result metadata = %q / %q", res[0].URI, res[0].Title)
	}
	// A is rank 1 in the vector list (distance 0) and rank 1 in the text list (only
	// doc matching both "x200" AND "reset"), so its fused score is exactly 2/(60+1).
	want := retrieve.RRFScore(1, 1)
	if math.Abs(res[0].Score-want) > 1e-9 {
		t.Fatalf("top fused score = %v, want %v (RRFScore(1,1))", res[0].Score, want)
	}
	// A scores strictly above the vector-only candidates.
	for _, r := range res[1:] {
		if r.Score >= res[0].Score {
			t.Fatalf("candidate %s scored %v >= top %v", r.DocumentID, r.Score, res[0].Score)
		}
	}

	// --- Source filter: only the shop source (docB), vector-reachable, is returned. ---
	got := retrieve.Params{Embedding: vec(1, 0, 0, 0, 0, 0, 0, 0), QueryText: "X200 reset",
		Filters: retrieve.Filters{SourceIDs: []string{sourceShop}}}
	assertOnly(ctx, t, db, got, docB, "source=shop")

	// --- URI-prefix filter: docs.acme.com/ excludes the shop doc. ---
	pfx := retrieve.Params{Embedding: vec(1, 0, 0, 0, 0, 0, 0, 0), QueryText: "X200 reset",
		Filters: retrieve.Filters{URIPrefix: "https://docs.acme.com/"}}
	assertContainsExcludes(ctx, t, db, pfx, []string{docA, docC}, []string{docB}, "uri prefix docs")

	// --- URI-prefix injection: a literal '%' must not act as a wildcard. ---
	inj := retrieve.Params{Embedding: vec(1, 0, 0, 0, 0, 0, 0, 0), QueryText: "X200 reset",
		Filters: retrieve.Filters{URIPrefix: "https://docs.acme.com/%"}}
	if r := mustRetrieve(ctx, t, db, inj); len(r) != 0 {
		t.Fatalf("uri-prefix with literal %% matched %d rows, want 0 (wildcard was not escaped): %s", len(r), ids(r))
	}

	// --- Metadata-tag filter: {"team":"support"} selects only docA. ---
	tags := retrieve.Params{Embedding: vec(1, 0, 0, 0, 0, 0, 0, 0), QueryText: "X200 reset",
		Filters: retrieve.Filters{MetadataTags: json.RawMessage(`{"team":"support"}`)}}
	assertOnly(ctx, t, db, tags, docA, "metadata team=support")

	// --- Date-range filter: a future lower bound excludes everything. ---
	future := time.Now().Add(1 * time.Hour)
	dr := retrieve.Params{Embedding: vec(1, 0, 0, 0, 0, 0, 0, 0), QueryText: "X200 reset",
		Filters: retrieve.Filters{DateFrom: &future}}
	if r := mustRetrieve(ctx, t, db, dr); len(r) != 0 {
		t.Fatalf("date-from in the future matched %d rows, want 0: %s", len(r), ids(r))
	}

	// --- live_chunks only: soft-deleting docA removes it from retrieval. ---
	if _, err := store.SoftDelete(ctx, db, docA); err != nil {
		t.Fatalf("soft delete docA: %v", err)
	}
	after := mustRetrieve(ctx, t, db, query)
	for _, r := range after {
		if r.DocumentID == docA {
			t.Fatalf("soft-deleted docA still retrieved: %s", ids(after))
		}
	}
}

// --- helpers -------------------------------------------------------------------

func seedSource(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tenantID, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`insert into sources (tenant_id, kind, name, status) values ($1, 'upload', $2, 'active') returning id::text`,
		tenantID, name).Scan(&id); err != nil {
		t.Fatalf("seed source %q: %v", name, err)
	}
	return id
}

type putSpec struct {
	src, ext, title, uri, meta, content, model string
	emb                                        []float32
}

func putDoc(ctx context.Context, t *testing.T, store documents.TenantStore, db *tenant.DB, s putSpec) string {
	t.Helper()
	title := s.title
	uri := s.uri
	parser := "markdown"
	r, err := store.Put(ctx, db, documents.PutInput{
		SourceID:    s.src,
		ExternalID:  s.ext,
		Title:       &title,
		URI:         &uri,
		Metadata:    json.RawMessage(s.meta),
		ContentHash: sha256Bytes(s.content),
		Content:     s.content,
		CharCount:   len(s.content),
		Parser:      &parser,
		Chunks: []documents.ChunkInput{{
			Position: 0, Content: s.content, TokenCount: 5,
			Embedding: s.emb, EmbeddingModel: s.model,
		}},
	})
	if err != nil {
		t.Fatalf("put %s: %v", s.ext, err)
	}
	return r.DocumentID
}

func mustRetrieve(ctx context.Context, t *testing.T, db *tenant.DB, p retrieve.Params) []retrieve.Result {
	t.Helper()
	r, err := retrieve.Retrieve(ctx, db, p)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	return r
}

func assertOnly(ctx context.Context, t *testing.T, db *tenant.DB, p retrieve.Params, wantDoc, label string) {
	t.Helper()
	r := mustRetrieve(ctx, t, db, p)
	if len(r) != 1 || r[0].DocumentID != wantDoc {
		t.Fatalf("%s: got %s, want only %s", label, ids(r), wantDoc)
	}
}

func assertContainsExcludes(ctx context.Context, t *testing.T, db *tenant.DB, p retrieve.Params, want, exclude []string, label string) {
	t.Helper()
	r := mustRetrieve(ctx, t, db, p)
	set := map[string]bool{}
	for _, x := range r {
		set[x.DocumentID] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Fatalf("%s: missing %s (got %s)", label, w, ids(r))
		}
	}
	for _, e := range exclude {
		if set[e] {
			t.Fatalf("%s: unexpectedly included %s (got %s)", label, e, ids(r))
		}
	}
}

func ids(r []retrieve.Result) string {
	var b strings.Builder
	for i, x := range r {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s(%.4f)", x.DocumentID, x.Score)
	}
	return b.String()
}
