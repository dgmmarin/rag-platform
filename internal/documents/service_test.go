package documents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// --- Test doubles. A *tenant.DB is unforgeable by design (ADR-0003), so the fake
// resolver hands back a nil handle and the fake store ignores it: this exercises
// the service's control flow (validation, open-error mapping, delegation,
// pagination) without a database. The real SQL is covered by the e2e suite. ---

type fakeResolver struct {
	openErr error
}

func (f fakeResolver) Open(_ context.Context, _ tenant.ID) (*tenant.DB, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return nil, nil // handle unused by fakeStore
}
func (f fakeResolver) Close(_ tenant.ID) {}

type fakeStore struct {
	docs       []Document
	chunks     []Chunk
	getDetail  DocumentDetail
	getErr     error
	deleteExst bool
	deleteErr  error
	gotContent bool
}

func (f *fakeStore) List(_ context.Context, _ *tenant.DB, _ ListFilter, limit int, _ *Cursor) ([]Document, error) {
	if len(f.docs) > limit {
		return f.docs[:limit], nil
	}
	return f.docs, nil
}
func (f *fakeStore) Get(_ context.Context, _ *tenant.DB, _ string, withContent bool) (DocumentDetail, error) {
	f.gotContent = withContent
	return f.getDetail, f.getErr
}
func (f *fakeStore) Chunks(_ context.Context, _ *tenant.DB, _ string, limit int, _ *ChunkCursor) ([]Chunk, error) {
	if len(f.chunks) > limit {
		return f.chunks[:limit], nil
	}
	return f.chunks, nil
}
func (f *fakeStore) SoftDelete(_ context.Context, _ *tenant.DB, _ string) (bool, error) {
	return f.deleteExst, f.deleteErr
}
func (f *fakeStore) Put(_ context.Context, _ *tenant.DB, _ PutInput) (PutResult, error) {
	return PutResult{}, nil
}

type fakeJobs struct {
	enqueued  []NewIngestJob
	active    Job
	activeHit bool
	enqErr    error
}

func (f *fakeJobs) EnqueueIngest(_ context.Context, nj NewIngestJob) (Job, error) {
	if f.enqErr != nil {
		return Job{}, f.enqErr
	}
	f.enqueued = append(f.enqueued, nj)
	return Job{ID: "job-1", TenantID: nj.TenantID, Kind: "ingest_document", Status: "queued", Payload: nj.Payload}, nil
}
func (f *fakeJobs) FindActiveIngest(_ context.Context, _, _ string) (Job, bool, error) {
	return f.active, f.activeHit, nil
}

type fakeStorage struct {
	puts    int
	lastKey string
	putErr  error
}

func (f *fakeStorage) Put(_ context.Context, key string, _ string, r io.Reader) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.puts++
	f.lastKey = key
	_, _ = io.Copy(io.Discard, r)
	return nil
}

const tid = "22222222-2222-2222-2222-222222222222"

func newTID(t *testing.T) tenant.ID {
	id, err := parseTID(tid)
	if err != nil {
		t.Fatalf("tid: %v", err)
	}
	return id
}

func parseTID(s string) (tenant.ID, error) {
	u, err := uuid.Parse(s)
	return tenant.ID(u), err
}

func TestListValidatesFilters(t *testing.T) {
	svc := NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{})
	if _, err := svc.List(context.Background(), newTID(t), ListFilter{Status: "weird"}, 0, ""); !isValidation(err) {
		t.Fatalf("bad status err = %v, want validation", err)
	}
	if _, err := svc.List(context.Background(), newTID(t), ListFilter{SourceID: "not-uuid"}, 0, ""); !isValidation(err) {
		t.Fatalf("bad source err = %v, want validation", err)
	}
	if _, err := svc.List(context.Background(), newTID(t), ListFilter{}, 0, "bad-cursor!!"); !isValidation(err) {
		t.Fatalf("bad cursor err = %v, want validation", err)
	}
}

