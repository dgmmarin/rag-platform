package querylog

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// --- Test doubles. A *tenant.DB is unforgeable by design (ADR-0003), so the fake
// resolver hands back a nil handle and the fake store ignores it: this exercises
// the async control flow and the QueryRecord→Record mapping without a database.
// The real SQL is covered by the e2e suite. ---

type fakeResolver struct {
	openErr error
	opened  int
	mu      sync.Mutex
}

func (f *fakeResolver) Open(_ context.Context, _ tenant.ID) (*tenant.DB, error) {
	f.mu.Lock()
	f.opened++
	f.mu.Unlock()
	if f.openErr != nil {
		return nil, f.openErr
	}
	return nil, nil // handle unused by fakeStore
}
func (f *fakeResolver) Close(_ tenant.ID) {}

type fakeStore struct {
	mu        sync.Mutex
	inserted  []Record
	insertErr error
	release   chan struct{} // when non-nil, Insert blocks until closed

	feedback    []Feedback
	feedbackErr error

	entries   []Entry
	listLimit int
	listCur   *Cursor
	listErr   error
}

func (f *fakeStore) Insert(_ context.Context, _ *tenant.DB, rec Record) error {
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(f.inserted, rec)
	return nil
}

func (f *fakeStore) UpsertFeedback(_ context.Context, _ *tenant.DB, fb Feedback) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.feedbackErr != nil {
		return f.feedbackErr
	}
	f.feedback = append(f.feedback, fb)
	return nil
}

func (f *fakeStore) List(_ context.Context, _ *tenant.DB, limit int, cur *Cursor) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listLimit = limit
	f.listCur = cur
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.entries) > limit {
		return f.entries[:limit], nil
	}
	return f.entries, nil
}

func (f *fakeStore) insertedRecords() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Record, len(f.inserted))
	copy(out, f.inserted)
	return out
}

func sampleRecord(tenantID string) answer.QueryRecord {
	return answer.QueryRecord{
		ID:                "q_" + uuid.NewString(),
		TenantID:          tenantID,
		Question:          "how do I reset the X200?",
		Grounded:          true,
		RetrievedChunkIDs: []string{"c1", "c2", "c3"},
		RetrievedScores:   []float64{0.9, 0.5, 0.1},
		CitationChunkIDs:  []string{"c1"},
		Usage:             answer.Usage{RetrievalMs: 120, GenerationMs: 1400, InTokens: 3200, OutTokens: 180},
		Model:             "claude-sonnet-5",
	}
}

func TestLoggerMapsQueryRecordToColumns(t *testing.T) {
	store := &fakeStore{}
	tid := uuid.NewString()
	l := &Logger{Resolver: &fakeResolver{}, Store: store, Slog: obs.Logger("test", 0, io.Discard)}

	rec := sampleRecord(tid)
	l.Log(obs.ContextWithRequestID(context.Background(), "req-42"), rec)
	l.Wait()

	got := store.insertedRecords()
	if len(got) != 1 {
		t.Fatalf("inserted %d records, want 1", len(got))
	}
	r := got[0]
	// The response id is q_<uuid>; the stored id is the bare uuid (query_log.id is uuid).
	if r.ID.String() != rec.ID[len("q_"):] {
		t.Fatalf("id = %s, want %s", r.ID, rec.ID[len("q_"):])
	}
	if r.Question != rec.Question || !r.Grounded {
		t.Fatalf("question/grounded mismapped: %+v", r)
	}
	if r.RequestID != "req-42" {
		t.Fatalf("request_id = %q, want req-42", r.RequestID)
	}
	if r.LLMModel != "claude-sonnet-5" {
		t.Fatalf("llm_model = %q", r.LLMModel)
	}
	if r.RetrievalMs != 120 || r.GenerationMs != 1400 || r.InTokens != 3200 || r.OutTokens != 180 {
		t.Fatalf("usage mismapped: %+v", r)
	}
	want := []RetrievedChunk{{ChunkID: "c1", Score: 0.9, Rank: 1}, {ChunkID: "c2", Score: 0.5, Rank: 2}, {ChunkID: "c3", Score: 0.1, Rank: 3}}
	if len(r.Retrieved) != 3 {
		t.Fatalf("retrieved = %+v, want %+v", r.Retrieved, want)
	}
	for i, rc := range want {
		if r.Retrieved[i] != rc {
			t.Fatalf("retrieved[%d] = %+v, want %+v", i, r.Retrieved[i], rc)
		}
	}
	if len(r.Citations) != 1 || r.Citations[0] != "c1" {
		t.Fatalf("citations = %+v, want [c1]", r.Citations)
	}
}

func TestLoggerLogDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	store := &fakeStore{release: release}
	l := &Logger{Resolver: &fakeResolver{}, Store: store, Slog: obs.Logger("test", 0, io.Discard)}

	done := make(chan struct{})
	go func() {
		l.Log(context.Background(), sampleRecord(uuid.NewString()))
		close(done)
	}()

	select {
	case <-done:
		// Log returned before the (blocked) store write completed — it is async.
	case <-time.After(2 * time.Second):
		t.Fatal("Log blocked on the store write; it must be asynchronous")
	}

	close(release) // let the background write finish
	l.Wait()
	if n := len(store.insertedRecords()); n != 1 {
		t.Fatalf("inserted %d, want 1 after release", n)
	}
}

