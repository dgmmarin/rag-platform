//go:build e2e

// STORY-04.4 golden path: the REAL public HTTP router (internal/api) served over a
// real net/http listener against a REAL enrolled tenant database plus the real
// control-plane Postgres (up via `mise run up`), no mocks. Documents/versions/
// chunks are TENANT content (schemas/tenant.sql), reached only through the
// resolver + tenant.DB (ADR-0003, C-3); the ingest_document job is control-plane
// queue state. The test proves, through the assembled API-key chain:
//   - list/get/get?content=true/chunks read the tenant's own content via the
//     resolver (tenant derived from the API key, never a parameter — FR-ACC-03),
//   - filters (source, status, q) and the current-version metadata work,
//   - a soft delete flips the document to 'deleted' in the real tenant DB,
//   - POST /v1/documents (multipart) enqueues a real ingest_document job in the
//     control-plane jobs table (object storage is the EPIC-06 seam, supplied here
//     by a tiny in-test Storage so the enqueue path is exercised end to end),
//   - an unknown id is 404 and an unauthenticated request is 401.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/ratelimit"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// memStorage is a stand-in for the EPIC-06 object store: it records the uploads so
// the ingest enqueue path can run end to end without MinIO/S3.
type memStorage struct{ puts int }

func (m *memStorage) Put(_ context.Context, _ string, _ string, r io.Reader) error {
	m.puts++
	_, _ = io.Copy(io.Discard, r)
	return nil
}

func TestDocumentsGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// --- Enrol a real tenant with a dedicated database + least-privilege role. ---
	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "docs-" + suffix
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
	if out, exit := runEnroll(t, ageKey, blob, slug, "Docs Test "+suffix, 768); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}
	tenantID := tenantScalar(t, slug, "t.id")
	dbName := tenantScalar(t, slug, "d.database_name")
	role := tenantScalar(t, slug, "d.username")
	encHex := psqlScalar(t, fmt.Sprintf(
		"select encode(d.password_enc, 'hex') from tenant_databases d join tenants t on t.id = d.tenant_id where t.slug = '%s'", slug))
	password := decryptHex(t, encHex)

	// --- A control-plane 'upload' source: informational source_id for documents,
	// and the FK target for the ingest_document job. ---
	var sourceID string
	if err := pool.QueryRow(ctx,
		`insert into sources (tenant_id, kind, name, status) values ($1, 'upload', 'uploads', 'active') returning id::text`,
		tenantID).Scan(&sourceID); err != nil {
		t.Fatalf("seed upload source: %v", err)
	}

	// --- Seed one document + version + chunk in the tenant DB, as the tenant's own
	// role (proving the row is reachable only through that tenant's connection).
	// Sequential statements (one psql -c => one transaction), not data-modifying
	// CTEs which cannot see each other's writes; the deferred current_version FK is
	// satisfied at commit. Explicit ids so the test does not depend on a returning. ---
	seedDocID := uuid.NewString()
	versionID := uuid.NewString()
	docExternal := "handbook-" + suffix + ".md"
	content := "The onboarding handbook for " + suffix
	seed := fmt.Sprintf(`
		insert into documents (id, source_id, external_id, title, status)
		values ('%s', '%s', '%s', 'Employee Handbook', 'active');
		insert into document_versions (id, document_id, content_hash, content, char_count, parser)
		values ('%s', '%s', sha256('%s'::bytea), '%s', %d, 'markdown');
		update documents set current_version = '%s' where id = '%s';
		insert into chunks (document_id, version_id, source_id, position, content, token_count, embedding_model)
		values ('%s', '%s', '%s', 0, '%s', 7, 'text-embedding-3-small');`,
		seedDocID, sourceID, docExternal,
		versionID, seedDocID, content, content, len(content),
		versionID, seedDocID,
		seedDocID, versionID, sourceID, content)
	execPsqlAs(t, role, password, dbName, seed)

	// --- Build the SAME router serve builds: real resolver + documents handlers. ---
	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher, CacheTTL: 50 * time.Millisecond})

	keySvc := auth.NewAPIKeyService(auth.MembershipFromPool(pool))
	_, secret, err := keySvc.Create(ctx, auth.CreateKeyParams{TenantID: tenantID, Name: "tok", Scopes: []string{"admin", "ingest", "query"}})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	verifier := auth.NewAPIKeyVerifier(auth.FromPool(pool))
	limiter := ratelimit.New(nil)
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	rl := &ratelimit.Middleware{Limiter: limiter, Limit: ratelimit.LimitFromSettings(settingsSvc, 1000), Burst: 1000, TenantBurst: 1000}

	docSvc := documents.NewService(resolver, documents.NewTenantStore(), documents.JobsFromPool(pool))
	docSvc.Storage = &memStorage{}
	dh := documents.NewHandlers(docSvc)
	deps := api.Deps{
		Log:                obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:            obs.NewMetrics(),
		RequireScopeQuery:  verifier.RequireScope(auth.ScopeQuery),
		RequireScopeIngest: verifier.RequireScope(auth.ScopeIngest),
		RequireScopeAdmin:  verifier.RequireScope(auth.ScopeAdmin),
		RateLimit:          rl.Handler,
		DocumentList:       http.HandlerFunc(dh.List),
		DocumentGet:        http.HandlerFunc(dh.Get),
		DocumentDelete:     http.HandlerFunc(dh.Delete),
		DocumentChunks:     http.HandlerFunc(dh.Chunks),
		DocumentIngest:     http.HandlerFunc(dh.Ingest),
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

	// --- Unauthenticated is refused. ---
	{
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/documents", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("anon list: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anon list = %d, want 401", resp.StatusCode)
		}
	}

	// --- List returns the {items,next_cursor} envelope containing the doc. ---
	code, body := call(http.MethodGet, "/v1/documents?status=active&q="+suffix, "", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body=%s", code, body)
	}
	var page documents.Page
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ExternalID != docExternal {
		t.Fatalf("list did not return the seeded document: %+v", page.Items)
	}
	docID := page.Items[0].ID

	// --- Filter by a different source returns nothing (tenant-scoped, real SQL). ---
	if c, b := call(http.MethodGet, "/v1/documents?source=00000000-0000-0000-0000-000000000000", "", nil); c == http.StatusOK {
		var p documents.Page
		_ = json.Unmarshal(b, &p)
		if len(p.Items) != 0 {
			t.Fatalf("source filter leaked %d docs", len(p.Items))
		}
	} else {
		t.Fatalf("source filter = %d, want 200", c)
	}

	// --- Get: current version metadata, no content by default. ---
	code, body = call(http.MethodGet, "/v1/documents/"+docID, "", nil)
	if code != http.StatusOK {
		t.Fatalf("get = %d, want 200; body=%s", code, body)
	}
	var detail documents.DocumentDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if detail.CurrentVersion == nil || detail.CurrentVersion.CharCount != len(content) {
		t.Fatalf("get missing/wrong current version meta: %+v", detail.CurrentVersion)
	}
	if detail.CurrentVersion.Content != nil {
		t.Fatal("get returned content without ?content=true")
	}

	// --- Get with ?content=true includes the full text. ---
	code, body = call(http.MethodGet, "/v1/documents/"+docID+"?content=true", "", nil)
	if code != http.StatusOK {
		t.Fatalf("get content = %d, want 200", code)
	}
	_ = json.Unmarshal(body, &detail)
	if detail.CurrentVersion == nil || detail.CurrentVersion.Content == nil || *detail.CurrentVersion.Content != content {
		t.Fatalf("get ?content=true did not return the content: %+v", detail.CurrentVersion)
	}

	// --- Unknown id is 404. ---
	if c, _ := call(http.MethodGet, "/v1/documents/11111111-1111-1111-1111-111111111111", "", nil); c != http.StatusNotFound {
		t.Fatalf("get missing = %d, want 404", c)
	}

	// --- Chunks debug endpoint returns the current-version chunk, no embedding. ---
	code, body = call(http.MethodGet, "/v1/documents/"+docID+"/chunks", "", nil)
	if code != http.StatusOK {
		t.Fatalf("chunks = %d, want 200; body=%s", code, body)
	}
	var chunks documents.ChunkPage
	if err := json.Unmarshal(body, &chunks); err != nil {
		t.Fatalf("decode chunks: %v", err)
	}
	if len(chunks.Items) != 1 || chunks.Items[0].Content != content {
		t.Fatalf("chunks did not return the seeded chunk: %+v", chunks.Items)
	}
	// The opaque embedding vector must never be returned (only embedding_model is).
	if strings.Contains(string(body), `"embedding":`) {
		t.Fatalf("chunks response leaks the embedding vector: %s", body)
	}

	// --- POST multipart upload enqueues a real ingest_document job (202). ---
	form, ct := uploadForm(t, "readme.md", "# hi "+suffix, sourceID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/documents", form)
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Idempotency-Key", "ik-"+suffix)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	ingestBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest = %d, want 202; body=%s", resp.StatusCode, ingestBody)
	}
	var ingestOut struct {
		Job documents.Job `json:"job"`
	}
	if err := json.Unmarshal(ingestBody, &ingestOut); err != nil {
		t.Fatalf("decode ingest: %v", err)
	}
	if ingestOut.Job.Kind != "ingest_document" || ingestOut.Job.Status != "queued" {
		t.Fatalf("unexpected ingest job: %+v", ingestOut.Job)
	}
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from jobs where id = '%s' and kind = 'ingest_document' and status = 'queued'", ingestOut.Job.ID)); n != "1" {
		t.Fatalf("ingest job not persisted in jobs table (count=%s)", n)
	}

	// --- Delete soft-deletes the document in the real tenant DB. ---
	if c, b := call(http.MethodDelete, "/v1/documents/"+docID, "", nil); c != http.StatusOK {
		t.Fatalf("delete = %d, want 200; body=%s", c, b)
	}
	got := psqlScalar2(t, dbName, fmt.Sprintf("select status from documents where id = '%s'", docID))
	if got != "deleted" {
		t.Fatalf("document status after delete = %q, want deleted", got)
	}
}

// uploadForm builds a multipart body with one file part and a source field.
func uploadForm(t *testing.T, filename, content, sourceID string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	_, _ = fw.Write([]byte(content))
	_ = mw.WriteField("source", sourceID)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}