func TestListPaginatesAndMapsTenantUnavailable(t *testing.T) {
	docs := []Document{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	svc := NewService(fakeResolver{}, &fakeStore{docs: docs}, &fakeJobs{})
	page, err := svc.List(context.Background(), newTID(t), ListFilter{}, 2, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("want 2 items + next_cursor, got %d items cursor=%q", len(page.Items), page.NextCursor)
	}

	down := NewService(fakeResolver{openErr: tenant.ErrTenantUnavailable}, &fakeStore{}, &fakeJobs{})
	if _, err := down.List(context.Background(), newTID(t), ListFilter{}, 0, ""); !errors.Is(err, ErrTenantUnavailable) {
		t.Fatalf("unavailable tenant err = %v, want ErrTenantUnavailable", err)
	}
}

func TestGetPassesContentFlag(t *testing.T) {
	fs := &fakeStore{getDetail: DocumentDetail{Document: Document{ID: "d1"}}}
	svc := NewService(fakeResolver{}, fs, &fakeJobs{})
	if _, err := svc.Get(context.Background(), newTID(t), "d1", true); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !fs.gotContent {
		t.Fatal("withContent not forwarded to store")
	}
}

func TestDeleteNotFoundAndReadOnly(t *testing.T) {
	nf := NewService(fakeResolver{}, &fakeStore{deleteExst: false}, &fakeJobs{})
	if err := nf.Delete(context.Background(), newTID(t), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v, want ErrNotFound", err)
	}
	ro := NewService(fakeResolver{}, &fakeStore{deleteErr: tenant.ErrReadOnly}, &fakeJobs{})
	if err := ro.Delete(context.Background(), newTID(t), "x"); !errors.Is(err, ErrTenantUnavailable) {
		t.Fatalf("delete on suspended = %v, want ErrTenantUnavailable", err)
	}
	ok := NewService(fakeResolver{}, &fakeStore{deleteExst: true}, &fakeJobs{})
	if err := ok.Delete(context.Background(), newTID(t), "x"); err != nil {
		t.Fatalf("delete ok = %v", err)
	}
}

func TestIngestNilStorageIsSeam(t *testing.T) {
	svc := NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{}) // Storage nil
	_, err := svc.Ingest(context.Background(), IngestParams{TenantID: tid, Filename: "a.pdf", ContentType: "application/pdf", Reader: strings.NewReader("x")})
	if !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("nil storage err = %v, want ErrStorageUnavailable", err)
	}
}

func TestIngestRequiresFile(t *testing.T) {
	svc := NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{})
	svc.Storage = &fakeStorage{}
	if _, err := svc.Ingest(context.Background(), IngestParams{TenantID: tid}); !isValidation(err) {
		t.Fatalf("missing filename err = %v, want validation", err)
	}
}

func TestIngestStoresAndEnqueues(t *testing.T) {
	st := &fakeStorage{}
	jobs := &fakeJobs{}
	svc := NewService(fakeResolver{}, &fakeStore{}, jobs)
	svc.Storage = st
	src := "33333333-3333-3333-3333-333333333333"
	job, err := svc.Ingest(context.Background(), IngestParams{
		TenantID: tid, SourceID: &src, Filename: "notes.md", ContentType: "text/markdown",
		Size: 5, Reader: strings.NewReader("hello"), IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if st.puts != 1 || !strings.Contains(st.lastKey, tid) {
		t.Fatalf("storage put=%d key=%q", st.puts, st.lastKey)
	}
	if job.Kind != "ingest_document" || job.Status != "queued" {
		t.Fatalf("job = %+v", job)
	}
	if len(jobs.enqueued) != 1 {
		t.Fatalf("enqueued %d jobs, want 1", len(jobs.enqueued))
	}
	var payload map[string]any
	if err := json.Unmarshal(jobs.enqueued[0].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for _, k := range []string{"external_id", "object_key", "mime_type", "idempotency_key", "source_id"} {
		if _, ok := payload[k]; !ok {
			t.Fatalf("payload missing %q: %v", k, payload)
		}
	}
	if payload["external_id"] != "notes.md" {
		t.Fatalf("external_id = %v, want notes.md", payload["external_id"])
	}
}

func TestIngestIdempotentReplay(t *testing.T) {
	st := &fakeStorage{}
	jobs := &fakeJobs{active: Job{ID: "existing"}, activeHit: true}
	svc := NewService(fakeResolver{}, &fakeStore{}, jobs)
	svc.Storage = st
	job, err := svc.Ingest(context.Background(), IngestParams{TenantID: tid, Filename: "a.pdf", ContentType: "application/pdf", Reader: strings.NewReader("x"), IdempotencyKey: "dupe"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if job.ID != "existing" {
		t.Fatalf("replay returned %q, want existing", job.ID)
	}
	if st.puts != 0 || len(jobs.enqueued) != 0 {
		t.Fatal("idempotent replay must not store or enqueue again")
	}
}

func isValidation(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}
