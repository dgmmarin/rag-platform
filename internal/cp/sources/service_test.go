package sources

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeStore is an in-memory Store for the service tests. It mimics the
// control-plane sources/jobs semantics the PoolDB enforces in SQL (unique name,
// one active sync per source), without a database.
type fakeStore struct {
	seq     int
	src     map[string]Source // id -> source
	jobs    []Job
	failOn  string // method name to force an error on
	nowFunc func() time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{src: map[string]Source{}, nowFunc: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }}
}

func (f *fakeStore) List(_ context.Context, tenantID string, limit int, cur *Cursor) ([]Source, error) {
	if f.failOn == "List" {
		return nil, errors.New("boom")
	}
	var out []Source
	for _, s := range f.src {
		if s.TenantID == tenantID {
			out = append(out, s)
		}
	}
	// newest first by created_at, id.
	sortSourcesDesc(out)
	if cur != nil {
		var filtered []Source
		for _, s := range out {
			if s.CreatedAt.Before(cur.CreatedAt) || (s.CreatedAt.Equal(cur.CreatedAt) && s.ID < cur.ID) {
				filtered = append(filtered, s)
			}
		}
		out = filtered
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) Get(_ context.Context, tenantID, id string) (Source, error) {
	if f.failOn == "Get" {
		return Source{}, errors.New("boom")
	}
	s, ok := f.src[id]
	if !ok || s.TenantID != tenantID {
		return Source{}, ErrNotFound
	}
	return s, nil
}

func (f *fakeStore) Create(_ context.Context, p CreateParams) (Source, error) {
	if f.failOn == "Create" {
		return Source{}, errors.New("boom")
	}
	for _, s := range f.src {
		if s.TenantID == p.TenantID && s.Name == p.Name {
			return Source{}, ErrDuplicateName
		}
	}
	f.seq++
	now := f.nowFunc().Add(time.Duration(f.seq) * time.Second)
	cfg := p.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	s := Source{
		ID:           newID(f.seq),
		TenantID:     p.TenantID,
		Kind:         p.Kind,
		Name:         p.Name,
		Status:       "active",
		Config:       cfg,
		ScheduleCron: p.ScheduleCron,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	f.src[s.ID] = s
	return s, nil
}

func (f *fakeStore) Update(_ context.Context, tenantID, id string, patch UpdatePatch) (Source, error) {
	if f.failOn == "Update" {
		return Source{}, errors.New("boom")
	}
	s, ok := f.src[id]
	if !ok || s.TenantID != tenantID {
		return Source{}, ErrNotFound
	}
	if patch.Name != nil {
		for _, o := range f.src {
			if o.TenantID == tenantID && o.Name == *patch.Name && o.ID != id {
				return Source{}, ErrDuplicateName
			}
		}
		s.Name = *patch.Name
	}
	if patch.Config != nil {
		s.Config = *patch.Config
	}
	if patch.Status != nil {
		s.Status = *patch.Status
	}
	if patch.ClearSchedule {
		s.ScheduleCron = nil
	} else if patch.ScheduleCron != nil {
		s.ScheduleCron = patch.ScheduleCron
	}
	s.UpdatedAt = f.nowFunc()
	f.src[id] = s
	return s, nil
}

func (f *fakeStore) MarkDeleting(_ context.Context, tenantID, id string) (bool, bool, error) {
	if f.failOn == "MarkDeleting" {
		return false, false, errors.New("boom")
	}
	s, ok := f.src[id]
	if !ok || s.TenantID != tenantID {
		return false, false, nil
	}
	if s.Status == "deleting" {
		return false, true, nil
	}
	s.Status = "deleting"
	f.src[id] = s
	return true, true, nil
}

func (f *fakeStore) EnqueueJob(_ context.Context, nj NewJob) (Job, error) {
	if f.failOn == "EnqueueJob" {
		return Job{}, errors.New("boom")
	}
	// Emulate the partial unique index: one active sync per source.
	if nj.Kind == "sync_source" {
		for _, j := range f.jobs {
			if j.SourceID != nil && *j.SourceID == nj.SourceID && j.Kind == "sync_source" &&
				(j.Status == "queued" || j.Status == "running") {
				return Job{}, ErrActiveSyncExists
			}
		}
	}
	f.seq++
	sid := nj.SourceID
	j := Job{
		ID:       newID(f.seq),
		TenantID: nj.TenantID,
		SourceID: &sid,
		Kind:     nj.Kind,
		Status:   "queued",
		Payload:  nj.Payload,
		QueuedAt: f.nowFunc(),
	}
	f.jobs = append(f.jobs, j)
	return j, nil
}

func (f *fakeStore) FindActiveSync(_ context.Context, tenantID, sourceID, key string) (Job, bool, error) {
	if f.failOn == "FindActiveSync" {
		return Job{}, false, errors.New("boom")
	}
	if key == "" {
		return Job{}, false, nil
	}
	for _, j := range f.jobs {
		if j.TenantID == tenantID && j.SourceID != nil && *j.SourceID == sourceID &&
			j.Kind == "sync_source" && (j.Status == "queued" || j.Status == "running") {
			var p struct {
				Key string `json:"idempotency_key"`
			}
			_ = json.Unmarshal(j.Payload, &p)
			if p.Key == key {
				return j, true, nil
			}
		}
	}
	return Job{}, false, nil
}

func newID(n int) string {
	return "00000000-0000-0000-0000-" + padID(n)
}

func padID(n int) string {
	s := ""
	for i := 0; i < 12; i++ {
		s = string(rune('0'+(n%10))) + s
		n /= 10
	}
	return s
}

func sortSourcesDesc(s []Source) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[j].CreatedAt.After(s[i].CreatedAt) || (s[j].CreatedAt.Equal(s[i].CreatedAt) && s[j].ID > s[i].ID) {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

const tenantA = "11111111-1111-1111-1111-111111111111"

func newTestService(t *testing.T, st *fakeStore) *Service {
	t.Helper()
	svc := NewService(st)
	svc.now = st.nowFunc
	return svc
}

func TestCreateRejectsUnknownKind(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	_, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "bogus", Name: "n"})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
}

func TestCreateRejectsEmptyName(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	_, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "  "})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for blank name, got %v", err)
	}
}

