// Package ingestdoc is the ingest_document job handler (SPEC-04 §5, SPEC-05,
// STORY-06.3). POST /v1/documents writes the raw upload to object storage and
// enqueues an ingest_document job; this package is the directly-callable handler
// that job dispatches to — it fetches the bytes back, runs the STORY-05 pipeline
// (parse → chunk → embed → commit) through the ingestion sink, and atomically
// creates the document + its first version (ADR-0008). A re-upload of the same
// filename produces a NEW version (the sink/store key documents by
// (source_id, external_id) and version by content hash), which is the observable
// half of the AC "re-upload creates a new version".
//
// The document ROW is created HERE, not by the HTTP handler: an active document
// must have a non-null current_version and there is no pending status (SPEC-03 §2
// invariant 1), so the row and its first version are built together in one
// transaction by the sink's store.Put (ADR-0008/0033). This reconciles SPEC-04
// §5's looser "creates a document row" wording with the ADR-0008 invariant — the
// invariant wins (ADR-0042).
//
// The River worker/dispatch loop that will call this per queued job is EPIC-09
// STORY-09.1; this package deliberately builds only the plain handler function.
package ingestdoc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/ingest/chunk"
	"github.com/rag-platform/ragctl/internal/ingest/embed"
	"github.com/rag-platform/ragctl/internal/ingest/embedcache"
	"github.com/rag-platform/ragctl/internal/ingest/sink"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// Fetcher reads an uploaded object back from storage (objectstore.Client). It is
// the read side of the documents Storage seam; the handler needs only Get.
type Fetcher interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// SettingsSource returns a tenant's resolved settings document (SPEC-02 §5).
// *tenants.SettingsService satisfies it structurally (Get(ctx, tenantID)), so this
// package keeps no control-plane import.
type SettingsSource interface {
	Get(ctx context.Context, tenantID string) (map[string]any, error)
}

// EmbedderFactory builds an Embedder for a tenant's settings. The production
// factory (embed.New from the settings provider/model + the platform's provider
// keys) is wired by the worker (EPIC-09); tests inject a deterministic stub, as
// the embedding provider is an external service (matching the sink e2e).
type EmbedderFactory interface {
	Embedder(ctx context.Context, s Settings) (embed.Embedder, error)
}

// Settings is the subset of tenant settings the ingest pipeline needs.
type Settings struct {
	EmbeddingProvider string
	EmbeddingModel    string
	EmbeddingDim      int
	ProvidersAllowed  []string
	ChunkTarget       int
	ChunkOverlap      int
}

// Job is one ingest_document unit: the storage key of the uploaded bytes plus the
// document identity. The worker builds it from the control-plane jobs.payload via
// JobFromPayload.
type Job struct {
	TenantID   string
	SourceID   string
	ObjectKey  string
	ExternalID string // (source_id, external_id) is the document identity = filename
	Filename   string
	MimeType   string
}

// Ingestor runs ingest_document jobs. Its collaborators are injected so the whole
// handler is unit-testable with fakes and the e2e drives it against the real
// stack (resolver + MinIO + tenant DB).
type Ingestor struct {
	Resolver tenant.Resolver    // resolves the tenant DB (ADR-0003); Dispatch only
	Storage  Fetcher            // object storage read-back
	Settings SettingsSource     // tenant settings (chunking, embedding, allowlist)
	Embedder EmbedderFactory    // builds the Embedder from settings
	Store    sink.Store         // documents.TenantStore in production
	Local    sink.LocalParser   // parse.Default()
	Sidecar  sink.SidecarParser // optional; PDF/DOCX/... via the Python sidecar
	// Cache looks up existing embeddings for byte-identical chunk content
	// (chunk-level drift, SPEC-05 §1). Optional: nil disables reuse.
	Cache embedcache.Cache
	// Metrics records ingestion throughput (SPEC-10 §2). Optional: nil is a no-op.
	Metrics *obs.Metrics
	Now     func() time.Time
}

// Dispatch resolves the tenant DB and runs the job. This is the entry point
// EPIC-09's worker calls per queued ingest_document job.
func (in *Ingestor) Dispatch(ctx context.Context, job Job) (sink.Stats, error) {
	id, err := uuid.Parse(job.TenantID)
	if err != nil {
		return sink.Stats{}, fmt.Errorf("ingestdoc: bad tenant id %q: %w", job.TenantID, err)
	}
	db, err := in.Resolver.Open(ctx, tenant.ID(id))
	if err != nil {
		return sink.Stats{}, fmt.Errorf("ingestdoc: open tenant: %w", err)
	}
	return in.Run(ctx, db, job)
}

