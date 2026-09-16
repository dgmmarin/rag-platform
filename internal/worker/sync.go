package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"golang.org/x/time/rate"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/ingest/chunk"
	"github.com/rag-platform/ragctl/internal/ingest/ingestdoc"
	"github.com/rag-platform/ragctl/internal/ingest/sink"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// defaultCrawlRate is the global per-run politeness rate the worker hands every
// connector (SPEC-04 §2). Connectors also apply their own per-host gate on top.
//
// ponytail: a single conservative default (2 req/s, burst 4). Upgrade path — read a
// per-source rate from the source config when a connector exposes one.
var (
	defaultCrawlRate  = rate.Limit(2)
	defaultCrawlBurst = 4
)

// SourceStore is the control-plane source lookup the sync worker needs: the source
// row (kind, config, tenant) and its encrypted credentials. sources.PoolDB
// satisfies it. Sources are control-plane registry data (C-3), read over the
// control-plane pool — never a tenant pool.
type SourceStore interface {
	Get(ctx context.Context, tenantID, id string) (Source, error)
	GetCredentials(ctx context.Context, tenantID, id string) ([]byte, error)
}

// Source is the subset of a control-plane sources row the sync worker uses. It is
// declared here (not imported) so the worker package does not depend on the
// control-plane sources package's full surface; sources.Source satisfies the shape
// structurally via a thin adapter at the composition root.
type Source struct {
	Kind   string
	Config json.RawMessage
}

// SettingsSource returns a tenant's resolved settings document (SPEC-02 §5).
// *tenants.SettingsService satisfies it structurally.
type SettingsSource interface {
	Get(ctx context.Context, tenantID string) (map[string]any, error)
}

// StateStoreFactory builds the per-source connector StateStore (cursor/ETag/crawl
// pages, SPEC-04 §1) for a source kind against a resolved tenant.DB. The concrete
// factory lives at the composition root because the store implementation is
// connector-specific (webcrawl's crawl_pages vs the generic connector_state table);
// keeping it a seam means the worker package needs no change when a connector is
// added (NFR-MNT-01). A nil return is valid — a connector then falls back to its
// own non-resumable in-memory state.
type StateStoreFactory interface {
	For(kind connector.Kind, db *tenant.DB, sourceID uuid.UUID) connector.StateStore
}

// syncWorker runs one sync_source job (SPEC-08 §1). It opens a fresh tenant.DB per
// job (ADR-0003), resolves the connector by the source's kind, decrypts the source
// credentials for the run only (SPEC-04 §6), assembles a connector.SyncRun (the
// per-source state store + a politeness limiter; the connector's own egress guard
// applies), and runs Sync — pushing every enumerated document through the
// connector→ingestion-sink bridge (parse → chunk → embed → commit, SPEC-05). The
// connector calls sink.Complete itself, so a full sync's soft-delete of unseen
// documents runs inside Sync.
type syncWorker struct {
	river.WorkerDefaults[SyncSourceArgs]

	resolver  tenant.Resolver
	sources   SourceStore
	decrypter CredentialDecrypter
	registry  *connector.Registry
	settings  SettingsSource
	embedder  ingestdoc.EmbedderFactory
	states    StateStoreFactory
	store     sink.Store
	local     sink.LocalParser
	sidecar   sink.SidecarParser
	metrics   *obs.Metrics
	log       *slog.Logger
}

// NextRetry overrides River's default policy with the SPEC-08 §1 sync backoff
// schedule (1m / 5m / 30m). Attempt is 1-based; the schedule is indexed by the
// number of failures so far. Past the schedule it falls back to the last step.
func (w *syncWorker) NextRetry(job *river.Job[SyncSourceArgs]) time.Time {
	i := job.Attempt - 1
	if i < 0 {
		i = 0
	}
	if i >= len(syncBackoff) {
		i = len(syncBackoff) - 1
	}
	return time.Now().Add(syncBackoff[i])
}

// syncJobTimeout caps one sync_source attempt. A sync resolves a connector and
// crawls → parses → chunks → embeds every enumerated document at a politeness rate
// (defaultCrawlRate), so River's 1-minute JobTimeoutDefault is far too short — it
// cancels the run mid-crawl with "context deadline exceeded" and the job retries
// forever (ISSUE-0062). 30 minutes covers a typical crawl and stays under the
// client's 1h RescueStuckJobsAfter net, so a genuinely wedged sync is still rescued.
//
// ponytail: one generous fixed cap. Upgrade path — derive the budget from the
// source's max_pages / rate when a large crawl legitimately needs longer.
const syncJobTimeout = 30 * time.Minute

// Timeout overrides River's 1-minute default for the long-running crawl/ingest job.
func (w *syncWorker) Timeout(*river.Job[SyncSourceArgs]) time.Duration { return syncJobTimeout }