func TestCreateRejectsNonObjectConfig(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	_, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n", Config: json.RawMessage(`[1,2]`)})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for array config, got %v", err)
	}
}

func TestCreateRequiresTenant(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	_, err := svc.Create(context.Background(), CreateParams{Kind: "web_crawl", Name: "n"})
	if err == nil {
		t.Fatal("want error when tenant is empty (fail closed)")
	}
}

func TestCreateInvokesConnectorValidation(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	sentinel := errors.New("bad config for kind")
	svc.Validator = fakeValidator{validate: func(_ string, _ json.RawMessage) error { return sentinel }}
	_, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError wrapping connector validation, got %v", err)
	}
}

func TestCreateSucceedsWithoutValidator(t *testing.T) {
	// With no connector framework wired (EPIC-06 seam), generic validation still
	// lets a well-formed source be created; kind-specific validation is skipped.
	st := newFakeStore()
	svc := newTestService(t, st)
	s, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "docs"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.Status != "active" || s.Kind != "web_crawl" {
		t.Fatalf("unexpected source: %+v", s)
	}
	if string(s.Config) != `{}` {
		t.Fatalf("config default: got %s", s.Config)
	}
}

func TestCreateDuplicateName(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	if _, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "dup"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "dup"})
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("want ErrDuplicateName, got %v", err)
	}
}

func TestUpdateStatusPauseResume(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	s, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})

	paused := "paused"
	got, err := svc.Update(context.Background(), UpdateParams{TenantID: tenantA, ID: s.ID, Patch: UpdatePatch{Status: &paused}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "paused" {
		t.Fatalf("want paused, got %s", got.Status)
	}
}

func TestUpdateRejectsBadStatus(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	s, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	bad := "deleting"
	_, err := svc.Update(context.Background(), UpdateParams{TenantID: tenantA, ID: s.ID, Patch: UpdatePatch{Status: &bad}})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for status=deleting via API, got %v", err)
	}
}

