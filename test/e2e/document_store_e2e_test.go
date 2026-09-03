//go:build e2e

// STORY-05.1 golden path: the tenant-content document/version WRITE store
// (internal/documents.TenantStore.Put) against a REAL enrolled tenant database
// (up via `mise run up`), reached ONLY through a resolver + *tenant.DB
// (ADR-0003, C-3) — no HTTP, no mocks. It proves the ADR-0008 / SPEC-05 §5
// commit semantics and the STORY-05.1 acceptance contract:
//   - Put with an unchanged content hash only touches last_seen_at (no new
//     version, no chunk churn, Changed=false),
//   - a changed hash inserts a new immutable version and its chunks,
//   - current_version is flipped in the SAME transaction as the chunk writes, so
//     the live_chunks view reflects the swap instantly and never a half-built
//     version (Invariants 1 and 2),
//   - a rollback to a prior hash reuses that version and its chunks (immutable).
package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestDocumentStorePutGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// --- Enrol a real tenant with a dedicated database at 768-d embeddings. ---
	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "docstore-" + suffix
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
	const dim = 768
	if out, exit := runEnroll(t, ageKey, blob, slug, "DocStore "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	tenantID := tenantScalar(t, slug, "t.id")

	// Informational control-plane source id (Invariant 4: no cross-DB FK).
	var sourceID string
	if err := pool.QueryRow(ctx,
		`insert into sources (tenant_id, kind, name, status) values ($1, 'upload', 'uploads', 'active') returning id::text`,
		tenantID).Scan(&sourceID); err != nil {
		t.Fatalf("seed upload source: %v", err)
	}

	// --- Open the tenant DB the only supported way: resolver.Open (ADR-0003). ---
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

	external := "handbook-" + suffix + ".md"
	embed := func(seed float32) []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = seed + float32(i)*1e-3
		}
		return v
	}
	title := "Employee Handbook"
	parser := "markdown"

	v1 := documents.PutInput{
		SourceID:    sourceID,
		ExternalID:  external,
		Title:       &title,
		ContentHash: sha256Bytes("v1 content " + suffix),
		Content:     "v1 content " + suffix,
		CharCount:   len("v1 content " + suffix),
		Parser:      &parser,
		Chunks: []documents.ChunkInput{{
			Position: 0, HeadingPath: []string{"Intro"}, Content: "v1 chunk",
			TokenCount: 3, Embedding: embed(0.1), EmbeddingModel: "text-embedding-3-small",
		}},
	}

	// --- New document: a version + chunk are written and current_version is set. ---
	r1, err := store.Put(ctx, db, v1)
	if err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if !r1.Changed {
		t.Fatalf("Put v1 Changed = false, want true (new document)")
	}
	docID := r1.DocumentID
	if got := tenantScalarDB(ctx, t, db, `select count(*) from document_versions where document_id = $1::uuid`, docID); got != "1" {
		t.Fatalf("version count after v1 = %s, want 1", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from chunks where document_id = $1::uuid`, docID); got != "1" {
		t.Fatalf("chunk count after v1 = %s, want 1", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select current_version::text from documents where id = $1::uuid`, docID); got != r1.VersionID {
		t.Fatalf("current_version after v1 = %s, want %s", got, r1.VersionID)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from live_chunks where document_id = $1::uuid`, docID); got != "1" {
		t.Fatalf("live_chunks after v1 = %s, want 1", got)
	}
	ls1 := lastSeen(ctx, t, db, docID)

	// --- Unchanged hash: only last_seen_at is touched. ---
	time.Sleep(5 * time.Millisecond)
	r2, err := store.Put(ctx, db, v1)
	if err != nil {
		t.Fatalf("Put v1 again: %v", err)
	}
	if r2.Changed {
		t.Fatalf("Put unchanged Changed = true, want false")
	}
	if r2.VersionID != r1.VersionID {
		t.Fatalf("Put unchanged flipped version: %s -> %s", r1.VersionID, r2.VersionID)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from document_versions where document_id = $1::uuid`, docID); got != "1" {
		t.Fatalf("unchanged Put created a version (count=%s)", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from chunks where document_id = $1::uuid`, docID); got != "1" {
		t.Fatalf("unchanged Put churned chunks (count=%s)", got)
	}
	if ls2 := lastSeen(ctx, t, db, docID); !ls2.After(ls1) {
		t.Fatalf("unchanged Put did not advance last_seen_at: %s !> %s", ls2, ls1)
	}

	// --- Changed hash: new version + chunks, atomic flip, instant live_chunks. ---
	v2 := v1
	v2.ContentHash = sha256Bytes("v2 content " + suffix)
	v2.Content = "v2 content " + suffix
	v2.Chunks = []documents.ChunkInput{
		{Position: 0, Content: "v2 chunk a", TokenCount: 4, Embedding: embed(0.2), EmbeddingModel: "text-embedding-3-small"},
		{Position: 1, Content: "v2 chunk b", TokenCount: 4, Embedding: embed(0.3), EmbeddingModel: "text-embedding-3-small"},
	}
	r3, err := store.Put(ctx, db, v2)
	if err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	if !r3.Changed || r3.VersionID == r1.VersionID {
		t.Fatalf("Put v2 = %+v, want Changed with a new version id", r3)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from document_versions where document_id = $1::uuid`, docID); got != "2" {
		t.Fatalf("version count after v2 = %s, want 2", got)
	}
	// current_version was flipped in the same tx as the chunk writes.
	if got := tenantScalarDB(ctx, t, db, `select current_version::text from documents where id = $1::uuid`, docID); got != r3.VersionID {
		t.Fatalf("current_version after v2 = %s, want %s", got, r3.VersionID)
	}
	// live_chunks reflects the swap instantly: only v2's two chunks, none of v1's.
	live := liveChunkContents(ctx, t, db, docID)
	if len(live) != 2 || live[0] != "v2 chunk a" || live[1] != "v2 chunk b" {
		t.Fatalf("live_chunks after v2 = %v, want [v2 chunk a, v2 chunk b]", live)
	}
	// v1's chunk is retained (immutable), so the physical table holds 1 + 2 = 3.
	if got := tenantScalarDB(ctx, t, db, `select count(*) from chunks where document_id = $1::uuid`, docID); got != "3" {
		t.Fatalf("total chunk count after v2 = %s, want 3 (v1 retained)", got)
	}
	// The live view must never expose a chunk of a non-current version.
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from live_chunks where document_id = $1::uuid and version_id <> $2::uuid`, docID, r3.VersionID); got != "0" {
		t.Fatalf("live_chunks leaked non-current-version chunks (count=%s)", got)
	}

	// --- Rollback: the v1 hash reappears; reuse that version and its chunks. ---
	r4, err := store.Put(ctx, db, v1)
	if err != nil {
		t.Fatalf("Put v1 rollback: %v", err)
	}
	if !r4.Changed || r4.VersionID != r1.VersionID {
		t.Fatalf("rollback Put = %+v, want Changed with reused version %s", r4, r1.VersionID)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from document_versions where document_id = $1::uuid`, docID); got != "2" {
		t.Fatalf("rollback created a version (count=%s), want 2", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select count(*) from chunks where document_id = $1::uuid`, docID); got != "3" {
		t.Fatalf("rollback re-inserted chunks (count=%s), want 3", got)
	}
	if got := tenantScalarDB(ctx, t, db, `select current_version::text from documents where id = $1::uuid`, docID); got != r1.VersionID {
		t.Fatalf("current_version after rollback = %s, want %s", got, r1.VersionID)
	}
	if live := liveChunkContents(ctx, t, db, docID); len(live) != 1 || live[0] != "v1 chunk" {
		t.Fatalf("live_chunks after rollback = %v, want [v1 chunk]", live)
	}
}

// sha256Bytes is a content hash matching document_versions.content_hash (sha256).
func sha256Bytes(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// tenantScalarDB runs a one-column, one-row query on the tenant handle.
func tenantScalarDB(ctx context.Context, t *testing.T, db *tenant.DB, sql string, args ...any) string {
	t.Helper()
	var out string
	if err := db.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
		t.Fatalf("tenant scalar %q: %v", sql, err)
	}
	return out
}

// lastSeen reads documents.last_seen_at for a document.
func lastSeen(ctx context.Context, t *testing.T, db *tenant.DB, docID string) time.Time {
	t.Helper()
	var ts time.Time
	if err := db.QueryRow(ctx, `select last_seen_at from documents where id = $1::uuid`, docID).Scan(&ts); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	return ts
}

// liveChunkContents returns the current-version chunk contents (ordered) from the
// live_chunks view — what retrieval would see.
func liveChunkContents(ctx context.Context, t *testing.T, db *tenant.DB, docID string) []string {
	t.Helper()
	rows, err := db.Query(ctx, `select content from live_chunks where document_id = $1::uuid order by position`, docID)
	if err != nil {
		t.Fatalf("query live_chunks: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan live_chunks: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("live_chunks rows: %v", err)
	}
	return out
}
