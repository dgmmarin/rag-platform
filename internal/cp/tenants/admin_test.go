package tenants

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/provision"
)

// --- Fakes for the AdminService collaborators (no real Postgres). ---

type fakeProvisioner struct {
	called bool
	params provision.Params
	res    provision.Result
	err    error
}

func (f *fakeProvisioner) Provision(_ context.Context, p provision.Params) (provision.Result, error) {
	f.called = true
	f.params = p
	if f.err != nil {
		return provision.Result{}, f.err
	}
	if f.res.TenantID == "" {
		f.res.TenantID = "t-new"
	}
	f.res.Slug = p.Slug
	return f.res, nil
}

type lifeCall struct {
	op    string
	slug  string
	grace time.Duration
	move  provision.MoveParams
}

type fakeLifecycle struct {
	calls []lifeCall
	err   error
}

func (f *fakeLifecycle) Suspend(_ context.Context, slug string) (provision.LifecycleResult, error) {
	f.calls = append(f.calls, lifeCall{op: "suspend", slug: slug})
	return provision.LifecycleResult{Slug: slug, ToStatus: "suspended"}, f.err
}
func (f *fakeLifecycle) Resume(_ context.Context, slug string) (provision.LifecycleResult, error) {
	f.calls = append(f.calls, lifeCall{op: "resume", slug: slug})
	return provision.LifecycleResult{Slug: slug, ToStatus: "active"}, f.err
}
func (f *fakeLifecycle) ScheduleDelete(_ context.Context, slug string, grace time.Duration) (provision.LifecycleResult, error) {
	f.calls = append(f.calls, lifeCall{op: "schedule_delete", slug: slug, grace: grace})
	return provision.LifecycleResult{Slug: slug, ToStatus: "deleting"}, f.err
}
func (f *fakeLifecycle) Move(_ context.Context, slug string, params provision.MoveParams) (provision.LifecycleResult, error) {
	f.calls = append(f.calls, lifeCall{op: "move", slug: slug, move: params})
	return provision.LifecycleResult{Slug: slug}, f.err
}

type fakeAdminStore struct {
	byID       map[string]Tenant
	list       []Tenant
	jobID      string
	recordedTo string
	getErr     error
}

func (f *fakeAdminStore) List(_ context.Context, limit int, _ *tenantCursor) ([]Tenant, error) {
	if limit < len(f.list) {
		return f.list[:limit], nil
	}
	return f.list, nil
}
func (f *fakeAdminStore) GetByID(_ context.Context, id string) (Tenant, error) {
	if f.getErr != nil {
		return Tenant{}, f.getErr
	}
	t, ok := f.byID[id]
	if !ok {
		return Tenant{}, ErrTenantNotFound
	}
	return t, nil
}
func (f *fakeAdminStore) RecordProvisionJob(_ context.Context, tenantID string) (string, error) {
	f.recordedTo = tenantID
	if f.jobID == "" {
		f.jobID = "job-prov"
	}
	return f.jobID, nil
}

func newService(store *fakeAdminStore, prov *fakeProvisioner, life *fakeLifecycle) *AdminService {
	return &AdminService{Store: store, Prov: prov, Life: life}
}

// --- Create ---

func TestAdminCreateProvisionsRecordsJobAndReturnsTenant(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{
		"t-new": {ID: "t-new", Slug: "acme", Name: "Acme", Status: "active", Region: "eu-central"},
	}}
	prov := &fakeProvisioner{res: provision.Result{TenantID: "t-new"}}
	svc := newService(store, prov, &fakeLifecycle{})

	got, err := svc.Create(context.Background(), CreateTenantParams{Slug: "acme", Name: "Acme", Region: "eu-central", EmbeddingDim: 768})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !prov.called {
		t.Fatal("provisioner was not called")
	}
	if prov.params.Slug != "acme" || prov.params.Name != "Acme" || prov.params.EmbeddingDim != 768 {
		t.Fatalf("provision params = %+v", prov.params)
	}
	if store.recordedTo != "t-new" {
		t.Fatalf("provision job recorded to %q, want t-new", store.recordedTo)
	}
	if got.JobID != "job-prov" {
		t.Fatalf("JobID = %q, want job-prov", got.JobID)
	}
	if got.Tenant.ID != "t-new" || got.Tenant.Status != "active" {
		t.Fatalf("tenant = %+v", got.Tenant)
	}
}

func TestAdminCreateRejectsMissingSlugOrName(t *testing.T) {
	svc := newService(&fakeAdminStore{}, &fakeProvisioner{}, &fakeLifecycle{})
	for _, p := range []CreateTenantParams{{Name: "n"}, {Slug: "s"}} {
		if _, err := svc.Create(context.Background(), p); err == nil {
			t.Fatalf("Create(%+v) = nil error, want validation error", p)
		} else {
			var ve *adminValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Create(%+v) err = %v, want adminValidationError", p, err)
			}
		}
	}
}

