package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/cp/jobs"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/embedcache"
	"github.com/rag-platform/ragctl/internal/ingest/ingestdoc"
	"github.com/rag-platform/ragctl/internal/ingest/sink"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// reconcileInterval is how often the mirror reconciler runs (SPEC-08 §3): drift
// between River and the jobs mirror is healed within this window even without a
// worker restart.
const reconcileInterval = 60 * time.Second

// Default per-queue worker concurrency (SPEC-08 §1). Separate pools per queue mean a
// long reindex on `maintenance` cannot starve syncs on `ingest`.
const (
	defaultIngestWorkers      = 8
	defaultMaintenanceWorkers = 2
	defaultPlatformWorkers    = 2
)

// QueueConcurrency is the per-queue worker parallelism. A zero field takes its
// default. The three queues are isolated so heavy maintenance work never starves the
// ingest path (SPEC-08 §1).
type QueueConcurrency struct {
	Ingest      int
	Maintenance int
	Platform    int
}

func (q QueueConcurrency) withDefaults() QueueConcurrency {
	if q.Ingest <= 0 {
		q.Ingest = defaultIngestWorkers
	}
	if q.Maintenance <= 0 {
		q.Maintenance = defaultMaintenanceWorkers
	}
	if q.Platform <= 0 {
		q.Platform = defaultPlatformWorkers
	}
	return q
}

// CredentialDecrypter opens sealed source credentials for a sync run (SPEC-04 §6,
// C-4). A *crypto.Cipher or a rotation *crypto.Keyring satisfies it.
type CredentialDecrypter interface {
	Decrypt(ciphertext []byte) ([]byte, error)
}

// Deps are the collaborators the worker needs, assembled at the composition root
// (internal/cli). River runs on the CONTROL-PLANE pool (ADR-0005); tenant data is
// reached only per-job through the Resolver (ADR-0003, C-3).
type Deps struct {
	// Pool is the control-plane pool River uses for its queue tables and that the
	// producers enqueue through. It is NEVER a tenant pool (C-3).
	Pool *pgxpool.Pool
	// Resolver is the only path to a tenant.DB (ADR-0003); every job opens one from
	// its tenant_id arg.
	Resolver tenant.Resolver
	// Cipher decrypts source credentials for a sync run (SPEC-04 §6, C-4). It is an
	// interface so a rotation keyring (multi-version, STORY-10.4) drops in for the
	// single-version cipher.
	Cipher CredentialDecrypter

	// Sources / Settings are control-plane registry reads (C-3).
	Sources  SourceStore
	Settings SettingsSource

	// Registry resolves a connector by source kind; StateStores builds the per-source
	// connector state store (seam so the worker needs no change per connector,
	// NFR-MNT-01).
	Registry    *connector.Registry
	StateStores StateStoreFactory

	// DocStore persists tenant content (sink.Store) and runs GC. Local/Sidecar are the
	// parse stages; Fetcher reads uploaded bytes back for ingest_document; Embedder
	// builds the tenant's embedder (fail-closed on providers_allowed).
	DocStore documents.TenantStore
	Local    sink.LocalParser
	Sidecar  sink.SidecarParser
	Fetcher  ingestdoc.Fetcher
	// Embedder builds a tenant's embedder from its settings (fail-closed on
	// providers_allowed). Production passes KeyedEmbedderFactory; it is the interface
	// so an e2e can inject a network-free stub (the embedding provider is external).
	Embedder ingestdoc.EmbedderFactory

	Log         *slog.Logger
	Concurrency QueueConcurrency

	// Metrics records jobs_duration_seconds and jobs_failed_total per kind (SPEC-10
	// §2/§5). Optional: a nil Metrics disables job metrics (the middleware no-ops).
	Metrics *obs.Metrics

	// WorkerID labels this worker process in the jobs mirror (jobs.worker_id,
	// SPEC-08 §3). Empty is filled with a hostname#short-uuid default.
	WorkerID string

	// IngestPerTenantCap is the per-tenant ceiling on concurrent ingest jobs
	// (SPEC-08 §1). Zero takes the default (2).
	IngestPerTenantCap int

	// Queues optionally restricts which queues this client consumes (e.g. run a
	// dedicated ingest-only worker). Empty/nil consumes all three. Unknown names are
	// ignored; if the selection resolves to no known queue, all three are used.
	Queues []string
}

// Worker wraps the River client and the control-plane pool it runs on. It is built
// by New, its River schema applied by Migrate, then Start/Stop drive the worker
// lifecycle (Stop drains in-flight jobs — the graceful-shutdown AC).
type Worker struct {
	client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
	log    *slog.Logger
	recon  jobs.Reconciler
}

