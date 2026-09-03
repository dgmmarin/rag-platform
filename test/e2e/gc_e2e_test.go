//go:build e2e

// STORY-05.9 golden path: the retention garbage-collection sweep
// (documents.TenantStore.CollectGarbage, SPEC-03 §4, the SPEC-08 gc_tenant job) over
// a REAL enrolled tenant database, reached ONLY through a resolver + *tenant.DB
// (ADR-0003, C-3). It seeds each of the four SPEC-03 §4 retention classes with both a
// collectible row (past its window) and a retained row (within it), plus live data,
// then proves CollectGarbage removes exactly the collectible rows (and their chunks by
// cascade), leaves live/current data untouched, reports the right per-class counts,
// and is idempotent on a second run.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// execDB runs a statement on the tenant handle, failing the test on error.
func execDB(ctx context.Context, t *testing.T, db *tenant.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func TestGCRetentionSweep(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "gc-" + suffix
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		var dbName, role string
		_ = pool.QueryRow(cctx,
			`select d.database_name, d.username from tenants t
			   join tenant_databases d on d.tenant_id = t.id where t.slug = $1`, slug).Scan(&dbName, &role)
		if dbName != "" {
			_, _ = pool.Exec(cctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		}
		if role != "" {
			_, _ = pool.Exec(cctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
		}
		_, _ = pool.Exec(cctx, `DELETE FROM tenants WHERE slug = $1`, slug)
	})

	const dim = 4
	if out, exit := runEnroll(t, ageKey, blob, slug, "GC "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `select id::text from tenants where slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}

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

	now := time.Now()
	source := uuid.NewString()
	emb := "[0,0,0,0]" // dim=4

	// --- Document A (active): two OLD non-current versions + a current version. ---
	// The old versions (and their chunks) are collectible; the current version and its
	// live chunks must survive.
	docA := uuid.NewString()
	vOld1, vOld2, vCur := uuid.NewString(), uuid.NewString(), uuid.NewString()
	execDB(ctx, t, db, `insert into documents (id, source_id, external_id, status, first_seen_at, last_seen_at)
		values ($1::uuid, $2::uuid, 'a', 'active', $3, $3)`, docA, source, now.Add(-100*24*time.Hour))
	execDB(ctx, t, db, `insert into document_versions (id, document_id, content_hash, content, char_count, created_at)
		values ($1::uuid, $2::uuid, $3, 'old one', 7, $4)`, vOld1, docA, sha256Bytes("A-old1"), now.Add(-40*24*time.Hour))
	execDB(ctx, t, db, `insert into document_versions (id, document_id, content_hash, content, char_count, created_at)
		values ($1::uuid, $2::uuid, $3, 'old two', 7, $4)`, vOld2, docA, sha256Bytes("A-old2"), now.Add(-35*24*time.Hour))
	execDB(ctx, t, db, `insert into document_versions (id, document_id, content_hash, content, char_count, created_at)
		values ($1::uuid, $2::uuid, $3, 'current', 7, $4)`, vCur, docA, sha256Bytes("A-cur"), now.Add(-1*24*time.Hour))
	execDB(ctx, t, db, `update documents set current_version = $2::uuid where id = $1::uuid`, docA, vCur)
	insChunk := `insert into chunks (document_id, version_id, source_id, position, content, token_count, embedding, embedding_model)
		values ($1::uuid, $2::uuid, $3::uuid, $4, $5, 3, $6::vector, 'm')`
	execDB(ctx, t, db, insChunk, docA, vOld1, source, 0, "old1 chunk", emb)
	execDB(ctx, t, db, insChunk, docA, vOld2, source, 0, "old2 chunk", emb)
	execDB(ctx, t, db, insChunk, docA, vCur, source, 0, "cur chunk 0", emb)
	execDB(ctx, t, db, insChunk, docA, vCur, source, 1, "cur chunk 1", emb)

	// --- Document B (soft-deleted past grace): fully collectible. ---
	docB := uuid.NewString()
	vB := uuid.NewString()
	execDB(ctx, t, db, `insert into documents (id, source_id, external_id, status, deleted_at, first_seen_at, last_seen_at)
		values ($1::uuid, $2::uuid, 'b', 'deleted', $3, $4, $4)`, docB, source, now.Add(-40*24*time.Hour), now.Add(-100*24*time.Hour))
	execDB(ctx, t, db, `insert into document_versions (id, document_id, content_hash, content, char_count, created_at)
		values ($1::uuid, $2::uuid, $3, 'b body', 6, $4)`, vB, docB, sha256Bytes("B"), now.Add(-50*24*time.Hour))
	execDB(ctx, t, db, `update documents set current_version = $2::uuid where id = $1::uuid`, docB, vB)
	execDB(ctx, t, db, insChunk, docB, vB, source, 0, "b chunk 0", emb)
	execDB(ctx, t, db, insChunk, docB, vB, source, 1, "b chunk 1", emb)

	// --- Document C (soft-deleted within grace): must survive. ---
	docC := uuid.NewString()
	vC := uuid.NewString()
	execDB(ctx, t, db, `insert into documents (id, source_id, external_id, status, deleted_at, first_seen_at, last_seen_at)
		values ($1::uuid, $2::uuid, 'c', 'deleted', $3, $3, $3)`, docC, source, now.Add(-5*24*time.Hour))
	execDB(ctx, t, db, `insert into document_versions (id, document_id, content_hash, content, char_count, created_at)
		values ($1::uuid, $2::uuid, $3, 'c body', 6, $4)`, vC, docC, sha256Bytes("C"), now.Add(-5*24*time.Hour))
	execDB(ctx, t, db, `update documents set current_version = $2::uuid where id = $1::uuid`, docC, vC)

	// --- Query log: two OLD rows (one rated) collectible, one RECENT retained. ---
	qlOld1, qlOld2, qlRecent := uuid.NewString(), uuid.NewString(), uuid.NewString()
	execDB(ctx, t, db, `insert into query_log (id, question, created_at) values ($1::uuid, 'old q1', $2)`, qlOld1, now.Add(-100*24*time.Hour))
	execDB(ctx, t, db, `insert into query_log (id, question, created_at) values ($1::uuid, 'old q2', $2)`, qlOld2, now.Add(-95*24*time.Hour))
	execDB(ctx, t, db, `insert into query_log (id, question, created_at) values ($1::uuid, 'recent q', $2)`, qlRecent, now.Add(-5*24*time.Hour))
	execDB(ctx, t, db, `insert into query_feedback (query_id, rating) values ($1::uuid, 1)`, qlOld1)

	// --- Crawl pages: one STALE collectible, one recent + one never-fetched retained. ---
	execDB(ctx, t, db, `insert into crawl_pages (source_id, url, normalized_url, last_fetched_at)
		values ($1::uuid, 'http://x/stale', 'x/stale', $2)`, source, now.Add(-40*24*time.Hour))
	execDB(ctx, t, db, `insert into crawl_pages (source_id, url, normalized_url, last_fetched_at)
		values ($1::uuid, 'http://x/fresh', 'x/fresh', $2)`, source, now.Add(-5*24*time.Hour))
	execDB(ctx, t, db, `insert into crawl_pages (source_id, url, normalized_url, last_fetched_at)
		values ($1::uuid, 'http://x/pending', 'x/pending', null)`, source)

	// --- Collect. BatchSize=1 forces the drain loop to run multiple bounded batches. ---
	policy := documents.GCPolicy{
		VersionRetention:    30 * 24 * time.Hour,
		DeletedDocRetention: 30 * 24 * time.Hour,
		QueryLogRetention:   90 * 24 * time.Hour,
		CrawlPageStale:      21 * 24 * time.Hour,
		BatchSize:           1,
	}
	m, err := store.CollectGarbage(ctx, db, policy, now)
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	want := documents.GCMetrics{OldVersions: 2, DeletedDocs: 1, QueryLogs: 2, CrawlPages: 1, Chunks: 4}
	if m != want {
		t.Fatalf("metrics = %+v, want %+v", m, want)
	}

	// --- Old non-current versions and their chunks are gone; the current one survives. ---
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from document_versions where id = any($1::uuid[])`, []string{vOld1, vOld2}); got != "0" {
		t.Fatalf("old versions remain: %s", got)
	}
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from chunks where version_id = any($1::uuid[])`, []string{vOld1, vOld2}); got != "0" {
		t.Fatalf("old-version chunks remain: %s", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from document_versions where id = $1::uuid`, vCur); got != "1" {
		t.Fatalf("current version missing: %s", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from live_chunks where document_id = $1::uuid`, docA); got != "2" {
		t.Fatalf("live_chunks for A = %s, want 2 (current chunks untouched)", got)
	}

	// --- Deleted-past-grace document B gone entirely; within-grace C survives. ---
	if got := tenantScalarDB(ctx, t, db, `select count(*) from documents where id = $1::uuid`, docB); got != "0" {
		t.Fatalf("deleted document B remains: %s", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from document_versions where document_id = $1::uuid`, docB); got != "0" {
		t.Fatalf("document B versions remain (cascade): %s", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from chunks where document_id = $1::uuid`, docB); got != "0" {
		t.Fatalf("document B chunks remain (cascade): %s", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from documents where id = $1::uuid`, docC); got != "1" {
		t.Fatalf("within-grace document C was collected: %s", got)
	}

	// --- Query log: old rows gone (feedback cascaded), recent retained. ---
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from query_log where id = any($1::uuid[])`, []string{qlOld1, qlOld2}); got != "0" {
		t.Fatalf("old query_log rows remain: %s", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from query_feedback where query_id = $1::uuid`, qlOld1); got != "0" {
		t.Fatalf("query_feedback did not cascade: %s", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from query_log where id = $1::uuid`, qlRecent); got != "1" {
		t.Fatalf("recent query_log was collected: %s", got)
	}

	// --- Crawl pages: stale gone; fresh + never-fetched retained. ---
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from crawl_pages where source_id = $1::uuid and normalized_url = 'x/stale'`, source); got != "0" {
		t.Fatalf("stale crawl page remains: %s", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from crawl_pages where source_id = $1::uuid`, source); got != "2" {
		t.Fatalf("crawl_pages remaining = %s, want 2 (fresh + never-fetched)", got)
	}

	// --- Idempotent: a second run over the same now removes nothing new. ---
	m2, err := store.CollectGarbage(ctx, db, policy, now)
	if err != nil {
		t.Fatalf("second CollectGarbage: %v", err)
	}
	if (m2 != documents.GCMetrics{}) {
		t.Fatalf("second run removed rows: %+v, want all zero", m2)
	}
}
