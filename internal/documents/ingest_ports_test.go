package documents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeUploadSource struct {
	id     string
	err    error
	calls  int
	lastTn string
}

func (f *fakeUploadSource) Resolve(_ context.Context, tenantID string) (string, error) {
	f.calls++
	f.lastTn = tenantID
	return f.id, f.err
}

type fakeLimits struct {
	mb  int64
	err error
}

func (f fakeLimits) MaxUploadBytes(_ context.Context, _ string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.mb, nil
}

func TestIngestResolvesImplicitUploadSource(t *testing.T) {
	st := &fakeStorage{}
	jobs := &fakeJobs{}
	up := &fakeUploadSource{id: "44444444-4444-4444-4444-444444444444"}
	svc := NewService(fakeResolver{}, &fakeStore{}, jobs)
	svc.Storage = st
	svc.UploadSource = up

	// No explicit source: the service must resolve the tenant's implicit upload source.
	_, err := svc.Ingest(context.Background(), IngestParams{
		TenantID: tid, Filename: "notes.md", ContentType: "text/markdown",
		Size: 5, Reader: strings.NewReader("hello"),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if up.calls != 1 || up.lastTn != tid {
		t.Fatalf("resolve calls=%d tenant=%q", up.calls, up.lastTn)
	}
	if len(jobs.enqueued) != 1 || jobs.enqueued[0].SourceID == nil || *jobs.enqueued[0].SourceID != up.id {
		t.Fatalf("enqueued source id = %v, want %q", jobs.enqueued, up.id)
	}
	var payload map[string]any
	_ = json.Unmarshal(jobs.enqueued[0].Payload, &payload)
	if payload["source_id"] != up.id {
		t.Fatalf("payload source_id = %v, want %q", payload["source_id"], up.id)
	}
}

func TestIngestWithoutSourceOrResolverFails(t *testing.T) {
	svc := NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{})
	svc.Storage = &fakeStorage{}
	// No explicit source and no UploadSource wired: fail closed rather than enqueue
	// a job with a null source_id.
	_, err := svc.Ingest(context.Background(), IngestParams{
		TenantID: tid, Filename: "notes.md", ContentType: "text/markdown", Reader: strings.NewReader("x"),
	})
	if err == nil {
		t.Fatal("expected an error when no source and no resolver are available")
	}
}

func TestMaxBytesForTenantFromSettings(t *testing.T) {
	svc := NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{})
	svc.MaxBytes = 50 << 20
	svc.Limits = fakeLimits{mb: 10 << 20}
	if got := svc.MaxBytesForTenant(context.Background(), tid); got != 10<<20 {
		t.Fatalf("MaxBytesForTenant = %d, want %d (from settings)", got, 10<<20)
	}
}

func TestMaxBytesForTenantFallsBackToConfig(t *testing.T) {
	svc := NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{})
	svc.MaxBytes = 7 << 20
	// No Limits port: fall back to the configured ceiling.
	if got := svc.MaxBytesForTenant(context.Background(), tid); got != 7<<20 {
		t.Fatalf("MaxBytesForTenant (nil limits) = %d, want %d", got, 7<<20)
	}
	// A settings read error also falls back (fail-safe, never unbounded).
	svc.Limits = fakeLimits{err: errors.New("db down")}
	if got := svc.MaxBytesForTenant(context.Background(), tid); got != 7<<20 {
		t.Fatalf("MaxBytesForTenant (limits error) = %d, want %d", got, 7<<20)
	}
}
