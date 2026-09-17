package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestReconcilerReturnsHealedRowsAndLogs(t *testing.T) {
	fs := newFakeStore()
	fs.reconciled = []Reconciled{{JobID: "11111111-1111-1111-1111-111111111111", RiverJobID: 84, Status: "cancelled"}}

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	r := Reconciler{Store: fs, Log: log}

	got, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got) != 1 || got[0] != fs.reconciled[0] {
		t.Fatalf("got %+v, want %+v unchanged", got, fs.reconciled)
	}

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("log line not JSON: %v (%q)", err, buf.String())
	}
	if line["event"] != "reconciled" {
		t.Fatalf("event = %v, want %q", line["event"], "reconciled")
	}
	if line["status"] != "cancelled" {
		t.Fatalf("status = %v, want %q", line["status"], "cancelled")
	}
	if !strings.Contains(buf.String(), "84") {
		t.Fatalf("log missing river_job_id: %q", buf.String())
	}
}

func TestReconcilerNilLogDoesNotPanic(t *testing.T) {
	fs := newFakeStore()
	fs.reconciled = []Reconciled{{JobID: "1", RiverJobID: 1, Status: "succeeded"}}
	r := Reconciler{Store: fs} // Log left nil

	got, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want 1 row", got)
	}
}

func TestReconcilerPropagatesStoreError(t *testing.T) {
	fs := newFakeStore()
	fs.failOn = "Reconcile"
	r := Reconciler{Store: fs}

	if _, err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile: want error when the store fails, got nil")
	}
}
