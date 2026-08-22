package tenant

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// spyRegistry records Invalidate calls so we can assert Resolver.Close drops the
// cached record (tenant move/delete path, SPEC-01 §4).
type spyRegistry struct {
	rec         Record
	invalidated []ID
	closed      bool
}

func (s *spyRegistry) Lookup(context.Context, ID) (Record, error) { return s.rec, nil }
func (s *spyRegistry) Invalidate(id ID)                           { s.invalidated = append(s.invalidated, id) }
func (s *spyRegistry) Close()                                     { s.closed = true }

func TestResolverCloseEvictsPoolAndInvalidatesRegistry(t *testing.T) {
	id := ID(uuid.New())
	spy := &spyRegistry{rec: activeRecord(id, StatusActive)}
	var builds int
	r := newResolver(spy, expectedSchemaVersion, func(context.Context, Record) (*pgxpool.Pool, error) {
		builds++
		return nil, nil
	})

	if _, err := r.Open(context.Background(), id); err != nil {
		t.Fatalf("open: %v", err)
	}
	r.Close(id)

	// Registry invalidated for this tenant.
	if len(spy.invalidated) != 1 || spy.invalidated[0] != id {
		t.Fatalf("Invalidate calls = %v, want [%s]", spy.invalidated, id)
	}
	// Pool evicted: the next Open rebuilds.
	if _, err := r.Open(context.Background(), id); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if builds != 2 {
		t.Fatalf("builds = %d, want 2 (rebuild after Close)", builds)
	}
}

func TestResolverCloseAllEmptiesCache(t *testing.T) {
	spy := &spyRegistry{}
	r := newResolver(spy, expectedSchemaVersion, func(context.Context, Record) (*pgxpool.Pool, error) {
		return nil, nil
	})
	for i := 0; i < 3; i++ {
		id := ID(uuid.New())
		spy.rec = activeRecord(id, StatusActive)
		if _, err := r.Open(context.Background(), id); err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	if r.pools.len() != 3 {
		t.Fatalf("pool count = %d, want 3", r.pools.len())
	}
	r.closeAll()
	if r.pools.len() != 0 {
		t.Fatalf("pool count after closeAll = %d, want 0", r.pools.len())
	}
}

func TestRegistryCloseIsSafe(t *testing.T) {
	t.Helper()
	reg := newCachingRegistry(
		func(context.Context, ID) (Record, error) { return Record{}, nil },
		time.Second, nil)
	reg.Close() // must not panic; in-memory cache owns no resources
}
