// Package connector is the ingestion connector framework (SPEC-04 §1, FR-SRC-13,
// NFR-MNT-01). It defines the single interface every source kind implements, a
// registry keyed by kind, and a JSON-Schema config-validation helper, so that
// adding a new connector requires no change outside its own package and its
// Register call (NFR-MNT-01).
//
// This is STORY-06.1: the interface, the registry and config validation only.
// The concrete connectors (upload, web_crawl, sitemap, api) live in later stories
// (STORY-06.3, EPIC-07) and register themselves into the default registry from an
// init(); nothing in this package reaches a database, object storage or the
// network. Credential decryption/handling (SPEC-04 §6) is STORY-06.2: Connector.Test
// takes a Credentials argument, but the control-plane seam wired here passes none
// yet (see NewSourcesValidator).
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// Kind identifies a source connector. The set mirrors the control-plane
// `source_kind` enum (schemas/control_plane.sql) and SPEC-04 §1; adding a kind
// means a control-plane migration and a new constant here plus its package.
type Kind string

const (
	KindUpload   Kind = "upload"
	KindWebCrawl Kind = "web_crawl"
	KindSitemap  Kind = "sitemap"
	KindAPI      Kind = "api"
	KindS3       Kind = "s3"
)

// ErrUnsupportedKind is returned by registry helpers when no connector is
// registered for a kind (e.g. a valid enum kind whose connector is not yet built).
var ErrUnsupportedKind = errors.New("connector: no connector registered for kind")

// Credentials is a connector's decrypted secret material for the duration of a
// sync or a test — an API token, basic-auth pair, etc. (SPEC-04 §6). It is never
// logged and is zeroed after use; the encryption/decryption lifecycle is
// STORY-06.2. A nil/empty map means "no credentials".
type Credentials map[string]string

// Document is one unit a connector emits into a Sink during Sync (SPEC-04 §1). A
// connector sets either Body (raw bytes for the parse pipeline) or Text (already
// normalised, e.g. the API connector). It is the connector-facing shape; the
// ingestion sink (internal/ingest/sink, STORY-05.6) is a separate implementation
// bridged by the worker (EPIC-09).
type Document struct {
	ExternalID string          // stable per source; (source, ExternalID) is identity
	Title      string          // document title, if known
	URI        string          // canonical URI for citations
	MimeType   string          // canonical MIME of Body
	Body       io.ReadCloser   // raw bytes; nil if Text is set
	Text       string          // already-normalised text (API connector)
	RawJSON    json.RawMessage // original record, optional
	Metadata   map[string]any  // document-level metadata
	ModifiedAt *time.Time      // source modification time, if known
}

// Sink receives documents a connector enumerates during Sync (SPEC-04 §1). It is
// an interface so a connector is testable with a recording sink and so the real
// ingestion pipeline is wired in by the worker (EPIC-09).
type Sink interface {
	// Put ingests one document; changed reports whether it was new or modified.
	Put(ctx context.Context, doc Document) (changed bool, err error)
	// Complete is called once after a full enumeration, enabling deletion detection.
	Complete(ctx context.Context) error
}

// StateStore is a connector's per-source key/value scratch space in the tenant DB
// (SPEC-04 §1: cursor, ETag cache, last-modified marks). Its concrete backing is
// provided by the worker per run (EPIC-09); this minimal get/set shape realises
// the "per-source key/value" contract and is finalised when Sync lands.
type StateStore interface {
	Get(ctx context.Context, key string) (value string, ok bool, err error)
	Set(ctx context.Context, key, value string) error
}

// Stats is the outcome of one Sync (SPEC-04 §1) — the enumeration counters the
// worker (EPIC-09) folds into jobs.stats/usage_daily. The full jobs.stats shape is
// SPEC-05 §6 (see internal/ingest/sink.Stats, the sink-side accumulator); these are
// the connector-reported counters and are finalised when Sync is implemented.
type Stats struct {
	DocsSeen     int   // documents enumerated
	DocsChanged  int   // documents reported new or changed by the sink
	DocsDeleted  int   // documents detected absent on a full sync
	BytesFetched int64 // raw bytes fetched
}

