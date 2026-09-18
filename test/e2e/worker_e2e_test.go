//go:build e2e

// STORY-09.1 golden path: the River worker (internal/worker) CONSUMING real jobs
// off the control-plane Postgres queue (ADR-0005) against the real local stack.
// River is Postgres-backed, so this runs against the real :5432 control plane; the
// external effects a job would reach — the embedding provider and object storage —
// are stubbed (no network), exactly as the sink e2e stubs the embedder. What is
// exercised for real is:
//   - job args carry tenant_id and the worker opens the RIGHT tenant.DB per job
//     (proven by the document landing in the enrolled tenant's own database),
//   - the ingest queue runs ingest_document end-to-end (River → tenant.DB → the
//     STORY-05 parse→chunk→embed→commit pipeline),
//   - graceful shutdown DRAINS: Stop blocks until an in-flight job finishes and the
//     job ends 'completed', never abandoned mid-flight.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/ingestdoc"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
	"github.com/rag-platform/ragctl/internal/worker"
)

// stubFactory is a network-free ingestdoc.EmbedderFactory returning a deterministic
// embedder of the tenant's provisioned dimension.
type stubFactory struct{ dim, tokens int }

func (f stubFactory) Embedder(_ context.Context, _ ingestdoc.Settings) (embed.Embedder, error) {
	return stubEmbedder{dim: f.dim, tokensPerText: f.tokens}, nil
}

// stubFetcher serves uploaded bytes back from an in-memory map (standing in for
// MinIO/S3). An optional gate blocks Get until released, so a test can hold a job
// in-flight to prove graceful drain.
type stubFetcher struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	started chan struct{} // closed on the first Get
	release chan struct{} // Get blocks until this is closed (nil = never blocks)
	once    sync.Once
}

func (f *stubFetcher) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	data, ok := f.blobs[key]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("stub fetcher: no object %q", key)
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

// workerFixture enrols a tenant, seeds an upload source and builds a River worker
// wired with stubbed external effects but a REAL resolver/tenant.DB and the real
// control-plane pool. It returns the worker, the enrolled tenant/source ids, the
// resolver and the fetcher the caller loads with bytes.
type workerFixture struct {
	w        *worker.Worker
	tenantID string
	sourceID string
	resolver tenant.Resolver
	fetch    *stubFetcher
}

func newWorkerFixture(ctx context.Context, t *testing.T, fetch *stubFetcher) workerFixture {
	t.Helper()
	migrateControl(t)
	_, _ = writeWrappedDEK(t)
	pool := controlPool(t)

	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	slug := "wrk-" + suffix
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

	ageKey, blob := writeWrappedDEK(t)
	const dim = 1024 // matches the default settings embedding.dim (voyage-3)
	if out, exit := runEnroll(t, ageKey, blob, slug, "Worker "+suffix, dim); exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}

	var tenantID string
	if err := pool.QueryRow(ctx, `select id::text from tenants where slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant id: %v", err)
	}
	var sourceID string
	if err := pool.QueryRow(ctx,
		`insert into sources (tenant_id, kind, name, status) values ($1, 'upload', 'uploads', 'active') returning id::text`,
		tenantID).Scan(&sourceID); err != nil {
		t.Fatalf("seed upload source: %v", err)
	}

	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher, CacheTTL: 50 * time.Millisecond})

	w, err := worker.New(worker.Deps{
		Pool:        pool,
		Resolver:    resolver,
		Cipher:      cipher,
		Settings:    tenants.NewSettingsService(tenants.SettingsFromPool(pool)),
		DocStore:    documents.NewTenantStore(),
		Local:       parse.Default(),
		Fetcher:     fetch,
		Embedder:    stubFactory{dim: dim, tokens: 7},
		Log:         obs.Logger("worker-e2e", obs.ParseLevel("info"), io.Discard),
		Concurrency: worker.QueueConcurrency{Ingest: 2, Maintenance: 1, Platform: 1},
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	if err := w.Migrate(ctx); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	return workerFixture{w: w, tenantID: tenantID, sourceID: sourceID, resolver: resolver, fetch: fetch}
}

// TestWorkerIngestsDocumentEndToEnd proves the ingest queue: an enqueued
// ingest_document River job is consumed on real Postgres, the worker opens the
// enrolled tenant's own DB from the job's tenant_id arg, and the document commits
// visibly into that tenant's live_chunks.
func TestWorkerIngestsDocumentEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fetch := &stubFetcher{blobs: map[string][]byte{"uploads/a.md": mdBytes("Alpha", "Alpha body one.")}}
	fx := newWorkerFixture(ctx, t, fetch)

	if _, err := fx.w.Client().Insert(ctx, worker.IngestDocumentArgs{
		TenantID:   fx.tenantID,
		SourceID:   fx.sourceID,
		ObjectKey:  "uploads/a.md",
		ExternalID: "a.md",
		Filename:   "a.md",
		MimeType:   "text/markdown",
	}, nil); err != nil {
		t.Fatalf("insert ingest_document: %v", err)
	}

	if err := fx.w.Start(ctx); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	t.Cleanup(func() {
		sctx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_ = fx.w.Stop(sctx)
	})

	db, err := fx.resolver.Open(ctx, tenant.ID(uuid.MustParse(fx.tenantID)))
	if err != nil {
		t.Fatalf("resolver.Open: %v", err)
	}

	// Poll the tenant DB until the worker has committed the document into live_chunks.
	deadline := time.Now().Add(60 * time.Second)
	for {
		got := tenantScalarDB(ctx, t, db,
			`select count(*) from live_chunks lc join documents d on d.id = lc.document_id where d.external_id = $1`, "a.md")
		if got != "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ingest_document was not consumed into the tenant DB within the deadline")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from documents where source_id = $1::uuid and status = 'active'`, fx.sourceID); got != "1" {
		t.Fatalf("active documents = %s, want 1", got)
	}
}