// Work resolves, enumerates and ingests one source.
func (w *syncWorker) Work(ctx context.Context, job *river.Job[SyncSourceArgs]) error {
	a := job.Args

	tid, err := uuid.Parse(a.TenantID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("sync_source: bad tenant id %q: %w", a.TenantID, err))
	}
	sid, err := uuid.Parse(a.SourceID)
	if err != nil {
		return river.JobCancel(fmt.Errorf("sync_source: bad source id %q: %w", a.SourceID, err))
	}

	db, err := w.resolver.Open(ctx, tenant.ID(tid))
	if err != nil {
		return fmt.Errorf("sync_source: open tenant: %w", err)
	}

	src, err := w.sources.Get(ctx, a.TenantID, a.SourceID)
	if err != nil {
		return fmt.Errorf("sync_source: load source: %w", err)
	}

	conn, ok := w.registry.Lookup(connector.Kind(src.Kind))
	if !ok {
		// A source whose connector is not built cannot succeed on retry.
		return river.JobCancel(fmt.Errorf("sync_source: no connector for kind %q", src.Kind))
	}

	creds, zero, err := w.credentials(ctx, a.TenantID, a.SourceID)
	if err != nil {
		return fmt.Errorf("sync_source: credentials: %w", err)
	}
	defer zero()

	rawSettings, err := w.settings.Get(ctx, a.TenantID)
	if err != nil {
		return fmt.Errorf("sync_source: load settings: %w", err)
	}
	s := parseTenantSettings(rawSettings)

	emb, err := w.embedder.Embedder(ctx, s)
	if err != nil {
		return fmt.Errorf("sync_source: build embedder: %w", err)
	}

	mode := sink.Incremental
	if a.Full {
		mode = sink.Full
	}
	isink := sink.New(sink.Config{
		DB:       db,
		Store:    w.store,
		Local:    w.local,
		Sidecar:  w.sidecar,
		Embedder: emb,
		SourceID: a.SourceID,
		Mode:     mode,
		Chunk:    chunk.Config{TargetTokens: s.ChunkTarget, OverlapTokens: s.ChunkOverlap},
		Model:    s.EmbeddingModel,
		// SPEC-10 §2 labels: tenant id, the source's kind, the embedding provider.
		Metrics:    w.metrics,
		Tenant:     a.TenantID,
		SourceKind: src.Kind,
		Provider:   s.EmbeddingProvider,
		Now:        time.Now,
	})

	var state connector.StateStore
	if w.states != nil {
		state = w.states.For(connector.Kind(src.Kind), db, sid)
	}

	run := connector.SyncRun{
		SourceID: sid,
		Config:   src.Config,
		Creds:    connector.Credentials(creds),
		State:    state,
		Full:     a.Full,
		Limiter:  rate.NewLimiter(defaultCrawlRate, defaultCrawlBurst),
		Log:      w.log,
	}

	// The connector enumerates into the bridge and calls sink.Complete itself.
	stats, err := conn.Sync(ctx, run, newConnectorSink(isink))
	if err != nil {
		return err
	}
	// Report the SPEC-05 §6 stats to the mirror so they land in jobs.stats on success
	// (SPEC-08 §3). The sink.Stats JSON tags already match the jobs.stats shape.
	if b, mErr := json.Marshal(stats); mErr == nil {
		Stats(ctx).Set(b)
	}
	w.log.Info("sync_source done",
		"tenant_id", a.TenantID, "source_id", a.SourceID, "kind", src.Kind, "full", a.Full,
		"docs_seen", stats.DocsSeen, "docs_changed", stats.DocsChanged, "docs_deleted", stats.DocsDeleted)
	return nil
}

// credentials decrypts a source's sealed credentials for the run only, returning a
// zero() the caller must defer to clear the plaintext (SPEC-04 §6, C-4). A source
// with no stored credentials yields a nil map and a no-op zero().
func (w *syncWorker) credentials(ctx context.Context, tenantID, sourceID string) (map[string]string, func(), error) {
	noop := func() {}
	enc, err := w.sources.GetCredentials(ctx, tenantID, sourceID)
	if err != nil {
		return nil, noop, err
	}
	if len(enc) == 0 {
		return nil, noop, nil
	}
	if w.decrypter == nil {
		return nil, noop, fmt.Errorf("no decrypter configured")
	}
	plain, err := w.decrypter.Decrypt(enc)
	if err != nil {
		return nil, noop, err
	}
	defer crypto.Zero(plain)
	var creds map[string]string
	if err := json.Unmarshal(plain, &creds); err != nil {
		return nil, noop, fmt.Errorf("decode credentials: %w", err)
	}
	zero := func() {
		for k := range creds {
			creds[k] = ""
			delete(creds, k)
		}
	}
	return creds, zero, nil
}
