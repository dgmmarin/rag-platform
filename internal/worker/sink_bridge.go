package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/ingest/sink"
)

// documentSink is the ingestion-side sink the bridge forwards into. *sink.Sink
// (internal/ingest/sink) satisfies it. Keeping it an interface makes the bridge
// unit-testable with a recorder and keeps the mapping logic away from the sink's
// parse/embed/commit machinery.
type documentSink interface {
	Put(ctx context.Context, doc sink.Document) error
	Complete(ctx context.Context) error
}

// connectorSink bridges the connector-facing connector.Sink (what a connector's
// Sync pushes documents at) to the ingestion sink.Sink (parse → chunk → embed →
// commit, SPEC-05). A connector emits connector.Document (Body io.ReadCloser or
// pre-normalised Text, string Title/URI, map Metadata); the ingestion sink wants
// sink.Document (Data []byte, *string Title/URI, json.RawMessage Metadata), so this
// adapter drains the Body, converts the fields, and forwards a single Put.
//
// It is the "a connector pushes documents at the sink" integration EPIC-09 was
// built for (ADR-0038 / SPEC-04 §1). It owns no SQL and no policy; the sink it
// wraps carries the tenant.DB, mode and embedder.
type connectorSink struct {
	inner documentSink
}

// newConnectorSink wraps an ingestion sink as a connector.Sink.
func newConnectorSink(inner documentSink) *connectorSink { return &connectorSink{inner: inner} }

// Put maps one connector.Document into a sink.Document and forwards it.
//
// The returned `changed` is always true: the authoritative changed/unchanged
// bookkeeping lives in sink.Stats (a hash short-circuit inside the sink), which the
// worker persists via the STORY-09.2 mirror. A connector uses `changed` only for
// its own crawl bookkeeping, and reporting "offered" (true) never causes it to skip
// a document.
//
// ponytail: approximate changed=true. Upgrade path — thread the sink.Stats delta
// (DocsChanged before/after the Put) back through here when a connector needs an
// exact per-document changed signal.
func (b *connectorSink) Put(ctx context.Context, doc connector.Document) (bool, error) {
	data, err := bodyBytes(doc)
	if err != nil {
		return false, err
	}
	if err := b.inner.Put(ctx, sink.Document{
		ExternalID: doc.ExternalID,
		Filename:   doc.ExternalID, // advisory (sidecar multipart name); external id is the best available label
		MimeType:   doc.MimeType,
		Data:       data,
		Title:      strPtrOrNil(doc.Title),
		URI:        strPtrOrNil(doc.URI),
		Metadata:   metadataJSON(doc.Metadata),
		RawJSON:    doc.RawJSON,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// Complete forwards to the ingestion sink so a FULL sync's soft-delete of unseen
// documents runs (SPEC-05 §5); on an incremental sync the sink's Complete is a no-op.
func (b *connectorSink) Complete(ctx context.Context) error { return b.inner.Complete(ctx) }

// bodyBytes drains the connector document's raw Body, or falls back to its
// pre-normalised Text when no Body is set (the API connector emits Text).
func bodyBytes(doc connector.Document) ([]byte, error) {
	if doc.Body == nil {
		return []byte(doc.Text), nil
	}
	defer func() { _ = doc.Body.Close() }()
	data, err := io.ReadAll(doc.Body)
	if err != nil {
		return nil, fmt.Errorf("worker: read connector body for %q: %w", doc.ExternalID, err)
	}
	return data, nil
}

// metadataJSON renders a connector's document metadata map as JSON for the store.
// A nil/empty map yields nil, which the store treats as '{}'.
func metadataJSON(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil // metadata is advisory; never fail a document on it
	}
	return b
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Compile-time guard: the bridge is a connector.Sink and *sink.Sink is a documentSink.
var (
	_ connector.Sink = (*connectorSink)(nil)
	_ documentSink   = (*sink.Sink)(nil)
)
