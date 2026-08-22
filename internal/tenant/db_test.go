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
