package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/ingest/sink"
)

// recordingSink captures what the bridge forwards to the ingestion sink.
type recordingSink struct {
	puts      []sink.Document
	completed bool
	putErr    error
}

func (r *recordingSink) Put(_ context.Context, d sink.Document) error {
	r.puts = append(r.puts, d)
	return r.putErr
}
func (r *recordingSink) Complete(_ context.Context) error {
	r.completed = true
	return nil
}

func strval(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestBridgeMapsBodyDocument: a connector Document carrying raw Body bytes and
// string Title/URI/Metadata must be mapped into the sink Document shape (Data from
// the drained Body, Title/URI as pointers, Metadata as JSON) and forwarded.
func TestBridgeMapsBodyDocument(t *testing.T) {
	rec := &recordingSink{}
	br := newConnectorSink(rec)

	changed, err := br.Put(context.Background(), connector.Document{
		ExternalID: "https://x/a",
		Title:      "Alpha",
		URI:        "https://x/a",
		MimeType:   "text/html",
		Body:       io.NopCloser(strings.NewReader("<h1>Alpha</h1>")),
		Metadata:   map[string]any{"lang": "en"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true (bridge reports the doc as offered)")
	}
	if len(rec.puts) != 1 {
		t.Fatalf("forwarded %d docs, want 1", len(rec.puts))
	}
	got := rec.puts[0]
	if got.ExternalID != "https://x/a" || string(got.Data) != "<h1>Alpha</h1>" || got.MimeType != "text/html" {
		t.Errorf("core fields wrong: %+v data=%q", got, got.Data)
	}
	if strval(got.Title) != "Alpha" || strval(got.URI) != "https://x/a" {
		t.Errorf("title/uri pointers wrong: title=%s uri=%s", strval(got.Title), strval(got.URI))
	}
	var meta map[string]any
	if err := json.Unmarshal(got.Metadata, &meta); err != nil {
		t.Fatalf("metadata not valid JSON: %v (%q)", err, got.Metadata)
	}
	if meta["lang"] != "en" {
		t.Errorf("metadata lost: %v", meta)
	}
}

// TestBridgeMapsTextDocument: an already-normalised Text document (API connector,
// no Body) must have its Data filled from Text.
func TestBridgeMapsTextDocument(t *testing.T) {
	rec := &recordingSink{}
	br := newConnectorSink(rec)

	if _, err := br.Put(context.Background(), connector.Document{
		ExternalID: "rec-1",
		Text:       "already normalised",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if string(rec.puts[0].Data) != "already normalised" {
		t.Errorf("Text not mapped to Data: %q", rec.puts[0].Data)
	}
}

// TestBridgePropagatesPutError: an infrastructure error from the sink (fail the
// job for retry) must surface, not be swallowed.
func TestBridgePropagatesPutError(t *testing.T) {
	rec := &recordingSink{putErr: errors.New("db down")}
	br := newConnectorSink(rec)
	if _, err := br.Put(context.Background(), connector.Document{ExternalID: "x", Text: "y"}); err == nil {
		t.Fatal("expected the sink error to propagate")
	}
}

// TestBridgeCompleteForwards: Complete must reach the sink so a full sync's
// soft-delete-of-unseen runs (SPEC-05 §5).
func TestBridgeCompleteForwards(t *testing.T) {
	rec := &recordingSink{}
	br := newConnectorSink(rec)
	if err := br.Complete(context.Background()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !rec.completed {
		t.Error("Complete did not forward to the sink")
	}
}