// New assembles the River client: it registers a worker per SPEC-08 §1 job kind and
// configures the three queues with independent concurrency. The ingest queue
// (ingest_document + sync_source) is fully wired end-to-end; gc_tenant is wired to
// the retention sweep; the remaining maintenance/platform kinds are registered as
// TODO handlers (they fail loud+permanent) so the queue structure is complete
// without ballooning this story.
func New(deps Deps) (*Worker, error) {
	if deps.Pool == nil {
		return nil, fmt.Errorf("worker: nil control-plane pool")
	}
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}

	workers := river.NewWorkers()

	// ingest_document: the STORY-06.3 handler, opening a tenant.DB per job.
	ingestor := &ingestdoc.Ingestor{
		Resolver: deps.Resolver,
		Storage:  deps.Fetcher,
		Settings: settingsAdapter{deps.Settings},
		Embedder: deps.Embedder,
		Store:    deps.DocStore,
		Local:    deps.Local,
		Sidecar:  deps.Sidecar,
		Cache:    embedcache.NewPgCache(),
		Metrics:  deps.Metrics,
		Now:      time.Now,
	}
	river.AddWorker(workers, &ingestWorker{ingestor: ingestor, log: log})

	// sync_source: resolve connector by kind → decrypt creds → Sync into the sink.
	river.AddWorker(workers, &syncWorker{
		resolver:  deps.Resolver,
		sources:   deps.Sources,
		decrypter: deps.Cipher,
		registry:  deps.Registry,
		settings:  deps.Settings,
		embedder:  deps.Embedder,
		states:    deps.StateStores,
		store:     deps.DocStore,
		local:     deps.Local,
		sidecar:   deps.Sidecar,
		metrics:   deps.Metrics,
		log:       log,
	})

	// gc_tenant: the STORY-05.9 retention sweep.
	river.AddWorker(workers, &gcWorker{resolver: deps.Resolver, store: deps.DocStore, log: log})

	// delete_source: the STORY-09.6 handler, opening a tenant.DB per job.
	river.AddWorker(workers, &deleteSourceWorker{resolver: deps.Resolver, store: deps.DocStore, log: log})

	// Registered-but-TODO kinds (complete the queue structure; fail loud+permanent).
	river.AddWorker(workers, &todoWorker[ReindexTenantArgs]{story: "reindex orchestration over STORY-05.8", log: log})
	river.AddWorker(workers, &todoWorker[ProvisionTenantArgs]{story: "async provisioning over STORY-02.3", log: log})
	river.AddWorker(workers, &todoWorker[DeleteTenantArgs]{story: "async deletion over STORY-02.4", log: log})

	// reconcile_jobs: heals mirror rows left behind by a crashed worker (SPEC-08
	// §3). Runs on the maintenance queue, once at startup and every
	// reconcileInterval thereafter (see the PeriodicJobs entry below and Start).
	recon := jobs.Reconciler{Store: jobs.FromPool(deps.Pool), Log: log}
	river.AddWorker(workers, &reconcileWorker{recon: recon})

	c := deps.Concurrency.withDefaults()
	all := map[string]river.QueueConfig{
		QueueIngest:      {MaxWorkers: c.Ingest},
		QueueMaintenance: {MaxWorkers: c.Maintenance},
		QueuePlatform:    {MaxWorkers: c.Platform},
	}
	queues := selectQueues(all, deps.Queues)
	workerID := deps.WorkerID
	if workerID == "" {
		workerID = newWorkerID()
	}
	// The mirror middleware keeps the control-plane jobs table in step with River
	// state (ADR-0005, ADR-0060): running before each job, one terminal transition
	// after. It runs on the control-plane pool and never fails a job (STORY-09.2).
	mirror := &mirrorMiddleware{store: jobsMirror{pool: deps.Pool}, workerID: workerID, log: log}
	// The tenant limiter is OUTERMOST (registered first) so a per-tenant cap decision —
	// and its snooze — happens before the mirror marks a job running (STORY-09.5).
	limiter := newTenantLimiter(deps.IngestPerTenantCap, log)
	client, err := river.NewClient(riverpgxv5.New(deps.Pool), &river.Config{
		Queues:  queues,
		Workers: workers,
		Logger:  log,
		// Re-queue a job orphaned by a crashed/restarted worker after 35 min, instead
		// of River's 1h default, so a stuck job recovers (and, for sync_source, resumes
		// from its persisted crawl state) sooner. It MUST stay above the longest job
		// Timeout (syncJobTimeout = 30 min): a live long job is ctx-cancelled by its own
		// Timeout at 30 min, so it is gone well before 35 min and never wrongly rescued.
		RescueStuckJobsAfter: 35 * time.Minute,
		// PeriodicJobs schedules reconcile_jobs every reconcileInterval so mirror
		// drift self-heals without a restart (SPEC-08 §3). RunOnStart also fires one
		// on client start; the UniqueOpts guard skips a new insert while one is still
		// available/running so overlapping runs never pile up.
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(reconcileInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return ReconcileJobsArgs{}, &river.InsertOpts{
						Queue: QueueMaintenance,
						UniqueOpts: river.UniqueOpts{
							ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStateRunning},
						},
					}
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
		// Order (outermost first): limiter (may snooze before any work), then the trace
		// span wrapping the job, then the mirror. So a job span (STORY-10.3) parents the
		// mirror writes and the handler's downstream provider/sidecar spans.
		// metricsMiddleware is INNERMOST so jobs_duration_seconds times just the
		// handler (not the limiter snooze or mirror writes) and jobs_failed_total
		// counts the handler's terminal error (SPEC-10 §2/§5).
		WorkerMiddleware: []rivertype.WorkerMiddleware{limiter, logMiddleware{log: log}, traceMiddleware{}, mirror, metricsMiddleware{m: deps.Metrics}},
	})
	if err != nil {
		return nil, fmt.Errorf("worker: build river client: %w", err)
	}

	return &Worker{client: client, pool: deps.Pool, log: log, recon: recon}, nil
}