// SyncRun is the input to Connector.Sync (SPEC-04 §1): everything a connector needs
// for one enumeration of one source. It is assembled by the worker per job.
type SyncRun struct {
	SourceID uuid.UUID       // the source being synced
	Config   json.RawMessage // the source's kind-specific config
	Creds    Credentials     // decrypted credentials for this run (STORY-06.2)
	State    StateStore      // per-source cursor/ETag scratch space
	Full     bool            // true = full enumeration; deletion detection allowed
	Limiter  *rate.Limiter   // per-host politeness limiter
	Log      *slog.Logger    // run-scoped logger (never logs document content)
	// Since is the start of the whole run SERIES (the job's CreatedAt, stable across
	// retries). A resumable connector uses it to tell a retry of THIS run (resume,
	// skip already-fetched work) from a fresh run over prior state (re-fetch). Zero
	// means "treat all persisted work as this run's" (the pre-resume-scoping default).
	Since time.Time
}

// Connector is the common interface every source kind implements (FR-SRC-13,
// SPEC-04 §1). A connector is created fresh per use by a registry factory, so it
// may hold per-sync state. Sync is implemented by the concrete connectors
// (STORY-06.3, EPIC-07); this framework story defines and wires ValidateConfig and
// Test only.
type Connector interface {
	// Kind returns the connector's source kind.
	Kind() Kind
	// ValidateConfig checks a source's kind-specific config, typically against a
	// JSON Schema (SchemaValidator). It returns a *ConfigError for invalid input.
	ValidateConfig(cfg json.RawMessage) error
	// Test validates reachability and credentials ("test connection", FR-SRC-14)
	// without persisting anything. creds is nil until STORY-06.2 wires decryption.
	Test(ctx context.Context, cfg json.RawMessage, creds Credentials) error
	// Sync enumerates the source's content and streams it into the sink. It must be
	// cancellable via ctx. Implemented by the concrete connectors (EPIC-07).
	Sync(ctx context.Context, run SyncRun, sink Sink) (Stats, error)
	// Fields returns the form-field descriptors for this connector's kind-specific
	// config (SPEC-11 §10, STORY-11.2), so the admin UI can render a schema-driven
	// source form (GET /admin/connector-kinds). Each Required:true field MUST be one
	// this connector's ValidateConfig actually rejects when absent — the drift-guard
	// test (internal/connector/kinds_test.go) enforces that a FieldSpec and
	// ValidateConfig never silently drift apart. Never a credential VALUE — only the
	// descriptor (Type:"secret" marks a field whose value the UI must write-only).
	Fields() []FieldSpec
}

// FieldSpec is one form-field descriptor for a connector's kind-specific config
// (SPEC-11 §10). It carries no value — only the shape the admin UI needs to render
// an input: what to call it, what kind of input, and whether the config is invalid
// without it. Name is the JSON key inside the source's `config` document (e.g.
// "start_urls"), except for a credential field (Type:"secret"), whose Name is the
// key the sources API expects in the separate `credentials` map (SPEC-04 §6) —
// never a key inside `config` — since a secret's VALUE never round-trips through
// config or this endpoint.
type FieldSpec struct {
	Name  string
	Label string
	// Type is the input shape the admin UI renders. Scalars: "text" | "url" |
	// "number" | "secret" | "bool". Composite config values: "stringlist" (a JSON
	// array of strings — the UI collects one value per line and submits an array,
	// e.g. start_urls) and "json" (an arbitrary JSON object/array the UI collects
	// as raw JSON and submits parsed, e.g. the api connector's auth/endpoints). A
	// scalar type for an array/object config key makes the UI submit a string the
	// connector's ValidateConfig rejects ("got string, want array").
	Type     string
	Required bool
}
