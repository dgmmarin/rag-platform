package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/config"
	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/connector/api"
	"github.com/rag-platform/ragctl/internal/connector/webcrawl"
	"github.com/rag-platform/ragctl/internal/cp/sources"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/ingest/ingestdoc"
	"github.com/rag-platform/ragctl/internal/ingest/parse"
	"github.com/rag-platform/ragctl/internal/ingest/sidecar"
	"github.com/rag-platform/ragctl/internal/objectstore"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
	"github.com/rag-platform/ragctl/internal/worker"
)

// workerDrainTimeout bounds how long graceful shutdown waits for in-flight jobs to
// finish draining before the worker is forced down (SPEC-08 §3). Jobs that respect
// ctx exit between documents committing nothing partial (SPEC-05 §5).
const workerDrainTimeout = 30 * time.Second

// workerConfig carries the resolved inputs runWorker needs.
type workerConfig struct {
	Queues             []string
	Concurrency        worker.QueueConcurrency
	IngestPerTenantCap int
	MetricsAddr        string
	Obs                ObsSettings
	Cfg                config.Config
	ControlURL         string
	Cipher             *crypto.Keyring
}

// runWorker builds the River worker on the control-plane database (ADR-0005),
// applies River's own schema, starts working the ingest/maintenance/platform queues,
// and blocks until SIGINT/SIGTERM, then drains in-flight jobs (client.Stop). It
// mirrors runAPIServer: structured logs and tracing go to logw/OTLP, and the whole
// thing is cancellable so the process shuts down gracefully (STORY-09.1).
func runWorker(ctx context.Context, wc workerConfig, logw io.Writer) error {
	log := obs.Logger("ragctl-worker", obs.ParseLevel(wc.Obs.LogLevel), logw)

	shutdownTracing, err := obs.SetupTracing(ctx, obs.TracingConfig{
		Service:      "ragctl-worker",
		OTLPEndpoint: wc.Obs.OTLPEndpoint,
		Insecure:     wc.Obs.OTLPInsecure,
		SamplerRatio: wc.Obs.SamplerRatio,
	})
	if err != nil {
		return fmt.Errorf("work: setup tracing: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), workerDrainTimeout)
		defer cancel()
		_ = shutdownTracing(sctx)
	}()

	if wc.ControlURL == "" {
		return fmt.Errorf("work: no control-plane URL (set --control-plane-url or CONTROL_PLANE_URL)")
	}

	// River runs on the control-plane pool (ADR-0005). NEVER a tenant pool (C-3);
	// tenant data is reached only per-job through the resolver (ADR-0003).
	pool, err := pgxpool.New(ctx, wc.ControlURL)
	if err != nil {
		return fmt.Errorf("work: open control-plane pool: %w", err)
	}
	defer pool.Close()

	// Metrics (SPEC-10 §2): the worker is a service and must expose /metrics too
	// (ADR-0067). The catalogue is shared with the API plane; job metrics are
	// emitted by the metricsMiddleware, queue depth by the sampler below, and
	// ingest/provider metrics by the sink + embedder threaded through the deps.
	metrics := obs.NewMetrics()

	deps, err := buildWorkerDeps(ctx, log, wc, pool, metrics)
	if err != nil {
		return err
	}

	w, err := worker.New(deps)
	if err != nil {
		return err
	}

	if err := w.Migrate(ctx); err != nil {
		return err
	}

	// Cancel on SIGINT/SIGTERM so the worker drains gracefully.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := w.Start(ctx); err != nil {
		return err
	}
	log.Info("worker started", "queues", queueNames(wc.Queues))

	// Prometheus /metrics endpoint for the worker (SPEC-10 §2, ADR-0067). Optional:
	// an empty addr disables it. Serving failure never brings the worker down —
	// metrics are observability, not a job dependency.
	var metricsSrv *http.Server
	if wc.MetricsAddr != "" {
		metricsSrv = &http.Server{Addr: wc.MetricsAddr, Handler: obs.NewServeMux(log, metrics)}
		go func() {
			log.Info("worker metrics serving", "addr", wc.MetricsAddr)
			if serr := metricsSrv.ListenAndServe(); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
				log.Warn("worker metrics endpoint stopped", "err", serr)
			}
		}()
	}

	// jobs_queue_depth sampler (SPEC-10 §2/§5): refresh the per-queue backlog gauge
	// on a ticker. A sample failure is logged, never fatal.
	go sampleQueueDepthLoop(ctx, pool, metrics, log)

	// The leader-elected scheduler enqueues cron syncs and daily GC (SPEC-08 §2). Every
	// replica runs it; a Postgres advisory lock means only one sweeps at a time. It
	// stops when ctx is cancelled; we wait for it before the pool closes.
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		worker.NewScheduler(pool, w.Client(), 0, log).Run(ctx)
	}()

	<-ctx.Done()
	log.Info("worker shutting down; draining in-flight jobs")
	<-schedDone
	sctx, cancel := context.WithTimeout(context.Background(), workerDrainTimeout)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(sctx)
	}
	return w.Stop(sctx)
}