// Migrate applies River's own schema to the control-plane database. River's tables
// (river_job, river_leader, river_queue, river_migration, ...) are managed by River's
// migrator, entirely separate from the goose-managed control_plane schema and its
// drift guard — see ADR-0059. It is idempotent (River records applied versions).
func (w *Worker) Migrate(ctx context.Context) error {
	m, err := rivermigrate.New(riverpgxv5.New(w.pool), nil)
	if err != nil {
		return fmt.Errorf("worker: build river migrator: %w", err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("worker: apply river migrations: %w", err)
	}
	return nil
}

// Start begins fetching and working jobs across all queues. It returns once the
// client has started; Stop drains it.
func (w *Worker) Start(ctx context.Context) error {
	if err := w.client.Start(ctx); err != nil {
		return fmt.Errorf("worker: start: %w", err)
	}
	// One synchronous reconcile pass right after start so a restart heals mirror
	// rows drifted by a prior crash immediately, without waiting for the first
	// periodic run (SPEC-08 §3). A reconcile error is logged, never fatal: the
	// periodic job (and the next restart) will retry.
	if healed, err := w.recon.Reconcile(ctx); err != nil {
		w.log.Warn("startup reconcile failed", "err", err.Error())
	} else if len(healed) > 0 {
		w.log.Info("startup reconcile healed drifted jobs", "count", len(healed))
	}
	return nil
}

// Stop gracefully stops the client, DRAINING in-flight jobs: it waits for running
// jobs to finish (respecting ctx as the drain deadline) before returning. This is the
// graceful-shutdown-drains AC (SPEC-08 §3). Use StopAndCancel to abandon instead.
func (w *Worker) Stop(ctx context.Context) error {
	if err := w.client.Stop(ctx); err != nil {
		return fmt.Errorf("worker: stop: %w", err)
	}
	return nil
}

// Client exposes the River client so producers (and tests) can enqueue jobs. Per
// ADR-0005 producers enqueue River jobs directly (transactionally); River state is
// authoritative and the control-plane jobs table mirrors it (STORY-09.2).
func (w *Worker) Client() *river.Client[pgx.Tx] { return w.client }

// NewInsertClient builds an INSERT-ONLY River client on the control-plane pool for
// producers running in `ragctl serve` (ADR-0005: transactional enqueue with the row
// that created it). It registers no workers and is never Started — River supports an
// insert-only client initialised by omitting Queues — so it carries none of the
// worker's handler dependencies; it exists only to InsertTx jobs alongside their
// mirror row (STORY-09.2).
func NewInsertClient(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) {
	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: slog.Default()})
	if err != nil {
		return nil, fmt.Errorf("worker: build insert client: %w", err)
	}
	return c, nil
}

// selectQueues restricts all to the requested names. An empty/nil selection, or one
// naming no known queue, returns all queues unchanged (a worker consuming everything
// is the safe default).
func selectQueues(all map[string]river.QueueConfig, want []string) map[string]river.QueueConfig {
	if len(want) == 0 {
		return all
	}
	out := make(map[string]river.QueueConfig, len(want))
	for _, name := range want {
		if cfg, ok := all[name]; ok {
			out[name] = cfg
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

// settingsAdapter bridges the worker's SettingsSource to ingestdoc.SettingsSource
// (identical method set; kept explicit so the two packages stay decoupled).
type settingsAdapter struct{ s SettingsSource }

func (a settingsAdapter) Get(ctx context.Context, tenantID string) (map[string]any, error) {
	return a.s.Get(ctx, tenantID)
}
