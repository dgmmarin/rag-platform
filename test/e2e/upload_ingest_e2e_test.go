//go:build e2e

// STORY-06.3 golden path: the upload connector + ingest_document handler end to
// end against the REAL local stack (up via `mise run up`) — real MinIO object
// storage, the real control-plane Postgres, and a REAL enrolled tenant database
// reached only through the resolver + *tenant.DB (ADR-0003, C-3). The embedding
// PROVIDER is external, so it is stubbed with a deterministic Embedder (the same
// seam the sink e2e uses); everything else is real. The test proves the AC
// "upload → object storage → job → document; re-upload creates a new version":
//   - documents.Service.Ingest writes the raw bytes to MinIO (verified by reading
//     them back through the S3 client) and enqueues a real ingest_document job,
//   - the implicit upload source is resolved/created (SPEC-04 §5),
//   - ingestdoc.Ingestor.Dispatch fetches the bytes, runs parse→chunk→embed→commit
//     and atomically creates the document + its first version and chunks,
//   - a re-upload of the same filename with new content creates a SECOND version
//     and flips documents.current_version to it (STORY-05.1 versioning).
package e2e

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/ingestdoc"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/objectstore"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// uploadStubEmbedder returns dim-length vectors so the commit hits the real
// vector(dim) column without reaching an external embedding provider.
type uploadStubEmbedder struct{ dim int }

func (e uploadStubEmbedder) Embed(_ context.Context, texts []string) (embed.Result, error) {
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		v := make([]float32, e.dim)
		for j := range v {
			v[j] = float32(i+1) * 1e-3
		}
		vecs[i] = v
	}
	return embed.Result{Vectors: vecs, Tokens: len(texts) * 5}, nil
}

// uploadStubFactory builds a stub embedder sized to the tenant's settings dim.
type uploadStubFactory struct{}

func (uploadStubFactory) Embedder(_ context.Context, s ingestdoc.Settings) (embed.Embedder, error) {
	return uploadStubEmbedder{dim: s.EmbeddingDim}, nil
}

func TestUploadIngestGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "upl-" + suffix
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
	if out, exit := runEnroll(t, ageKey, blob, slug, "Upload "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `select id::text from tenants where slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}

	// --- Real object storage (MinIO) via the S3 client. ---
	store, err := objectstore.New(ctx, objectstore.Config{
		Endpoint:  "http://localhost:" + hostPort("MINIO_PORT", "9000"),
		AccessKey: hostPort("MINIO_ROOT_USER", "minio"),
		SecretKey: hostPort("MINIO_ROOT_PASSWORD", "minio12345"),
		Bucket:    "rag-uploads-" + suffix,
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}

	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))

	// --- The upload service (POST /v1/documents core): real storage + implicit
	// upload source + real jobs table. ---
	svc := documents.NewService(tenant.NewResolver(tenant.Config{ControlPool: pool}), documents.NewTenantStore(), documents.JobsFromPool(pool))
	svc.Storage = store
	svc.UploadSource = documents.UploadSourceFromPool(pool)
	svc.Limits = documents.SettingsUploadLimits{Settings: settingsSvc}

	// --- The ingest_document handler: real resolver, storage, settings, tenant
	// store; stub embedder (external provider). ---
	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher, CacheTTL: 50 * time.Millisecond})
	ingestor := &ingestdoc.Ingestor{
		Resolver: resolver,
		Storage:  store,
		Settings: settingsSvc,
		Embedder: uploadStubFactory{},
		Store:    documents.NewTenantStore(),
		Local:    parse.Default(),
		Now:      time.Now,
	}

	db, err := resolver.Open(ctx, tenant.ID(uuid.MustParse(tenantID)))
	if err != nil {
		t.Fatalf("resolver.Open: %v", err)
	}

	const filename = "handbook.md"

	// upload runs the whole chain: service.Ingest (store bytes + enqueue job) then
	// dispatch the queued ingest_document job through the handler.
	upload := func(body string) ingestdoc.Job {
		t.Helper()
		job, ierr := svc.Ingest(ctx, documents.IngestParams{
			TenantID:    tenantID,
			Filename:    filename,
			ContentType: "text/markdown",
			Size:        int64(len(body)),
			Reader:      strings.NewReader(body),
		})
		if ierr != nil {
			t.Fatalf("service.Ingest: %v", ierr)
		}
		if job.Kind != "ingest_document" || job.Status != "queued" {
			t.Fatalf("enqueued job = %+v", job)
		}
		ij, jerr := ingestdoc.JobFromPayload(tenantID, job.Payload)
		if jerr != nil {
			t.Fatalf("JobFromPayload: %v", jerr)
		}
		// The bytes must actually be in MinIO (object storage round trip).
		rc, gerr := store.Get(ctx, ij.ObjectKey)
		if gerr != nil {
			t.Fatalf("object storage get %q: %v", ij.ObjectKey, gerr)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != body {
			t.Fatalf("object bytes = %q, want %q", got, body)
		}
		if _, derr := ingestor.Dispatch(ctx, ij); derr != nil {
			t.Fatalf("ingestor.Dispatch: %v", derr)
		}
		return ij
	}

	// --- First upload → document + first version. ---
	first := upload("# Handbook\n\nWelcome to the first edition of the handbook.\n")

	var docID, curVer1 string
	if err := db.QueryRow(ctx, `
		select d.id::text, d.current_version::text
		from documents d where d.source_id = $1::uuid and d.external_id = $2`,
		first.SourceID, filename).Scan(&docID, &curVer1); err != nil {
		t.Fatalf("read document after first upload: %v", err)
	}
	if curVer1 == "" {
		t.Fatal("active document must have a non-null current_version (SPEC-03 §2 invariant 1)")
	}
	var status string
	if err := db.QueryRow(ctx, `select status::text from documents where id = $1::uuid`, docID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "active" {
		t.Fatalf("document status = %q, want active", status)
	}
	var chunkCount int
	if err := db.QueryRow(ctx, `select count(*) from chunks where document_id = $1::uuid and version_id = $2::uuid`, docID, curVer1).Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkCount == 0 {
		t.Fatal("expected the first version to have chunks")
	}

	// --- Re-upload same filename, new content → a NEW version, current flipped. ---
	upload("# Handbook\n\nSecond edition: substantially revised and expanded content.\n")

	var curVer2 string
	if err := db.QueryRow(ctx, `select current_version::text from documents where id = $1::uuid`, docID).Scan(&curVer2); err != nil {
		t.Fatalf("read current_version after re-upload: %v", err)
	}
	if curVer2 == curVer1 {
		t.Fatalf("re-upload did not create a new version (current_version stayed %s)", curVer1)
	}
	var versionCount int
	if err := db.QueryRow(ctx, `select count(*) from document_versions where document_id = $1::uuid`, docID).Scan(&versionCount); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versionCount != 2 {
		t.Fatalf("document_versions = %d, want 2 after a re-upload", versionCount)
	}
}