// workerQueueDepthInterval is how often the worker refreshes jobs_queue_depth
// (SPEC-10 §2/§5). A minute is well under the alert's 30-minute window.
const workerQueueDepthInterval = time.Minute

// sampleQueueDepthLoop refreshes the queue-depth gauge until ctx is cancelled. It
// samples once at start (so the gauge is populated before the first tick) and then
// on each tick; a sampling error is logged, never fatal.
func sampleQueueDepthLoop(ctx context.Context, pool *pgxpool.Pool, metrics *obs.Metrics, log *slog.Logger) {
	sample := func() {
		if err := worker.SampleQueueDepths(ctx, pool, metrics); err != nil && ctx.Err() == nil {
			log.Warn("queue-depth sample failed", "err", err)
		}
	}
	sample()
	t := time.NewTicker(workerQueueDepthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sample()
		}
	}
}

// buildWorkerDeps assembles the worker's collaborators from the resolved config: the
// tenant resolver (ADR-0003), control-plane source/settings reads (C-3), the
// connector registry + per-kind state-store factory, the ingestion parse/embed/store
// stages, and object storage for ingest_document read-back.
func buildWorkerDeps(ctx context.Context, log *slog.Logger, wc workerConfig, pool *pgxpool.Pool, metrics *obs.Metrics) (worker.Deps, error) {
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: wc.Cipher})
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))

	var sidecarClient *sidecar.Client
	if wc.Cfg.ParserURL != "" {
		sidecarClient = sidecar.New(wc.Cfg.ParserURL)
	}

	// Object storage for ingest_document byte read-back. Optional: an unset/unreachable
	// store leaves Fetcher nil and ingest_document fails on that seam (reads/sync are
	// unaffected), mirroring the API server's tolerance (STORY-06.3).
	var fetcher ingestdoc.Fetcher
	if wc.Cfg.ObjectStoreEndpoint != "" {
		store, oerr := objectstore.New(ctx, objectstore.Config{
			Endpoint:  wc.Cfg.ObjectStoreEndpoint,
			AccessKey: wc.Cfg.ObjectStoreAccessKey,
			SecretKey: wc.Cfg.ObjectStoreSecretKey,
			Bucket:    wc.Cfg.ObjectStoreBucket,
			Region:    wc.Cfg.ObjectStoreRegion,
		})
		if oerr != nil {
			log.Warn("object storage unavailable; ingest_document jobs will fail on the fetch seam", "err", oerr)
		} else {
			fetcher = store
		}
	}

	return worker.Deps{
		Pool:               pool,
		Resolver:           resolver,
		Cipher:             wc.Cipher,
		Sources:            sourceStoreAdapter{db: sources.FromPool(pool)},
		Settings:           settingsSvc,
		Registry:           connector.DefaultRegistry(),
		StateStores:        stateStoreFactory{},
		DocStore:           documents.NewTenantStore(),
		Local:              parse.Default(),
		Sidecar:            sidecarClient,
		Fetcher:            fetcher,
		Embedder:           worker.KeyedEmbedderFactory{APIKey: wc.Cfg.EmbeddingAPIKey, BaseURL: wc.Cfg.EmbeddingBaseURL, Metrics: metrics},
		Metrics:            metrics,
		Log:                log,
		Concurrency:        wc.Concurrency,
		IngestPerTenantCap: wc.IngestPerTenantCap,
		Queues:             wc.Queues,
	}, nil
}

// sourceStoreAdapter adapts the control-plane sources store to worker.SourceStore:
// it projects a sources.Source down to the (kind, config) the sync worker needs and
// passes credential ciphertext straight through. Sources are control-plane registry
// data (C-3): the underlying store uses the control-plane pool.
type sourceStoreAdapter struct{ db sources.PoolDB }

func (a sourceStoreAdapter) Get(ctx context.Context, tenantID, id string) (worker.Source, error) {
	s, err := a.db.Get(ctx, tenantID, id)
	if err != nil {
		return worker.Source{}, err
	}
	return worker.Source{Kind: s.Kind, Config: s.Config}, nil
}

func (a sourceStoreAdapter) GetCredentials(ctx context.Context, tenantID, id string) ([]byte, error) {
	return a.db.GetCredentials(ctx, tenantID, id)
}

// stateStoreFactory builds the per-source connector state store by kind: the
// resumable crawl_pages store for web_crawl/sitemap, the generic connector_state
// store otherwise. It is the composition-root's connector-specific knowledge, kept
// out of the worker package so adding a connector needs no worker change (NFR-MNT-01).
type stateStoreFactory struct{}

func (stateStoreFactory) For(kind connector.Kind, db *tenant.DB, sourceID uuid.UUID) connector.StateStore {
	switch kind {
	case connector.KindWebCrawl, connector.KindSitemap:
		return webcrawl.NewTenantPageStore(db, sourceID)
	default:
		return api.NewTenantStateStore(db, sourceID)
	}
}

func queueNames(q []string) []string {
	if len(q) == 0 {
		return []string{worker.QueueIngest, worker.QueueMaintenance, worker.QueuePlatform}
	}
	return q
}