func TestUpdateNotFound(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	name := "x"
	_, err := svc.Update(context.Background(), UpdateParams{TenantID: tenantA, ID: newID(99), Patch: UpdatePatch{Name: &name}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteMarksDeletingAndEnqueues(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	s, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})

	job, err := svc.Delete(context.Background(), tenantA, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != "delete_source" || job.Status != "queued" {
		t.Fatalf("unexpected delete job: %+v", job)
	}
	if st.src[s.ID].Status != "deleting" {
		t.Fatalf("source not marked deleting: %s", st.src[s.ID].Status)
	}
}

func TestDeleteNotFound(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	_, err := svc.Delete(context.Background(), tenantA, newID(42))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteIdempotentWhenAlreadyDeleting(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	s, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	if _, err := svc.Delete(context.Background(), tenantA, s.ID); err != nil {
		t.Fatal(err)
	}
	// Second delete must not error and must not enqueue a second job.
	if _, err := svc.Delete(context.Background(), tenantA, s.ID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	n := 0
	for _, j := range st.jobs {
		if j.Kind == "delete_source" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 delete_source job, got %d", n)
	}
}

func TestSyncEnqueuesJob(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	s, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	job, err := svc.Sync(context.Background(), SyncParams{TenantID: tenantA, SourceID: s.ID, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != "sync_source" || job.Status != "queued" {
		t.Fatalf("unexpected sync job: %+v", job)
	}
	var p struct {
		Full bool `json:"full"`
	}
	_ = json.Unmarshal(job.Payload, &p)
	if !p.Full {
		t.Fatalf("payload full flag not set: %s", job.Payload)
	}
}

func TestSyncConflictWhenActive(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	s, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	if _, err := svc.Sync(context.Background(), SyncParams{TenantID: tenantA, SourceID: s.ID}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Sync(context.Background(), SyncParams{TenantID: tenantA, SourceID: s.ID})
	if !errors.Is(err, ErrActiveSyncExists) {
		t.Fatalf("want ErrActiveSyncExists, got %v", err)
	}
}

func TestSyncIdempotentReplay(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	s, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	first, err := svc.Sync(context.Background(), SyncParams{TenantID: tenantA, SourceID: s.ID, IdempotencyKey: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	// Same key while active must replay the same job, not 409.
	again, err := svc.Sync(context.Background(), SyncParams{TenantID: tenantA, SourceID: s.ID, IdempotencyKey: "abc"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("replay returned a different job: %s vs %s", again.ID, first.ID)
	}
}

func TestSyncNotFound(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	_, err := svc.Sync(context.Background(), SyncParams{TenantID: tenantA, SourceID: newID(7)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestTestConnectionUnavailableWithoutValidator(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	s, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	err := svc.Test(context.Background(), tenantA, s.ID)
	if !errors.Is(err, ErrConnectorUnavailable) {
		t.Fatalf("want ErrConnectorUnavailable, got %v", err)
	}
}

func TestTestConnectionRunsValidator(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	s, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "n"})
	called := false
	svc.Validator = fakeValidator{test: func(_ context.Context, _ string, _ json.RawMessage) error {
		called = true
		return nil
	}}
	if err := svc.Test(context.Background(), tenantA, s.ID); err != nil {
		t.Fatalf("test: %v", err)
	}
	if !called {
		t.Fatal("validator.Test not called")
	}
}

func TestTestConnectionNotFound(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	svc.Validator = fakeValidator{}
	err := svc.Test(context.Background(), tenantA, newID(3))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListPaginates(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	for i := 0; i < 3; i++ {
		if _, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "web_crawl", Name: "s" + padID(i)}); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := svc.List(context.Background(), ListParams{TenantID: tenantA, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Items) != 2 || page1.NextCursor == "" {
		t.Fatalf("page1: items=%d next=%q", len(page1.Items), page1.NextCursor)
	}
	page2, err := svc.List(context.Background(), ListParams{TenantID: tenantA, Limit: 2, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Items) != 1 || page2.NextCursor != "" {
		t.Fatalf("page2: items=%d next=%q", len(page2.Items), page2.NextCursor)
	}
	// No overlap between pages.
	if page1.Items[0].ID == page2.Items[0].ID || page1.Items[1].ID == page2.Items[0].ID {
		t.Fatal("pages overlap")
	}
}

func TestListRejectsBadCursor(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	_, err := svc.List(context.Background(), ListParams{TenantID: tenantA, Cursor: "!!!not-base64!!!"})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for bad cursor, got %v", err)
	}
}

// fakeValidator is a stub connector-framework hook for the service tests.
type fakeValidator struct {
	validate func(kind string, cfg json.RawMessage) error
	test     func(ctx context.Context, kind string, cfg json.RawMessage) error
}

func (f fakeValidator) ValidateConfig(kind string, cfg json.RawMessage) error {
	if f.validate == nil {
		return nil
	}
	return f.validate(kind, cfg)
}

func (f fakeValidator) Test(ctx context.Context, kind string, cfg json.RawMessage) error {
	if f.test == nil {
		return nil
	}
	return f.test(ctx, kind, cfg)
}
