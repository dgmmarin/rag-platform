// Package upload is the "upload" source connector (SPEC-04 §5, FR-SRC-02). Unlike
// the crawl/sitemap/api connectors it is NOT scheduled: documents are pushed one
// at a time through POST /v1/documents, each producing an ingest_document job
// (internal/ingest/ingestdoc), rather than enumerated by a Sync. This connector
// therefore exists mainly to (a) occupy the "upload" kind in the connector
// registry so the sources API's config-validation and "test connection" seams
// resolve it (STORY-06.1), and (b) make explicit, via Sync, that an upload source
// is never swept.
//
// Registration happens in this package's init(), so a blank import
// (`_ ".../internal/connector/upload"`) at the composition root wires it with no
// other change (NFR-MNT-01).
package upload

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/rag-platform/ragctl/internal/connector"
)

// ErrNotScheduled is returned by Sync: an upload source is not enumerated — its
// documents arrive individually via the upload API (SPEC-04 §5). The scheduler
// (EPIC-09) never creates a sync_source job for an upload source; this is the
// belt-and-braces guard if one is ever attempted.
var ErrNotScheduled = errors.New("upload: source is not scheduled; documents are ingested individually")

// configSchema accepts any JSON object: an upload source carries no meaningful
// kind-specific config (there is nothing to crawl or authenticate). Requiring a
// well-formed object still rejects a non-object/malformed body (SPEC-04 §7 step 2).
var configSchema = connector.MustSchemaValidator([]byte(`{"type":"object"}`))

// uploadConnector implements connector.Connector for KindUpload.
type uploadConnector struct{}

// New returns a fresh upload connector.
func New() connector.Connector { return uploadConnector{} }

func (uploadConnector) Kind() connector.Kind { return connector.KindUpload }

// ValidateConfig checks the (empty) config is a well-formed JSON object.
func (uploadConnector) ValidateConfig(cfg json.RawMessage) error {
	if len(cfg) == 0 {
		return nil // an absent config is fine — upload has no required fields
	}
	return configSchema.Validate(cfg)
}

// Fields returns no descriptors: an upload source carries no meaningful
// kind-specific config (ValidateConfig above accepts any well-formed object, empty
// included) — there is nothing for the admin UI to render (SPEC-11 §10).
func (uploadConnector) Fields() []connector.FieldSpec { return nil }

// RequiredConfigFields exposes configSchema's own top-level `required` keys (here:
// none) for the reverse drift guard (internal/connector/kinds_test.go, SPEC-11
// §10) — NOT part of connector.Connector, a test-only introspection hook.
func (uploadConnector) RequiredConfigFields() []string { return configSchema.Required() }

// Test is a no-op success (FR-SRC-14, STORY-07.8). Unlike the web_crawl/sitemap/api
// connectors — whose Test now probes a live external system for reachability and
// credentials — an upload source has NO external system and NO credentials to verify:
// its documents are pushed in through POST /v1/documents, not pulled from anywhere.
// Object-storage health is a platform-wide readiness concern (the /readyz probe),
// deliberately NOT a per-source test — a transient storage outage should not make
// every upload source report itself "broken", and one tenant's test must not probe
// shared infrastructure. So "test connection" for an upload source trivially passes.
func (uploadConnector) Test(_ context.Context, _ json.RawMessage, _ connector.Credentials) error {
	return nil
}

// Sync is never valid for an upload source (SPEC-04 §5); it fails loudly.
func (uploadConnector) Sync(_ context.Context, _ connector.SyncRun, _ connector.Sink) (connector.Stats, error) {
	return connector.Stats{}, ErrNotScheduled
}

func init() {
	connector.Register(connector.KindUpload, New)
}