// TestWorkerMirrorsJobStatusToJobsTable proves STORY-09.2: a producer enqueues the
// River job and its jobs mirror row in ONE transaction (linked by river_job_id,
// exactly as the documents producer does), and as the worker runs the job the mirror
// middleware drives that row queued -> running -> succeeded with the worker id,
// timings and the SPEC-05 §6 stats — the admin-facing view (SPEC-08 §3), reading the
// jobs table, never River internals.
func TestWorkerMirrorsJobStatusToJobsTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fetch := &stubFetcher{blobs: map[string][]byte{"uploads/c.md": mdBytes("Charlie", "Charlie body one two three.")}}
	fx := newWorkerFixture(ctx, t, fetch)
	pool := controlPool(t)

	// Producer half (ADR-0005): enqueue the River job and its mirror row atomically.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := fx.w.Client().InsertTx(ctx, tx, worker.IngestDocumentArgs{
		TenantID: fx.tenantID, SourceID: fx.sourceID, ObjectKey: "uploads/c.md",
		ExternalID: "c.md", Filename: "c.md", MimeType: "text/markdown",
	}, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert river job: %v", err)
	}
	var jobID string
	if err := tx.QueryRow(ctx, `
		insert into jobs (tenant_id, source_id, kind, status, payload, river_job_id)
		values ($1::uuid, $2::uuid, 'ingest_document', 'queued', '{}', $3)
		returning id::text`, fx.tenantID, fx.sourceID, res.Job.ID).Scan(&jobID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert jobs row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := fx.w.Start(ctx); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	t.Cleanup(func() {
		sctx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_ = fx.w.Stop(sctx)
	})

	// Poll the mirror row until the worker drives it to succeeded.
	deadline := time.Now().Add(60 * time.Second)
	var status, workerID string
	var stats []byte
	var started, finished *time.Time
	for {
		if err := pool.QueryRow(ctx, `
			select status::text, coalesce(worker_id, ''), stats, started_at, finished_at
			  from jobs where id = $1::uuid`, jobID).Scan(&status, &workerID, &stats, &started, &finished); err != nil {
			t.Fatalf("read jobs row: %v", err)
		}
		if status == "succeeded" {
			break
		}
		if status == "failed" {
			t.Fatalf("job mirrored as failed; stats=%s", stats)
		}
		if time.Now().After(deadline) {
			t.Fatalf("mirror row did not reach succeeded (last status %q)", status)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if workerID == "" {
		t.Fatal("worker_id was not mirrored")
	}
	if started == nil || finished == nil {
		t.Fatalf("started_at/finished_at not mirrored (started=%v finished=%v)", started, finished)
	}
	var s struct {
		ChunksWritten int `json:"chunks_written"`
	}
	if err := json.Unmarshal(stats, &s); err != nil {
		t.Fatalf("jobs.stats is not valid JSON: %s", stats)
	}
	if s.ChunksWritten < 1 {
		t.Fatalf("stats not mirrored into jobs.stats (chunks_written=%d): %s", s.ChunksWritten, stats)
	}
}

// TestSchedulerEnqueuesDueCronSync proves STORY-09.3: the leader-elected scheduler
// finds an active cron source whose next_run_at is due, enqueues a sync_source job
// (with its mirror row), advances next_run_at from the cron and bumps the run counter.
// TWO schedulers run concurrently to exercise leader election — the outcome must be
// exactly one enqueue, never a duplicate (SPEC-08 §2).
func TestSchedulerEnqueuesDueCronSync(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fx := newWorkerFixture(ctx, t, &stubFetcher{})
	pool := controlPool(t)

	// Make the seeded source a cron source that is due right now.
	if _, err := pool.Exec(ctx,
		`update sources set schedule_cron = '*/5 * * * *', next_run_at = now() - interval '1 minute',
		     status = 'active', sync_run_count = 0 where id = $1::uuid`, fx.sourceID); err != nil {
		t.Fatalf("arm cron source: %v", err)
	}

	// Two replicas of the scheduler, short interval; the advisory lock (and River
	// uniqueness) must keep them from double-enqueueing.
	logs := &captureWriter{}
	log := obs.Logger("sched-e2e", obs.ParseLevel("info"), logs)
	sctx, scancel := context.WithCancel(ctx)
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			worker.NewScheduler(pool, fx.w.Client(), 80*time.Millisecond, log).Run(sctx)
		}()
	}
	t.Cleanup(func() { scancel(); <-done; <-done })

	// Poll until the sync_source mirror row appears for the source.
	deadline := time.Now().Add(30 * time.Second)
	var count int
	for {
		if err := pool.QueryRow(ctx,
			`select count(*) from jobs where kind = 'sync_source' and source_id = $1::uuid`, fx.sourceID).Scan(&count); err != nil {
			t.Fatalf("count sync jobs: %v", err)
		}
		if count >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduler did not enqueue a sync_source for the due cron source")
		}
		time.Sleep(150 * time.Millisecond)
	}

	// next_run_at advanced into the future and the counter incremented; the first run is
	// a full sync (SPEC-08 §2). Give the concurrent replicas a moment to prove no dup.
	time.Sleep(500 * time.Millisecond)
	if err := pool.QueryRow(ctx,
		`select count(*) from jobs where kind = 'sync_source' and source_id = $1::uuid`, fx.sourceID).Scan(&count); err != nil {
		t.Fatalf("recount sync jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("sync_source jobs for source = %d, want exactly 1 (leader election / uniqueness)", count)
	}
	// ISSUE-0080: the scheduled sync's trace opens with an `enqueued` lifecycle event
	// (not `started`), carrying the source id — so an operator can trace the job from
	// its start. (gc_tenant also enqueues here; its event has an empty source_id.)
	var enqueued bool
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"event":"enqueued"`) &&
			strings.Contains(line, `"kind":"sync_source"`) &&
			strings.Contains(line, `"source_id":"`+fx.sourceID+`"`) {
			enqueued = true
			break
		}
	}
	if !enqueued {
		t.Fatalf("no sync_source \"enqueued\" event for source %q:\n%s", fx.sourceID, logs.String())
	}
	var future bool
	var runCount int
	if err := pool.QueryRow(ctx,
		`select next_run_at > now(), sync_run_count from sources where id = $1::uuid`, fx.sourceID).Scan(&future, &runCount); err != nil {
		t.Fatalf("read source schedule: %v", err)
	}
	if !future {
		t.Fatal("next_run_at was not advanced into the future")
	}
	if runCount != 1 {
		t.Fatalf("sync_run_count = %d, want 1", runCount)
	}
	var full bool
	if err := pool.QueryRow(ctx,
		`select (payload->>'full')::bool from jobs where kind = 'sync_source' and source_id = $1::uuid`, fx.sourceID).Scan(&full); err != nil {
		t.Fatalf("read job payload: %v", err)
	}
	if !full {
		t.Fatal("first scheduled run should be a full sync")
	}
}

// TestCancelRunningJobStopsAndMirrorsCancelled proves STORY-09.4: a RUNNING job that
// is cancelled through River stops (its work context is cancelled, so the handler
// exits between documents committing nothing partial) and the mirror middleware records
// the cancelled terminal — never succeeded (SPEC-08 §4).
func TestCancelRunningJobStopsAndMirrorsCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fetch := &stubFetcher{
		blobs:   map[string][]byte{"uploads/d.md": mdBytes("Delta", "Delta body.")},
		started: make(chan struct{}),
		release: make(chan struct{}), // never released; the cancel unblocks the fetcher via ctx
	}
	fx := newWorkerFixture(ctx, t, fetch)
	pool := controlPool(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := fx.w.Client().InsertTx(ctx, tx, worker.IngestDocumentArgs{
		TenantID: fx.tenantID, SourceID: fx.sourceID, ObjectKey: "uploads/d.md",
		ExternalID: "d.md", Filename: "d.md", MimeType: "text/markdown",
	}, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert river job: %v", err)
	}
	riverID := res.Job.ID
	var jobID string
	if err := tx.QueryRow(ctx, `
		insert into jobs (tenant_id, source_id, kind, status, payload, river_job_id)
		values ($1::uuid, $2::uuid, 'ingest_document', 'queued', '{}', $3)
		returning id::text`, fx.tenantID, fx.sourceID, riverID).Scan(&jobID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert jobs row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := fx.w.Start(ctx); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	t.Cleanup(func() {
		sctx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_ = fx.w.Stop(sctx)
	})

	// Wait until the job is genuinely running (the handler entered the gated fetcher).
	select {
	case <-fetch.started:
	case <-time.After(30 * time.Second):
		t.Fatal("job never started running")
	}

	// Cancel the running job through River; its work context is cancelled, which
	// unblocks the fetcher and stops the handler.
	if _, err := fx.w.Client().JobCancel(ctx, riverID); err != nil {
		t.Fatalf("river cancel: %v", err)
	}

	// The mirror row must reach cancelled — not succeeded.
	deadline := time.Now().Add(30 * time.Second)
	var status string
	for {
		if err := pool.QueryRow(ctx, `select status::text from jobs where id = $1::uuid`, jobID).Scan(&status); err != nil {
			t.Fatalf("read mirror row: %v", err)
		}
		if status == "cancelled" {
			break
		}
		if status == "succeeded" {
			t.Fatal("job succeeded despite being cancelled while running")
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled running job did not mirror cancelled (last status %q)", status)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Nothing partial committed: the document never landed in the tenant DB.
	db, err := fx.resolver.Open(ctx, tenant.ID(uuid.MustParse(fx.tenantID)))
	if err != nil {
		t.Fatalf("resolver.Open: %v", err)
	}
	if got := tenantScalarDB(ctx, t, db,
		`select count(*) from documents where external_id = $1`, "d.md"); got != "0" {
		t.Fatalf("cancelled job committed a document (count=%s), want 0 (nothing partial)", got)
	}
}

// TestDeleteSourceRemovesContentAndReportsStats proves STORY-09.6: a delete_source job
// removes the source's documents, chunks and crawl state from the tenant DB and reports
// the counts in jobs.stats (FR-SRC-12). Content is created through the real ingest path,
// then a delete_source is enqueued and consumed.
func TestDeleteSourceRemovesContentAndReportsStats(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fetch := &stubFetcher{blobs: map[string][]byte{"uploads/e.md": mdBytes("Echo", "Echo body one two three.")}}
	fx := newWorkerFixture(ctx, t, fetch)
	pool := controlPool(t)

	if err := fx.w.Start(ctx); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	t.Cleanup(func() {
		sctx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_ = fx.w.Stop(sctx)
	})

	// 1. Ingest a document for the source (creates a document + chunks).
	if _, err := fx.w.Client().Insert(ctx, worker.IngestDocumentArgs{
		TenantID: fx.tenantID, SourceID: fx.sourceID, ObjectKey: "uploads/e.md",
		ExternalID: "e.md", Filename: "e.md", MimeType: "text/markdown",
	}, nil); err != nil {
		t.Fatalf("insert ingest_document: %v", err)
	}
	db, err := fx.resolver.Open(ctx, tenant.ID(uuid.MustParse(fx.tenantID)))
	if err != nil {
		t.Fatalf("resolver.Open: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if got := tenantScalarDB(ctx, t, db, `select count(*) from chunks where source_id = $1::uuid`, fx.sourceID); got != "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ingest did not create chunks in time")
		}
		time.Sleep(300 * time.Millisecond)
	}
	// Seed crawl state for the source to prove it is removed too.
	if _, err := db.Exec(ctx,
		`insert into crawl_pages (source_id, url, normalized_url) values ($1::uuid, 'http://x/1', 'http://x/1')`, fx.sourceID); err != nil {
		t.Fatalf("seed crawl_page: %v", err)
	}

	// 2. Enqueue delete_source (River job + linked mirror row).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := fx.w.Client().InsertTx(ctx, tx, worker.DeleteSourceArgs{TenantID: fx.tenantID, SourceID: fx.sourceID}, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert delete_source: %v", err)
	}
	var jobID string
	if err := tx.QueryRow(ctx, `
		insert into jobs (tenant_id, source_id, kind, status, payload, river_job_id)
		values ($1::uuid, $2::uuid, 'delete_source', 'queued', '{}', $3)
		returning id::text`, fx.tenantID, fx.sourceID, res.Job.ID).Scan(&jobID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert jobs row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 3. Wait until the delete_source mirror row is succeeded.
	deadline = time.Now().Add(60 * time.Second)
	var status string
	var stats []byte
	for {
		if err := pool.QueryRow(ctx, `select status::text, stats from jobs where id = $1::uuid`, jobID).Scan(&status, &stats); err != nil {
			t.Fatalf("read mirror row: %v", err)
		}
		if status == "succeeded" {
			break
		}
		if status == "failed" {
			t.Fatalf("delete_source failed; stats=%s", stats)
		}
		if time.Now().After(deadline) {
			t.Fatalf("delete_source did not succeed (last status %q)", status)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// 4. Content is gone and the counts were reported.
	for _, tbl := range []string{"documents", "chunks", "crawl_pages"} {
		if got := tenantScalarDB(ctx, t, db,
			"select count(*) from "+tbl+" where source_id = $1::uuid", fx.sourceID); got != "0" {
			t.Fatalf("%s for the source = %s after delete_source, want 0", tbl, got)
		}
	}
	var s struct {
		Documents  int `json:"documents"`
		Chunks     int `json:"chunks"`
		CrawlPages int `json:"crawl_pages"`
	}
	if err := json.Unmarshal(stats, &s); err != nil {
		t.Fatalf("jobs.stats not valid JSON: %s", stats)
	}
	if s.Documents < 1 || s.Chunks < 1 || s.CrawlPages < 1 {
		t.Fatalf("delete_source stats not reported: %s", stats)
	}
}

// TestWorkerGracefulShutdownDrains proves Stop drains in-flight work: a job is held
// running by the gated fetcher, Stop is called and must BLOCK until the job is
// released and finishes, after which the job is 'completed' — not abandoned.
func TestWorkerGracefulShutdownDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fetch := &stubFetcher{
		blobs:   map[string][]byte{"uploads/b.md": mdBytes("Bravo", "Bravo body.")},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	fx := newWorkerFixture(ctx, t, fetch)

	if _, err := fx.w.Client().Insert(ctx, worker.IngestDocumentArgs{
		TenantID:   fx.tenantID,
		SourceID:   fx.sourceID,
		ObjectKey:  "uploads/b.md",
		ExternalID: "b.md",
		Filename:   "b.md",
		MimeType:   "text/markdown",
	}, nil); err != nil {
		t.Fatalf("insert ingest_document: %v", err)
	}
	if err := fx.w.Start(ctx); err != nil {
		t.Fatalf("worker start: %v", err)
	}

	// Wait until the job is genuinely in-flight (the handler entered the fetcher).
	select {
	case <-fetch.started:
	case <-time.After(30 * time.Second):
		t.Fatal("job never started running")
	}

	// Stop while the job is in-flight; it must block until the job drains.
	stopDone := make(chan error, 1)
	go func() {
		sctx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		stopDone <- fx.w.Stop(sctx)
	}()

	// Stop must NOT have returned while the job is still running.
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned %v before the in-flight job was released — not draining", err)
	case <-time.After(750 * time.Millisecond):
	}

	// Release the job; Stop must now complete cleanly (the job drained).
	close(fetch.release)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop after drain: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Stop did not return after the job was released")
	}

	// The drained job finished successfully, not abandoned mid-flight.
	pool := controlPool(t)
	var state string
	if err := pool.QueryRow(ctx,
		`select state from river_job where kind = 'ingest_document' order by id desc limit 1`).Scan(&state); err != nil {
		t.Fatalf("read river_job state: %v", err)
	}
	if state != "completed" {
		t.Fatalf("drained job state = %q, want completed", state)
	}
}
