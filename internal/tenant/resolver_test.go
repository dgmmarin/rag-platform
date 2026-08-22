package tenant

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeRegistry serves canned records so resolution rules can be tested without a
// control-plane database.
type fakeRegistry struct {
	rec Record
	err error
}

func (f fakeRegistry) Lookup(_ context.Context, _ ID) (Record, error) { return f.rec, f.err }
func (f fakeRegistry) Invalidate(ID)                                  {}
func (f fakeRegistry) Close()                                         {}

// newTestResolver builds a resolver whose pool builder never dials: it returns a
// nil pool, letting us assert the status→mode mapping without a live database.
func newTestResolver(reg Registry) *resolver {
	return newResolver(reg, expectedSchemaVersion, func(_ context.Context, _ Record) (*pgxpool.Pool, error) {
		return nil, nil
	})
}

func activeRecord(id ID, status Status) Record {
	return Record{ID: id, Status: status, SchemaVersion: expectedSchemaVersion, MaxConns: 5}
}

func TestOpenActiveIsReadWrite(t *testing.T) {
	id := ID(uuid.New())
	r := newTestResolver(fakeRegistry{rec: activeRecord(id, StatusActive)})

	db, err := r.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open active: %v", err)
	}
	if db.ReadOnly() {
		t.Fatalf("active tenant DB must be read-write")
	}
	if db.Status() != StatusActive {
		t.Fatalf("status = %s, want active", db.Status())
	}
}

func TestOpenSuspendedIsReadOnly(t *testing.T) {
	id := ID(uuid.New())
	r := newTestResolver(fakeRegistry{rec: activeRecord(id, StatusSuspended)})

	db, err := r.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open suspended: %v", err)
	}
	if !db.ReadOnly() {
		t.Fatalf("suspended tenant DB must be read-only")
	}
}

func TestOpenUnavailableStatuses(t *testing.T) {
	for _, st := range []Status{StatusProvisioning, StatusDeleting} {
		id := ID(uuid.New())
		r := newTestResolver(fakeRegistry{rec: activeRecord(id, st)})
		if _, err := r.Open(context.Background(), id); !errors.Is(err, ErrTenantUnavailable) {
			t.Fatalf("Open %s: want ErrTenantUnavailable, got %v", st, err)
		}
	}
}

func TestOpenDeletedIsNotFound(t *testing.T) {
	id := ID(uuid.New())
	r := newTestResolver(fakeRegistry{rec: activeRecord(id, StatusDeleted)})
	if _, err := r.Open(context.Background(), id); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("Open deleted: want ErrTenantNotFound, got %v", err)
	}
}

func TestOpenUnknownIsNotFound(t *testing.T) {
	id := ID(uuid.New())
	r := newTestResolver(fakeRegistry{err: ErrTenantNotFound})
	if _, err := r.Open(context.Background(), id); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("Open unknown: want ErrTenantNotFound, got %v", err)
	}
}

// Fail closed on schema drift (SPEC-01 §7): an out-of-date tenant must not serve.
func TestOpenSchemaOutdatedFailsClosed(t *testing.T) {
	id := ID(uuid.New())
	rec := activeRecord(id, StatusActive)
	rec.SchemaVersion = expectedSchemaVersion - 1
	r := newTestResolver(fakeRegistry{rec: rec})
	if _, err := r.Open(context.Background(), id); !errors.Is(err, ErrSchemaOutdated) {
		t.Fatalf("Open outdated schema: want ErrSchemaOutdated, got %v", err)
	}
}