func TestLoggerSwallowsWriteError(t *testing.T) {
	store := &fakeStore{insertErr: errors.New("boom")}
	l := &Logger{Resolver: &fakeResolver{}, Store: store, Slog: obs.Logger("test", 0, io.Discard)}
	// Must not panic and must not surface the error to the caller (no return value).
	l.Log(context.Background(), sampleRecord(uuid.NewString()))
	l.Wait()
	if n := len(store.insertedRecords()); n != 0 {
		t.Fatalf("inserted %d, want 0 on error", n)
	}
}

func TestLoggerSwallowsResolveError(t *testing.T) {
	res := &fakeResolver{openErr: tenant.ErrTenantUnavailable}
	store := &fakeStore{}
	l := &Logger{Resolver: res, Store: store, Slog: obs.Logger("test", 0, io.Discard)}
	l.Log(context.Background(), sampleRecord(uuid.NewString()))
	l.Wait()
	if n := len(store.insertedRecords()); n != 0 {
		t.Fatalf("inserted %d, want 0 when resolve fails", n)
	}
}

func TestLoggerSkipsUnparseableID(t *testing.T) {
	res := &fakeResolver{}
	store := &fakeStore{}
	l := &Logger{Resolver: res, Store: store, Slog: obs.Logger("test", 0, io.Discard)}
	rec := sampleRecord(uuid.NewString())
	rec.ID = "not-a-uuid"
	l.Log(context.Background(), rec)
	l.Wait()
	if res.opened != 0 {
		t.Fatalf("resolver opened %d times for an unparseable id; want 0 (no work spawned)", res.opened)
	}
}

func TestLoggerSkipsMissingTenant(t *testing.T) {
	res := &fakeResolver{}
	l := &Logger{Resolver: res, Store: &fakeStore{}, Slog: obs.Logger("test", 0, io.Discard)}
	rec := sampleRecord("")
	rec.TenantID = ""
	l.Log(context.Background(), rec)
	l.Wait()
	if res.opened != 0 {
		t.Fatalf("resolver opened %d times for an empty tenant; want 0", res.opened)
	}
}

// --- Service: feedback + admin list ---

func TestServiceFeedbackValidatesRating(t *testing.T) {
	svc := &Service{Resolver: &fakeResolver{}, Store: &fakeStore{}}
	qid := "q_" + uuid.NewString()
	for _, bad := range []int{0, 2, -2, 5} {
		err := svc.Feedback(context.Background(), tenant.ID(uuid.New()), qid, bad, "")
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("rating %d: err = %v, want ValidationError", bad, err)
		}
	}
}

func TestServiceFeedbackUpserts(t *testing.T) {
	store := &fakeStore{}
	svc := &Service{Resolver: &fakeResolver{}, Store: store}
	qid := "q_" + uuid.NewString()
	if err := svc.Feedback(context.Background(), tenant.ID(uuid.New()), qid, 1, "great"); err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	if len(store.feedback) != 1 {
		t.Fatalf("feedback writes = %d, want 1", len(store.feedback))
	}
	fb := store.feedback[0]
	if fb.QueryID.String() != qid[len("q_"):] || fb.Rating != 1 || fb.Comment != "great" {
		t.Fatalf("feedback = %+v", fb)
	}
}

func TestServiceFeedbackUnknownQuery(t *testing.T) {
	store := &fakeStore{feedbackErr: ErrQueryNotFound}
	svc := &Service{Resolver: &fakeResolver{}, Store: store}
	err := svc.Feedback(context.Background(), tenant.ID(uuid.New()), "q_"+uuid.NewString(), 1, "")
	if !errors.Is(err, ErrQueryNotFound) {
		t.Fatalf("err = %v, want ErrQueryNotFound", err)
	}
}

func TestServiceFeedbackBadID(t *testing.T) {
	svc := &Service{Resolver: &fakeResolver{}, Store: &fakeStore{}}
	err := svc.Feedback(context.Background(), tenant.ID(uuid.New()), "not-a-uuid", 1, "")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError for a bad query id", err)
	}
}

func TestServiceListPaginates(t *testing.T) {
	store := &fakeStore{}
	// three entries, limit 2 → one page of 2 + a next cursor
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		store.entries = append(store.entries, Entry{
			ID:        "q_" + uuid.NewString(),
			Question:  "q",
			CreatedAt: base.Add(-time.Duration(i) * time.Minute),
		})
	}
	// give the last-of-page entry a stable uuid id for cursor extraction
	svc := &Service{Resolver: &fakeResolver{}, Store: store}
	page, err := svc.List(context.Background(), tenant.ID(uuid.New()), 2, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if store.listLimit != 3 { // limit+1 fetch
		t.Fatalf("store fetched limit %d, want 3 (limit+1)", store.listLimit)
	}
	if len(page.Items) != 2 {
		t.Fatalf("page items = %d, want 2", len(page.Items))
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next_cursor when more rows exist")
	}
}

func TestServiceListInvalidCursor(t *testing.T) {
	svc := &Service{Resolver: &fakeResolver{}, Store: &fakeStore{}}
	_, err := svc.List(context.Background(), tenant.ID(uuid.New()), 50, "!!!not-base64!!!")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError for a bad cursor", err)
	}
}

func TestServiceListTenantUnavailable(t *testing.T) {
	svc := &Service{Resolver: &fakeResolver{openErr: tenant.ErrTenantUnavailable}, Store: &fakeStore{}}
	_, err := svc.List(context.Background(), tenant.ID(uuid.New()), 50, "")
	if !errors.Is(err, ErrTenantUnavailable) {
		t.Fatalf("err = %v, want ErrTenantUnavailable", err)
	}
}