// Run ingests one uploaded document into the (already resolved) tenant DB. It
// fetches the bytes, loads settings, builds an Embedder and a single-document
// INCREMENTAL sink (an upload is never a full enumeration, so Complete never
// soft-deletes the tenant's other documents), and drives one Sink.Put — which
// creates the document + first version, or a new version on re-upload.
func (in *Ingestor) Run(ctx context.Context, db *tenant.DB, job Job) (sink.Stats, error) {
	if job.ObjectKey == "" || job.SourceID == "" || job.ExternalID == "" {
		return sink.Stats{}, fmt.Errorf("ingestdoc: job missing object_key/source_id/external_id")
	}

	data, err := in.fetch(ctx, job.ObjectKey)
	if err != nil {
		return sink.Stats{}, err
	}

	rawSettings, err := in.Settings.Get(ctx, job.TenantID)
	if err != nil {
		return sink.Stats{}, fmt.Errorf("ingestdoc: load settings: %w", err)
	}
	s := parseSettings(rawSettings)

	emb, err := in.Embedder.Embedder(ctx, s)
	if err != nil {
		return sink.Stats{}, fmt.Errorf("ingestdoc: build embedder: %w", err)
	}

	sk := sink.New(sink.Config{
		DB:       db,
		Store:    in.Store,
		Local:    in.Local,
		Sidecar:  in.Sidecar,
		Embedder: emb,
		Cache:    in.Cache,
		SourceID: job.SourceID,
		Mode:     sink.Incremental,
		Chunk:    chunk.Config{TargetTokens: s.ChunkTarget, OverlapTokens: s.ChunkOverlap},
		Model:    s.EmbeddingModel,
		// SPEC-10 §2 labels: an ingest_document job is always an upload source.
		Metrics:    in.Metrics,
		Tenant:     job.TenantID,
		SourceKind: "upload",
		Provider:   s.EmbeddingProvider,
		Now:        in.Now,
	})

	doc := sink.Document{
		ExternalID: job.ExternalID,
		Filename:   job.Filename,
		MimeType:   job.MimeType,
		Data:       data,
		RawRef:     strptr(job.ObjectKey), // keep the original bytes' storage key
	}
	if err := sk.Put(ctx, doc); err != nil {
		return sk.Stats(), err
	}
	return sk.Stats(), nil
}

// fetch reads the whole object into memory (upload sizes are bounded by the
// upload ceiling, and the parser/sink contract already holds a document in memory).
func (in *Ingestor) fetch(ctx context.Context, key string) ([]byte, error) {
	rc, err := in.Storage.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("ingestdoc: fetch %q: %w", key, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("ingestdoc: read %q: %w", key, err)
	}
	return data, nil
}

// JobFromPayload builds a Job from a queued ingest_document job's tenant id and
// jobs.payload (the shape documents.Service.Ingest enqueues). It is the seam the
// EPIC-09 worker uses. object_key and source_id are required (fail closed).
func JobFromPayload(tenantID string, payload json.RawMessage) (Job, error) {
	var p struct {
		ExternalID string `json:"external_id"`
		Filename   string `json:"filename"`
		MimeType   string `json:"mime_type"`
		ObjectKey  string `json:"object_key"`
		SourceID   string `json:"source_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return Job{}, fmt.Errorf("ingestdoc: decode payload: %w", err)
	}
	if p.ObjectKey == "" {
		return Job{}, fmt.Errorf("ingestdoc: payload has no object_key")
	}
	if p.SourceID == "" {
		return Job{}, fmt.Errorf("ingestdoc: payload has no source_id")
	}
	external := p.ExternalID
	if external == "" {
		external = p.Filename
	}
	return Job{
		TenantID:   tenantID,
		SourceID:   p.SourceID,
		ObjectKey:  p.ObjectKey,
		ExternalID: external,
		Filename:   p.Filename,
		MimeType:   p.MimeType,
	}, nil
}

// parseSettings extracts the pipeline-relevant fields from a settings document
// (SPEC-02 §5). Missing/typed-wrong fields fall back to zero, which the sink and
// chunker treat as their SPEC-05 defaults; the embedding provider/model still
// drive the factory. JSON numbers decode as float64.
func parseSettings(doc map[string]any) Settings {
	var s Settings
	if emb, ok := doc["embedding"].(map[string]any); ok {
		s.EmbeddingProvider, _ = emb["provider"].(string)
		s.EmbeddingModel, _ = emb["model"].(string)
		s.EmbeddingDim = toInt(emb["dim"])
	}
	if ch, ok := doc["chunking"].(map[string]any); ok {
		s.ChunkTarget = toInt(ch["target_tokens"])
		s.ChunkOverlap = toInt(ch["overlap_tokens"])
	}
	if allowed, ok := doc["providers_allowed"].([]any); ok {
		for _, a := range allowed {
			if p, ok := a.(string); ok {
				s.ProvidersAllowed = append(s.ProvidersAllowed, p)
			}
		}
	}
	return s
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func strptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
