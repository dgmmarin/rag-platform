package upload

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rag-platform/ragctl/internal/connector"
)

func TestKind(t *testing.T) {
	if k := New().Kind(); k != connector.KindUpload {
		t.Fatalf("Kind() = %q, want %q", k, connector.KindUpload)
	}
}

func TestValidateConfigAcceptsEmptyObject(t *testing.T) {
	for _, cfg := range []string{`{}`, `{"note":"anything"}`} {
		if err := New().ValidateConfig(json.RawMessage(cfg)); err != nil {
			t.Fatalf("ValidateConfig(%s) = %v, want nil", cfg, err)
		}
	}
}

func TestValidateConfigRejectsMalformed(t *testing.T) {
	if err := New().ValidateConfig(json.RawMessage(`not json`)); err == nil {
		t.Fatal("ValidateConfig(malformed) = nil, want error")
	}
}

func TestTestIsNoop(t *testing.T) {
	// An upload source has no external system to reach; Test succeeds (FR-SRC-14).
	if err := New().Test(context.Background(), json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("Test() = %v, want nil", err)
	}
}

func TestSyncNotScheduled(t *testing.T) {
	// SPEC-04 §5: upload is "not scheduled" — documents arrive one ingest_document
	// job at a time, so a sync enumeration is never valid.
	_, err := New().Sync(context.Background(), connector.SyncRun{}, nil)
	if !errors.Is(err, ErrNotScheduled) {
		t.Fatalf("Sync() err = %v, want ErrNotScheduled", err)
	}
}

func TestRegisteredInDefaultRegistry(t *testing.T) {
	c, ok := connector.Lookup(connector.KindUpload)
	if !ok {
		t.Fatal("upload connector not registered in the default registry")
	}
	if c.Kind() != connector.KindUpload {
		t.Fatalf("registered connector Kind() = %q", c.Kind())
	}
}
