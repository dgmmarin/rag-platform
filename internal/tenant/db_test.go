package tenant

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// A read-only DB (suspended tenant) must refuse writes before touching the pool,
// so we can assert the guard without a live connection (pool is nil here).
func TestReadOnlyDBRefusesWrites(t *testing.T) {
	id := ID(uuid.New())
	db := &DB{id: id, status: StatusSuspended, readOnly: true}

	if _, err := db.Exec(context.Background(), "update x set y = 1"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Exec on read-only DB: want ErrReadOnly, got %v", err)
	}
	if _, err := db.Begin(context.Background()); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Begin on read-only DB: want ErrReadOnly, got %v", err)
	}
}

// BeginRead is the read-path counterpart of Begin: unlike Begin it must NOT
// refuse a read-only (suspended) tenant, because a read is always safe and
// retrieval (SPEC-06 §2) needs a transaction to scope `set local hnsw.ef_search`.
// With a nil pool it therefore reaches the pool and panics, proving it passed the
// guard rather than short-circuiting with ErrReadOnly the way Begin does.
func TestBeginReadDoesNotRefuseReadOnlyTenant(t *testing.T) {
	db := &DB{id: ID(uuid.New()), status: StatusSuspended, readOnly: true} // nil pool
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("BeginRead returned without reaching the pool; it must not guard on readOnly")
		}
	}()
	_, _ = db.BeginRead(context.Background())
}

func TestDBIDAndStatus(t *testing.T) {
	id := ID(uuid.New())
	db := &DB{id: id, status: StatusActive}
	if db.ID() != id {
		t.Fatalf("ID() = %s, want %s", db.ID(), id)
	}
	if db.Status() != StatusActive {
		t.Fatalf("Status() = %s, want %s", db.Status(), StatusActive)
	}
	if db.ReadOnly() {
		t.Fatalf("ReadOnly() = true, want false for active tenant")
	}
}