func TestAdminCreatePropagatesProvisionError(t *testing.T) {
	boom := errors.New("provision boom")
	svc := newService(&fakeAdminStore{}, &fakeProvisioner{err: boom}, &fakeLifecycle{})
	if _, err := svc.Create(context.Background(), CreateTenantParams{Slug: "s", Name: "n"}); !errors.Is(err, boom) {
		t.Fatalf("Create err = %v, want provision boom", err)
	}
}

// --- Patch ---

func TestAdminPatchStatusRoutesToLifecycle(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{"t1": {ID: "t1", Slug: "acme", Status: "active"}}}
	life := &fakeLifecycle{}
	svc := newService(store, &fakeProvisioner{}, life)

	suspended := "suspended"
	if _, err := svc.Patch(context.Background(), PatchTenantParams{ID: "t1", Status: &suspended}); err != nil {
		t.Fatalf("Patch suspend: %v", err)
	}
	if len(life.calls) != 1 || life.calls[0].op != "suspend" || life.calls[0].slug != "acme" {
		t.Fatalf("lifecycle calls = %+v, want one suspend on acme", life.calls)
	}

	life.calls = nil
	active := "active"
	if _, err := svc.Patch(context.Background(), PatchTenantParams{ID: "t1", Status: &active}); err != nil {
		t.Fatalf("Patch resume: %v", err)
	}
	if len(life.calls) != 1 || life.calls[0].op != "resume" {
		t.Fatalf("lifecycle calls = %+v, want one resume", life.calls)
	}
}

func TestAdminPatchConnectionRoutesToMove(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{"t1": {ID: "t1", Slug: "acme", Status: "active"}}}
	life := &fakeLifecycle{}
	svc := newService(store, &fakeProvisioner{}, life)

	host := "pg-2"
	port := 6543
	pw := "s3cret"
	_, err := svc.Patch(context.Background(), PatchTenantParams{ID: "t1", Connection: &ConnectionPatch{Host: &host, Port: &port, Password: &pw}})
	if err != nil {
		t.Fatalf("Patch move: %v", err)
	}
	if len(life.calls) != 1 || life.calls[0].op != "move" {
		t.Fatalf("lifecycle calls = %+v, want one move", life.calls)
	}
	if life.calls[0].move.Host != "pg-2" || life.calls[0].move.Port != 6543 || life.calls[0].move.Password != "s3cret" {
		t.Fatalf("move params = %+v", life.calls[0].move)
	}
}

func TestAdminPatchRejectsEmptyPatch(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{"t1": {ID: "t1", Slug: "acme"}}}
	svc := newService(store, &fakeProvisioner{}, &fakeLifecycle{})
	_, err := svc.Patch(context.Background(), PatchTenantParams{ID: "t1"})
	var ve *adminValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("empty patch err = %v, want adminValidationError", err)
	}
}

func TestAdminPatchRejectsUnknownStatus(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{"t1": {ID: "t1", Slug: "acme"}}}
	svc := newService(store, &fakeProvisioner{}, &fakeLifecycle{})
	bogus := "deleting"
	_, err := svc.Patch(context.Background(), PatchTenantParams{ID: "t1", Status: &bogus})
	var ve *adminValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("bad status err = %v, want adminValidationError", err)
	}
}

func TestAdminPatchUnknownTenantIsNotFound(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{}}
	svc := newService(store, &fakeProvisioner{}, &fakeLifecycle{})
	active := "active"
	if _, err := svc.Patch(context.Background(), PatchTenantParams{ID: "missing", Status: &active}); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("Patch missing tenant err = %v, want ErrTenantNotFound", err)
	}
}

// --- Delete ---

func TestAdminDeleteSchedulesWithGrace(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{"t1": {ID: "t1", Slug: "acme", Status: "active"}}}
	life := &fakeLifecycle{}
	svc := newService(store, &fakeProvisioner{}, life)

	grace := 48 * time.Hour
	if _, err := svc.Delete(context.Background(), "t1", grace); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(life.calls) != 1 || life.calls[0].op != "schedule_delete" || life.calls[0].slug != "acme" || life.calls[0].grace != grace {
		t.Fatalf("lifecycle calls = %+v, want schedule_delete acme 48h", life.calls)
	}
}

func TestAdminDeleteUnknownTenantIsNotFound(t *testing.T) {
	store := &fakeAdminStore{byID: map[string]Tenant{}}
	svc := newService(store, &fakeProvisioner{}, &fakeLifecycle{})
	if _, err := svc.Delete(context.Background(), "missing", 0); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("Delete missing err = %v, want ErrTenantNotFound", err)
	}
}

// --- List ---

func TestAdminListReturnsPageWithCursor(t *testing.T) {
	store := &fakeAdminStore{list: []Tenant{
		{ID: "a", CreatedAt: time.Now()},
		{ID: "b", CreatedAt: time.Now().Add(-time.Minute)},
		{ID: "c", CreatedAt: time.Now().Add(-2 * time.Minute)},
	}}
	svc := newService(store, &fakeProvisioner{}, &fakeLifecycle{})
	page, err := svc.List(context.Background(), ListTenantsParams{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2 (limit)", len(page.Items))
	}
	if page.NextCursor == "" {
		t.Fatal("want a next_cursor when a further page exists")
	}
}
